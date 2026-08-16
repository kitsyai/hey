package hub

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kitsyai/hey/internal/pgwire"
	"github.com/kitsyai/hey/internal/svc"
)

// postgresBridgeHandler is the v1 postgres HTTP bridge (docs/design/
// services.md §8's "postgres — the shipped exemplar" Bridge): a browser
// cannot open a raw postgres-wire socket, so this translates POST
// /svc/postgres/query and /svc/postgres/exec into a real postgres-wire
// connection (internal/pgwire) using the instance's own generated
// credentials (svc.json), with parameters always bound through the extended
// query protocol — never string-interpolated into SQL.
//
// The wire contract here is pinned by the app-side client, not this
// package — @heypkv/hub-web's heyDatabase.ts (createHeyHubDatabaseProvider)
// is the authoritative spec this handler implements to:
//
//		POST {sql, params?, namespace, name, version, sessionId} -> query: {rows: [{col: val, ...}, ...]}
//		                                                          -> exec:  {rowsAffected, lastInsertId?}
//
//	  - X-Hey-Session pins one real postgres connection per session (see
//	    postgresSessionPool) so BEGIN/COMMIT/ROLLBACK sent as separate /exec
//	    calls on the same session compose into one real transaction — the
//	    hub's other alternative, a fresh connection per request, cannot do
//	    that at all (COMMIT on a connection with no open transaction is a
//	    no-op, not "commit what the last request started").
//	  - {namespace, name} select a dedicated postgres schema (schemaName),
//	    giving every distinct HubDatabase its own tables inside the one
//	    "postgres" database the pack provisions — "hard per-origin namespaces"
//	    (docs/hey-kernel-brief.md), not just a convention two callers could
//	    collide on.
//	  - Uint8Array parameters/results cross the wire as {"$bytes": "<base64>"}
//	    (decodeWireParams / encodeWireValue), symmetric with the client.
//
// Statement scope (checkStatementKind) is a deliberately blunt keyword
// blocklist, not a SQL parser or a read/write split: /query and /exec accept
// the same statements — the app-side contract's query()/exec() choice is "do
// I want rows back" (an app calls query() for `INSERT ... RETURNING id`
// exactly as it does a SELECT), not "is this read-only" — except a short
// list of genuinely dangerous, non-application operations (role/permission/
// superuser-affecting statements, filesystem COPY, arbitrary procedural DO
// blocks) blocked on both endpoints. Permissive enough for the client's real
// needs (BEGIN/COMMIT/ROLLBACK, CREATE TABLE/ALTER TABLE via its migrate()),
// honest about being a blunt instrument rather than a
// substitute for a properly scoped, non-superuser database role. initdb's
// bootstrap user (the postgres pack's generated credentials) IS a superuser
// by construction — provisioning a real scoped bridge role is flagged as
// follow-up work in this build's report, not silently assumed away here.
func postgresBridgeHandler(inst *svc.Instance, pack svc.Pack) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, rest := splitSvcPath(r.URL.Path)
		var kind string
		switch rest {
		case "/query":
			kind = "query"
		case "/exec":
			kind = "exec"
		default:
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "the postgres bridge only accepts POST")
			return
		}
		_ = kind // query vs exec only changes the response shape (rows vs rowsAffected) below

		if pack.Protocol != "postgres-wire" {
			writeJSONError(w, http.StatusNotFound, "no postgres-wire bridge configured for "+inst.Name)
			return
		}
		if !svc.Alive(inst) {
			writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("%s is not running — try `hey up %s`", inst.Name, inst.Name))
			return
		}

		var req struct {
			SQL       string `json:"sql"`
			Params    []any  `json:"params"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			SessionID string `json:"sessionId"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		// X-Hey-Session is the header the client actually sends the session id
		// on (see heyDatabase.ts's createBridgeClient); the JSON body's
		// sessionId field is honored too, for callers that only set the body.
		if h := r.Header.Get("X-Hey-Session"); h != "" {
			req.SessionID = h
		}
		if strings.TrimSpace(req.SQL) == "" {
			writeJSONError(w, http.StatusBadRequest, "sql must not be empty")
			return
		}
		if err := checkStatementKind(req.SQL); err != nil {
			writeJSONError(w, http.StatusForbidden, err.Error())
			return
		}
		params, err := decodeWireParams(req.Params)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		schema := schemaName(req.Namespace, req.Name)
		connect := func() (*pgwire.Conn, error) {
			c, connErr := pgwire.Connect("127.0.0.1", inst.Port, inst.User, inst.Password, "postgres", 5*time.Second)
			if connErr != nil {
				return nil, connErr
			}
			if _, execErr := c.Exec(fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, quoteIdent(schema)), nil); execErr != nil {
				c.Close()
				return nil, execErr
			}
			if _, execErr := c.Exec(fmt.Sprintf(`SET search_path TO %s, public`, quoteIdent(schema)), nil); execErr != nil {
				c.Close()
				return nil, execErr
			}
			return c, nil
		}

		var conn *pgwire.Conn
		var pinned *pooledConn
		if req.SessionID != "" {
			pinned, err = pgSessions.acquire(req.SessionID, connect)
			if err != nil {
				writeJSONError(w, http.StatusBadGateway, "connect: "+err.Error())
				return
			}
			pinned.mu.Lock()
			defer pinned.mu.Unlock()
			conn = pinned.conn
		} else {
			conn, err = connect()
			if err != nil {
				writeJSONError(w, http.StatusBadGateway, "connect: "+err.Error())
				return
			}
			defer conn.Close()
		}

		res, err := conn.Query(req.SQL, params) // Query and Exec both run the same extended-protocol round trip
		if err != nil {
			if req.SessionID != "" {
				// postgres aborts the current transaction (if any) after an
				// error until a ROLLBACK on the SAME connection; dropping the
				// pinned connection here is simpler and self-healing — the
				// client's own rollback-failure handling already tolerates a
				// ROLLBACK that lands on a fresh connection with nothing to
				// roll back (see heyDatabase.ts's transaction()/readTransaction()
				// catch blocks), and every later statement on this session gets
				// a working connection again instead of staying wedged.
				pgSessions.release(req.SessionID)
			}
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch kind {
		case "query":
			_ = json.NewEncoder(w).Encode(map[string]any{"rows": encodeWireRows(res)})
		case "exec":
			resp := map[string]any{"rowsAffected": parseRowsAffected(res.CommandTag)}
			if id, ok := lastInsertID(req.SQL, res); ok {
				resp["lastInsertId"] = id
			}
			_ = json.NewEncoder(w).Encode(resp)
		}
	})
}

