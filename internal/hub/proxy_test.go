package hub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kitsyai/hey/internal/svc"
)

func TestSplitSvcPath(t *testing.T) {
	cases := []struct{ path, name, rest string }{
		{"/svc/vinci/v1/chat/completions", "vinci", "/v1/chat/completions"},
		{"/svc/vinci", "vinci", ""},
		{"/svc/postgres/query", "postgres", "/query"},
		{"/healthz", "", ""},
		{"/services", "", ""},
	}
	for _, c := range cases {
		name, rest := splitSvcPath(c.path)
		if name != c.name || rest != c.rest {
			t.Errorf("splitSvcPath(%q) = %q, %q; want %q, %q", c.path, name, rest, c.name, c.rest)
		}
	}
}

func TestProxyToInstance_StripsPrefixAndForwards(t *testing.T) {
	var gotPath, gotBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	svcDir := t.TempDir()
	inst := writeFakeInstance(t, svcDir, "vinci", "vinci", serverPort(t, backend), os.Getpid())

	req := httptest.NewRequest(http.MethodPost, "/svc/vinci/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`))
	rec := httptest.NewRecorder()
	proxyToInstance(inst).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("backend saw path %q, want the /svc/vinci prefix stripped", gotPath)
	}
	if gotBody != `{"model":"qwen"}` {
		t.Errorf("backend saw body %q", gotBody)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("proxied response body = %q", rec.Body.String())
	}
}

func TestProxyToInstance_NotRunning(t *testing.T) {
	svcDir := t.TempDir()
	inst := writeFakeInstance(t, svcDir, "vinci", "vinci", 51234, 0 /* not alive */)
	req := httptest.NewRequest(http.MethodGet, "/svc/vinci/v1/models", nil)
	rec := httptest.NewRecorder()
	proxyToInstance(inst).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for a stopped instance", rec.Code)
	}
}

func TestSvcDispatch_UnknownInstance404s(t *testing.T) {
	svcDir := t.TempDir()
	packs := &svc.PackSet{Packs: map[string]svc.Pack{"vinci": {Pack: "vinci", Protocol: "openai-http"}}}
	req := httptest.NewRequest(http.MethodGet, "/svc/never-provisioned/v1/models", nil)
	rec := httptest.NewRecorder()
	svcDispatch(svcDir, packs).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestSvcDispatch_RoutesBridgeVsProxyByPackKind(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	svcDir := t.TempDir()
	writeFakeInstance(t, svcDir, "vinci", "vinci", serverPort(t, backend), os.Getpid())
	writeFakeInstance(t, svcDir, "postgres", "postgres", 5, 0 /* not alive; bridge path just needs to be reached, not to actually connect */)

	packs := &svc.PackSet{Packs: map[string]svc.Pack{
		"vinci":    {Pack: "vinci", Protocol: "openai-http"},
		"postgres": {Pack: "postgres", Protocol: "postgres-wire", Bridge: &svc.Bridge{Path: "/svc/postgres", Kind: "http-query-api"}},
	}}

	// vinci: no Bridge -> reverse-proxied straight to the instance.
	req := httptest.NewRequest(http.MethodGet, "/svc/vinci/v1/models", nil)
	rec := httptest.NewRecorder()
	svcDispatch(svcDir, packs).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("vinci proxy status = %d, want 200 from the backend", rec.Code)
	}

	// postgres: Bridge set -> routed to the bridge handler, not proxied (a
	// browser can't speak raw postgres-wire, so any response here proves the
	// dispatcher picked the bridge path, not proxyToInstance).
	req2 := httptest.NewRequest(http.MethodPost, "/svc/postgres/query", strings.NewReader(`{"sql":"SELECT 1"}`))
	rec2 := httptest.NewRecorder()
	svcDispatch(svcDir, packs).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Errorf("postgres bridge status = %d, want 503 (not running) — proves it reached the bridge handler, not the reverse proxy", rec2.Code)
	}
}

func TestSvcDispatch_CustomNamedInstanceRoutesByItsOwnPack(t *testing.T) {
	// `hey up postgres --name pg2` — the instance is named "pg2" but its Pack
	// field still says "postgres"; /svc/pg2/... must resolve through that,
	// not fail because there's no pack literally named "pg2".
	svcDir := t.TempDir()
	writeFakeInstance(t, svcDir, "pg2", "postgres", 5, 0)
	packs := &svc.PackSet{Packs: map[string]svc.Pack{
		"postgres": {Pack: "postgres", Protocol: "postgres-wire", Bridge: &svc.Bridge{Path: "/svc/postgres", Kind: "http-query-api"}},
	}}
	req := httptest.NewRequest(http.MethodPost, "/svc/pg2/query", strings.NewReader(`{"sql":"SELECT 1"}`))
	rec := httptest.NewRecorder()
	svcDispatch(svcDir, packs).ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Errorf("a custom-named instance must route through its own pack's Bridge, not 404")
	}
}
