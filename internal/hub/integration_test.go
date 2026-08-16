package hub

// Heavy, network-touching integration test: provisions a REAL postgres via
// internal/svc (downloading the ~314 MB EDB archive on first run, same as
// cmd/hey/svc_integration_test.go's TestIntegrationPostgresE2E) and drives
// the full hub HTTP surface — /services, then the postgres bridge's
// query/exec, transaction session-pinning, and bytea round trip — against
// it. Run with: HEY_HUB_INTEGRATION=1 go test ./internal/hub -run
// TestIntegrationHubPostgresBridge -v

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kitsyai/hey/internal/svc"
)

func TestIntegrationHubPostgresBridge(t *testing.T) {
	if os.Getenv("HEY_HUB_INTEGRATION") != "1" {
		t.Skip("set HEY_HUB_INTEGRATION=1 to run the heavy hub+real-postgres integration test")
	}

	svcDir := t.TempDir()
	packs, err := svc.LoadPacks(t.TempDir()) // embedded default packs.json
	if err != nil {
		t.Fatal(err)
	}
	pgPack, err := packs.Get("postgres")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := svc.Up(svcDir, "postgres", pgPack, pgPack.LatestVersion(), nil)
	if err != nil {
		if strings.Contains(err.Error(), "no artifact for") {
			t.Skipf("postgres pack has no artifact for this platform: %v", err)
		}
		t.Fatalf("svc.Up postgres: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop(inst, pgPack) })

	h := NewHandler(svcDir, packs, testConfig(), DefaultPort, "0.1.0-dev")
	doRequest := func(method, path, sessionID string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		var reader *strings.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			reader = strings.NewReader(string(b))
		} else {
			reader = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, reader)
		req.Host = "127.0.0.1:8525"
		req.Header.Set(RequiredClientHeader, "1")
		req.Header.Set("Origin", "https://heypkv.ai")
		req.Header.Set("Content-Type", "application/json")
		if sessionID != "" {
			req.Header.Set("X-Hey-Session", sessionID)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		if rec.Body.Len() > 0 {
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
		}
		return rec, out
	}

	// /services shows the live instance, healthy.
	rec, _ := doRequest(http.MethodGet, "/services", "", nil)
	var cat Catalog
	_ = json.Unmarshal(rec.Body.Bytes(), &cat)
	if len(cat.Services) != 1 || cat.Services[0].Name != "postgres" || cat.Services[0].Health != "healthy" {
		t.Fatalf("/services = %+v", cat.Services)
	}

	// A trivial parameterized query.
	rec, out := doRequest(http.MethodPost, "/svc/postgres/query", "s1", map[string]any{
		"sql": "SELECT 1 AS one, $1::text AS greeting", "params": []any{"hello"},
		"namespace": "integration", "name": "demo",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("query status = %d body=%v", rec.Code, out)
	}
	rows, _ := out["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %v", out)
	}
	row := rows[0].(map[string]any)
	if row["greeting"] != "hello" || row["one"].(float64) != 1 {
		t.Errorf("row = %v", row)
	}

	// DDL through /exec (the client's migrate() needs this).
	rec, out = doRequest(http.MethodPost, "/svc/postgres/exec", "s1", map[string]any{
		"sql":       "CREATE TABLE IF NOT EXISTS t (id SERIAL PRIMARY KEY, name TEXT, data BYTEA)",
		"namespace": "integration", "name": "demo",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create table status = %d body=%v", rec.Code, out)
	}

	// Transaction across separate HTTP requests on the same session: BEGIN,
	// an INSERT ... RETURNING via /query (rows wanted), COMMIT — must be
	// visible from a DIFFERENT session afterward (proves the connection was
	// really pinned per-session, not one-shot per request).
	doRequest(http.MethodPost, "/svc/postgres/exec", "s1", map[string]any{"sql": "BEGIN", "namespace": "integration", "name": "demo"})
	rec, out = doRequest(http.MethodPost, "/svc/postgres/query", "s1", map[string]any{
		"sql": "INSERT INTO t (name, data) VALUES ($1, $2) RETURNING id", "namespace": "integration", "name": "demo",
		"params": []any{"committed-row", map[string]any{"$bytes": "aGVsbG8="}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("insert status = %d body=%v", rec.Code, out)
	}
	doRequest(http.MethodPost, "/svc/postgres/exec", "s1", map[string]any{"sql": "COMMIT", "namespace": "integration", "name": "demo"})

	rec, out = doRequest(http.MethodPost, "/svc/postgres/query", "s2-verify", map[string]any{
		"sql": "SELECT name, data FROM t WHERE name = $1", "params": []any{"committed-row"},
		"namespace": "integration", "name": "demo",
	})
	rows, _ = out["rows"].([]any)
	if rec.Code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("committed row not visible from a different session: status=%d body=%v", rec.Code, out)
	}
	committedRow := rows[0].(map[string]any)
	bytesMarker, ok := committedRow["data"].(map[string]any)
	if !ok || bytesMarker["$bytes"] != "aGVsbG8=" {
		t.Errorf("bytea did not round-trip as a $bytes marker: %v", committedRow["data"])
	}

	// Rollback on a fresh session: the row must NOT be visible afterward.
	doRequest(http.MethodPost, "/svc/postgres/exec", "s3", map[string]any{"sql": "BEGIN", "namespace": "integration", "name": "demo"})
	doRequest(http.MethodPost, "/svc/postgres/exec", "s3", map[string]any{
		"sql": "INSERT INTO t (name) VALUES ('rolled-back-row')", "namespace": "integration", "name": "demo",
	})
	doRequest(http.MethodPost, "/svc/postgres/exec", "s3", map[string]any{"sql": "ROLLBACK", "namespace": "integration", "name": "demo"})
	rec, out = doRequest(http.MethodPost, "/svc/postgres/query", "s2-verify", map[string]any{
		"sql": "SELECT count(*) AS n FROM t WHERE name = 'rolled-back-row'", "namespace": "integration", "name": "demo",
	})
	rows, _ = out["rows"].([]any)
	if rec.Code != http.StatusOK || len(rows) != 1 || rows[0].(map[string]any)["n"].(float64) != 0 {
		t.Fatalf("rollback did not roll back: %v", out)
	}

	// A dangerous statement is refused.
	rec, out = doRequest(http.MethodPost, "/svc/postgres/exec", "s3", map[string]any{
		"sql": "GRANT ALL ON t TO PUBLIC", "namespace": "integration", "name": "demo",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("GRANT should be refused (403), got %d: %v", rec.Code, out)
	}

	// Namespace isolation: a different (namespace, name) pair gets its own
	// schema and never sees "t" from the "integration/demo" pair.
	rec, out = doRequest(http.MethodPost, "/svc/postgres/query", "s-other-ns", map[string]any{
		"sql": "SELECT to_regclass('t') IS NOT NULL AS table_exists", "namespace": "a-different-namespace", "name": "demo",
	})
	rows, _ = out["rows"].([]any)
	if rec.Code != http.StatusOK || len(rows) != 1 || rows[0].(map[string]any)["table_exists"] != false {
		t.Errorf("a different namespace should not see the other namespace's table: %v", out)
	}
}