// --- session-pinned connections ---------------------------------------------

// sessionIdleTimeout bounds how long an idle session's connection is kept
// open. The client never signals "session closed" over the wire (close() is
// purely client-local — see heyDatabase.ts), so idle eviction is the only
// thing that bounds how many postgres connections a hub can accumulate.
const sessionIdleTimeout = 10 * time.Minute

type pooledConn struct {
	conn     *pgwire.Conn
	mu       sync.Mutex // serializes requests that land on the same session concurrently
	lastUsed time.Time
}

type postgresSessionPool struct {
	mu       sync.Mutex
	sessions map[string]*pooledConn
}

var pgSessions = &postgresSessionPool{sessions: map[string]*pooledConn{}}

// acquire returns the pinned connection for sessionID, creating one via
// connect if this is the session's first request. Every acquire also
// opportunistically evicts idle-expired sessions, so a hub that keeps
// receiving traffic (from any session) never accumulates unbounded
// connections even though there is no explicit close signal.
func (p *postgresSessionPool) acquire(sessionID string, connect func() (*pgwire.Conn, error)) (*pooledConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reapLocked()
	if pc, ok := p.sessions[sessionID]; ok {
		pc.lastUsed = time.Now()
		return pc, nil
	}
	conn, err := connect()
	if err != nil {
		return nil, err
	}
	pc := &pooledConn{conn: conn, lastUsed: time.Now()}
	p.sessions[sessionID] = pc
	return pc, nil
}

// release closes and drops sessionID's pinned connection, if any.
func (p *postgresSessionPool) release(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pc, ok := p.sessions[sessionID]; ok {
		_ = pc.conn.Close()
		delete(p.sessions, sessionID)
	}
}

func (p *postgresSessionPool) reapLocked() {
	cutoff := time.Now().Add(-sessionIdleTimeout)
	for id, pc := range p.sessions {
		if pc.lastUsed.Before(cutoff) {
			_ = pc.conn.Close()
			delete(p.sessions, id)
		}
	}
}

// --- per-namespace schema isolation -----------------------------------------

// schemaName derives a stable, injection-proof postgres schema name from a
// HubDatabase descriptor's (namespace, name) — hashed rather than sanitized
// so arbitrary client-supplied strings (an origin URL, unicode, anything)
// never need identifier-escaping logic to be trusted. Every distinct
// (namespace, name) pair — in practice, namespace is the calling origin —
// gets its own schema, so two apps' hub databases can never collide on the
// same tables inside the one shared "postgres" database the pack provisions.
func schemaName(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	return "hey_" + hex.EncodeToString(sum[:])[:40]
}

func quoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

// --- request/response wire shape --------------------------------------------

// decodeWireParams converts the client's JSON params (heyDatabase.ts's
// encodeParameters) into pgwire bind values: a {"$bytes": "<base64>"} marker
// becomes pgwire.Bytes (sent as postgres's \x-hex bytea text form); every
// other JSON-native value (string/number/bool/null) passes through as-is.
func decodeWireParams(raw []any) ([]any, error) {
	out := make([]any, len(raw))
	for i, v := range raw {
		obj, ok := v.(map[string]any)
		if !ok {
			out[i] = v
			continue
		}
		b64, ok := obj["$bytes"].(string)
		if !ok || len(obj) != 1 {
			return nil, fmt.Errorf("hey hub postgres bridge: unsupported object parameter at index %d", i)
		}
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("hey hub postgres bridge: bad $bytes parameter at index %d: %w", i, err)
		}
		out[i] = pgwire.Bytes(data)
	}
	return out, nil
}

// encodeWireRows zips a pgwire.Result's columns/rows into the client's
// expected shape: an array of column-name-keyed objects, not parallel
// columns+rows arrays.
func encodeWireRows(res *pgwire.Result) []map[string]any {
	rows := make([]map[string]any, len(res.Rows))
	for i, row := range res.Rows {
		m := make(map[string]any, len(res.Columns))
		for j, col := range res.Columns {
			if j < len(row) {
				m[col] = encodeWireValue(row[j])
			}
		}
		rows[i] = m
	}
	return rows
}

// encodeWireValue applies the client's {"$bytes": base64} marker to any
// []byte value (pgwire decodes bytea columns to []byte — see
// internal/pgwire/conn.go's decodeValue); everything else is already
// JSON-native (bool/int64/float64/string/nil) and passes straight through.
func encodeWireValue(v any) any {
	if b, ok := v.([]byte); ok {
		return map[string]string{"$bytes": base64.StdEncoding.EncodeToString(b)}
	}
	return v
}

// parseRowsAffected extracts the row count from a postgres CommandTag
// ("INSERT 0 3", "UPDATE 3", "DELETE 2"; "BEGIN"/"COMMIT"/"ROLLBACK" have no
// count and yield 0, matching the client's own `?? 0` default).
func parseRowsAffected(tag string) int {
	fields := strings.Fields(tag)
	if len(fields) == 0 {
		return 0
	}
	last := fields[len(fields)-1]
	n := 0
	for _, c := range last {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// lastInsertID reports the client's optional lastInsertId: a single-row,
// single-column integer result from an INSERT ... RETURNING <id> statement —
// postgres has no automatic equivalent to MySQL/SQLite's last-insert-id, so
// the app is expected to RETURNING the column it wants back.
func lastInsertID(sql string, res *pgwire.Result) (int64, bool) {
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "INSERT") {
		return 0, false
	}
	if len(res.Rows) != 1 || len(res.Columns) != 1 {
		return 0, false
	}
	id, ok := res.Rows[0][0].(int64)
	return id, ok
}

// --- statement scope ---------------------------------------------------------

// blockedKeywords are refused through EITHER endpoint: role/permission/
// superuser-affecting statements, filesystem access, and arbitrary
// procedural code — nothing an application-level schema migration,
// transaction, or query legitimately needs. Everything else is allowed
// through both /query and /exec: the app-side contract's query()/exec()
// split (heyDatabase.ts) is "do I want rows back", not "is this read-only" —
// an app calls query() for `INSERT ... RETURNING id` exactly as it does for
// a SELECT, and exec() for `CREATE TABLE`/`BEGIN`/`COMMIT` exactly as it does
// for a plain INSERT — so a kind-based SELECT-only/no-SELECT split on top of
// this list would reject statements the shipped client legitimately sends.
var blockedKeywords = []string{
	"DROP DATABASE", "DROP ROLE", "DROP USER", "CREATE ROLE", "CREATE USER",
	"ALTER ROLE", "ALTER USER", "ALTER SYSTEM", "GRANT", "REVOKE",
	"COPY", "REASSIGN", "SECURITY LABEL", "SET ROLE", "RESET ROLE",
	"LISTEN", "NOTIFY", "DO",
}

func checkStatementKind(sql string) error {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	for _, kw := range blockedKeywords {
		if strings.HasPrefix(upper, kw) {
			return fmt.Errorf("hey hub postgres bridge: %s is not allowed through the browser bridge", kw)
		}
	}
	return nil
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
