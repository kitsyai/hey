package hub

import (
	"strings"
	"testing"

	"github.com/kitsyai/hey/internal/pgwire"
)

func TestSchemaName_DeterministicAndDistinct(t *testing.T) {
	a1 := schemaName("https://example.com", "biz")
	a2 := schemaName("https://example.com", "biz")
	b := schemaName("https://example.com", "other-app")
	c := schemaName("https://other.example", "biz")
	if a1 != a2 {
		t.Error("schemaName must be deterministic for the same inputs")
	}
	if a1 == b || a1 == c {
		t.Error("distinct (namespace, name) pairs must get distinct schemas")
	}
	if !strings.HasPrefix(a1, "hey_") {
		t.Errorf("schema name %q should be hey_-prefixed", a1)
	}
}

func TestQuoteIdent_EscapesEmbeddedQuotes(t *testing.T) {
	got := quoteIdent(`weird"name`)
	want := `"weird""name"`
	if got != want {
		t.Errorf("quoteIdent = %q, want %q", got, want)
	}
}

func TestCheckStatementKind_BlocksDangerousOps(t *testing.T) {
	for _, sql := range []string{
		"GRANT ALL ON t TO PUBLIC", "DROP ROLE x", "CREATE ROLE x",
		"ALTER SYSTEM SET x = 1", "COPY t TO '/etc/passwd'", "DO $$ BEGIN END $$",
		"  grant select on t to public", // case/whitespace insensitive
	} {
		if err := checkStatementKind(sql); err == nil {
			t.Errorf("expected %q to be blocked", sql)
		}
	}
}

func TestCheckStatementKind_AllowsAppStatements(t *testing.T) {
	// The real client (heyDatabase.ts) sends all of these through /exec or
	// /query interchangeably — query()/exec() picks the response shape, not
	// the statement kind (see the package doc).
	for _, sql := range []string{
		"SELECT 1", "WITH x AS (SELECT 1) SELECT * FROM x",
		"BEGIN", "COMMIT", "ROLLBACK",
		"CREATE TABLE IF NOT EXISTS t (id SERIAL PRIMARY KEY)",
		"ALTER TABLE t ADD COLUMN name TEXT",
		"INSERT INTO t (name) VALUES ($1) RETURNING id",
		"UPDATE t SET name = $1 WHERE id = $2",
		"DELETE FROM t WHERE id = $1",
		"TRUNCATE t",
	} {
		if err := checkStatementKind(sql); err != nil {
			t.Errorf("expected %q to be allowed, got %v", sql, err)
		}
	}
}

func TestDecodeWireParams_BytesMarkerAndPassthrough(t *testing.T) {
	raw := []any{
		"plain string", float64(42), true, nil,
		map[string]any{"$bytes": "aGVsbG8="}, // "hello" base64
	}
	out, err := decodeWireParams(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out[0] != "plain string" || out[1] != float64(42) || out[2] != true || out[3] != nil {
		t.Errorf("passthrough values mismatched: %#v", out)
	}
	b, ok := out[4].(pgwire.Bytes)
	if !ok || string(b) != "hello" {
		t.Errorf("$bytes marker did not decode to pgwire.Bytes(\"hello\"): %#v", out[4])
	}
}

func TestDecodeWireParams_RejectsMalformedObject(t *testing.T) {
	_, err := decodeWireParams([]any{map[string]any{"not_bytes": "x"}})
	if err == nil {
		t.Error("expected an error for an object param that isn't a $bytes marker")
	}
	_, err = decodeWireParams([]any{map[string]any{"$bytes": "not-valid-base64!!!"}})
	if err == nil {
		t.Error("expected an error for invalid base64 in a $bytes marker")
	}
}

func TestEncodeWireValue_BytesGetMarker(t *testing.T) {
	got := encodeWireValue([]byte("hello"))
	m, ok := got.(map[string]string)
	if !ok || m["$bytes"] != "aGVsbG8=" {
		t.Errorf("encodeWireValue([]byte) = %#v", got)
	}
	if v := encodeWireValue("plain"); v != "plain" {
		t.Errorf("encodeWireValue(string) = %#v, want passthrough", v)
	}
	if v := encodeWireValue(nil); v != nil {
		t.Errorf("encodeWireValue(nil) = %#v", v)
	}
}

func TestEncodeWireRows_ZipsColumnsAndValues(t *testing.T) {
	res := &pgwire.Result{
		Columns: []string{"id", "name"},
		Rows:    [][]any{{int64(1), "a"}, {int64(2), "b"}},
	}
	rows := encodeWireRows(res)
	if len(rows) != 2 || rows[0]["id"] != int64(1) || rows[0]["name"] != "a" || rows[1]["name"] != "b" {
		t.Errorf("encodeWireRows = %#v", rows)
	}
}

func TestParseRowsAffected(t *testing.T) {
	cases := map[string]int{
		"INSERT 0 3": 3, "UPDATE 3": 3, "DELETE 2": 2,
		"BEGIN": 0, "COMMIT": 0, "ROLLBACK": 0, "SELECT 5": 5, "": 0,
	}
	for tag, want := range cases {
		if got := parseRowsAffected(tag); got != want {
			t.Errorf("parseRowsAffected(%q) = %d, want %d", tag, got, want)
		}
	}
}

func TestLastInsertID(t *testing.T) {
	res := &pgwire.Result{Columns: []string{"id"}, Rows: [][]any{{int64(42)}}}
	id, ok := lastInsertID("INSERT INTO t (x) VALUES ($1) RETURNING id", res)
	if !ok || id != 42 {
		t.Errorf("lastInsertID = %d, %v", id, ok)
	}
	// Not an INSERT — never reported, even with a matching row shape.
	if _, ok := lastInsertID("UPDATE t SET x = 1 RETURNING id", res); ok {
		t.Error("lastInsertID should only apply to INSERT statements")
	}
	// Multi-row/-column results don't have a single "the" id.
	multi := &pgwire.Result{Columns: []string{"id", "name"}, Rows: [][]any{{int64(1), "a"}}}
	if _, ok := lastInsertID("INSERT INTO t (x) VALUES ($1) RETURNING id, name", multi); ok {
		t.Error("lastInsertID should require exactly one column")
	}
}
