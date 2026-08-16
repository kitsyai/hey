package pgwire

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Bytes wraps a []byte parameter so Query/Exec sends it as postgres's
// "\x"-prefixed hex bytea text format instead of fmt.Sprint's default (and
// wrong — "[104 101 108 108 111]") representation of a byte slice. The hub's
// postgres bridge uses this for the client's { "$bytes": "<base64>" }
// parameter marker (see internal/hub/bridge_postgres.go).
type Bytes []byte

// Conn is a single, unpooled connection to a postgres backend, speaking just
// enough of the wire protocol for the hub's query/exec bridge (see the
// package doc). It is not safe for concurrent use.
type Conn struct {
	c net.Conn
	r *bufio.Reader
}

// Connect dials host:port, completes the startup message and
// SCRAM-SHA-256 (or cleartext) authentication, and waits for the server to
// report ready. The returned Conn is ready for Query/Exec.
func Connect(host string, port int, user, password, database string, timeout time.Duration) (*Conn, error) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return nil, fmt.Errorf("pgwire: dial: %w", err)
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	conn := &Conn{c: c, r: bufio.NewReader(c)}
	if err := conn.startup(user, password, database); err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{}) // startup deadline only; callers set their own per-call
	return conn, nil
}

// Close sends Terminate and closes the underlying connection.
func (c *Conn) Close() error {
	_ = writeMessage(c.c, 'X', nil)
	return c.c.Close()
}

func (c *Conn) startup(user, password, database string) error {
	if err := writeStartup(c.c, map[string]string{
		"user": user, "database": database, "client_encoding": "UTF8",
	}); err != nil {
		return err
	}
	for {
		msg, err := readMessage(c.r)
		if err != nil {
			return fmt.Errorf("pgwire: startup: %w", err)
		}
		switch msg.typ {
		case 'E':
			return errorFromFields(msg.payload)
		case 'R':
			if len(msg.payload) < 4 {
				return fmt.Errorf("pgwire: malformed authentication message")
			}
			authType := binary.BigEndian.Uint32(msg.payload[0:4])
			switch authType {
			case 0: // AuthenticationOk
				// continue; ParameterStatus/BackendKeyData/ReadyForQuery follow
			case 3: // AuthenticationCleartextPassword
				if err := writeMessage(c.c, 'p', append([]byte(password), 0)); err != nil {
					return err
				}
			case 10: // AuthenticationSASL
				if err := c.doSCRAM(password); err != nil {
					return err
				}
			default:
				return fmt.Errorf("pgwire: unsupported authentication method %d (this bridge speaks scram-sha-256 and cleartext only)", authType)
			}
		case 'S', 'K': // ParameterStatus, BackendKeyData
			// informational; not needed for the bridge's purposes.
		case 'N': // NoticeResponse
			// non-fatal; ignore.
		case 'Z': // ReadyForQuery
			return nil
		default:
			return fmt.Errorf("pgwire: unexpected message %q during startup", msg.typ)
		}
	}
}

// doSCRAM runs the SCRAM-SHA-256 exchange to completion, including consuming
// the trailing AuthenticationOk so the caller's startup loop just continues.
func (c *Conn) doSCRAM(password string) error {
	first, state, err := scramClientFirst()
	if err != nil {
		return err
	}
	if err := writeMessage(c.c, 'p', first); err != nil {
		return err
	}

	msg, err := readMessage(c.r)
	if err != nil {
		return fmt.Errorf("pgwire: SCRAM: %w", err)
	}
	if msg.typ == 'E' {
		return errorFromFields(msg.payload)
	}
	if msg.typ != 'R' || len(msg.payload) < 4 || binary.BigEndian.Uint32(msg.payload[0:4]) != 11 {
		return fmt.Errorf("pgwire: expected AuthenticationSASLContinue")
	}
	final, err := scramClientFinal(state, msg.payload[4:], password)
	if err != nil {
		return err
	}
	if err := writeMessage(c.c, 'p', final); err != nil {
		return err
	}

	msg2, err := readMessage(c.r)
	if err != nil {
		return fmt.Errorf("pgwire: SCRAM: %w", err)
	}
	if msg2.typ == 'E' {
		return errorFromFields(msg2.payload)
	}
	if msg2.typ != 'R' || len(msg2.payload) < 4 || binary.BigEndian.Uint32(msg2.payload[0:4]) != 12 {
		return fmt.Errorf("pgwire: expected AuthenticationSASLFinal")
	}
	if err := scramVerifyServerFinal(state, msg2.payload[4:]); err != nil {
		return err
	}

	msg3, err := readMessage(c.r)
	if err != nil {
		return fmt.Errorf("pgwire: SCRAM: %w", err)
	}
	if msg3.typ == 'E' {
		return errorFromFields(msg3.payload)
	}
	if msg3.typ != 'R' || len(msg3.payload) < 4 || binary.BigEndian.Uint32(msg3.payload[0:4]) != 0 {
		return fmt.Errorf("pgwire: expected AuthenticationOk after SCRAM")
	}
	return nil
}

// Result is one statement's outcome: Columns/Rows for a query (rows decoded
// from postgres's text wire format into JSON-native Go values by column type
// OID — see decodeValue), CommandTag always set (e.g. "SELECT 3", "INSERT 0 1").
type Result struct {
	Columns    []string
	Rows       [][]any
	CommandTag string
}

// Query runs sql as a parameterized statement (never string-interpolated —
// params are bound via the extended query protocol's Bind message) and
// returns its result rows.
func (c *Conn) Query(sql string, params []any) (*Result, error) {
	return c.execExtended(sql, params)
}

// Exec runs sql as a parameterized statement and returns its command tag
// (row count etc.), for statements that don't return rows.
func (c *Conn) Exec(sql string, params []any) (*Result, error) {
	return c.execExtended(sql, params)
}

// execExtended runs the Parse/Bind/Describe/Execute/Sync sequence — the
// "extended query" protocol — so parameters are sent as typed values, never
// spliced into the SQL text.
func (c *Conn) execExtended(sql string, params []any) (*Result, error) {
	textParams := make([][]byte, len(params))
	for i, p := range params {
		switch v := p.(type) {
		case nil:
			textParams[i] = nil
		case Bytes:
			textParams[i] = []byte("\\x" + hex.EncodeToString(v))
		default:
			textParams[i] = []byte(fmt.Sprint(v))
		}
	}

	// Parse: unnamed statement, unnamed portal throughout.
	parsePayload := []byte{0} // statement name = ""
	parsePayload = append(parsePayload, sql...)
	parsePayload = append(parsePayload, 0)
	parsePayload = appendInt16(parsePayload, 0) // let the server infer param types
	if err := writeMessage(c.c, 'P', parsePayload); err != nil {
		return nil, err
	}

	// Bind: unnamed portal, unnamed statement, all-text param + result formats.
	bindPayload := []byte{0, 0} // portal "", statement ""
	bindPayload = appendInt16(bindPayload, 1)
	bindPayload = appendInt16(bindPayload, 0) // format code 0 (text) applies to all params
	bindPayload = appendInt16(bindPayload, int16(len(textParams)))
	for _, v := range textParams {
		if v == nil {
			bindPayload = append(bindPayload, 0xFF, 0xFF, 0xFF, 0xFF) // length -1: NULL
			continue
		}
		bindPayload = appendUint32(bindPayload, uint32(len(v)))
		bindPayload = append(bindPayload, v...)
	}
	bindPayload = appendInt16(bindPayload, 1)
	bindPayload = appendInt16(bindPayload, 0) // result format: text
	if err := writeMessage(c.c, 'B', bindPayload); err != nil {
		return nil, err
	}

	// Describe the (unnamed) portal, so a query's shape is known before Execute.
	describePayload := []byte{'P', 0}
	if err := writeMessage(c.c, 'D', describePayload); err != nil {
		return nil, err
	}

	// Execute: unnamed portal, no row limit.
	execPayload := []byte{0}
	execPayload = appendUint32(execPayload, 0)
	if err := writeMessage(c.c, 'E', execPayload); err != nil {
		return nil, err
	}

	if err := writeMessage(c.c, 'S', nil); err != nil {
		return nil, err
	}

	return c.readExtendedResult()
}

// readExtendedResult reads the response stream for one
// Parse/Bind/Describe/Execute/Sync round trip through to ReadyForQuery.
func (c *Conn) readExtendedResult() (*Result, error) {
	res := &Result{}
	var colOIDs []uint32
	var firstErr error

	for {
		msg, err := readMessage(c.r)
		if err != nil {
			return nil, fmt.Errorf("pgwire: %w", err)
		}
		switch msg.typ {
		case '1': // ParseComplete
		case '2': // BindComplete
		case 'n': // NoData (statement returns no rows, e.g. INSERT/UPDATE)
		case 't': // ParameterDescription (from Describe) — not needed here
		case 'T': // RowDescription
			res.Columns, colOIDs = parseRowDescription(msg.payload)
		case 'D': // DataRow
			row, err := parseDataRow(msg.payload, colOIDs)
			if err != nil {
				return nil, err
			}
			res.Rows = append(res.Rows, row)
		case 'C': // CommandComplete
			res.CommandTag = parseCString(msg.payload)
		case 'E': // ErrorResponse
			if firstErr == nil {
				firstErr = errorFromFields(msg.payload)
			}
		case 'N': // NoticeResponse — non-fatal
		case 'Z': // ReadyForQuery — end of this round trip
			return res, firstErr
		default:
			// Unknown/irrelevant message kind; ignore rather than fail, so a
			// future minor protocol addition doesn't break the bridge.
		}
	}
}

func parseCString(payload []byte) string {
	s, _, err := cString(payload, 0)
	if err != nil {
		return ""
	}
	return s
}

// parseRowDescription decodes a 'T' message into column names and their
// postgres type OIDs (used by parseDataRow for text->JSON coercion).
func parseRowDescription(payload []byte) (names []string, oids []uint32) {
	if len(payload) < 2 {
		return nil, nil
	}
	count := binary.BigEndian.Uint16(payload[0:2])
	off := 2
	names = make([]string, 0, count)
	oids = make([]uint32, 0, count)
	for i := 0; i < int(count); i++ {
		name, next, err := cString(payload, off)
		if err != nil {
			break
		}
		off = next
		if off+18 > len(payload) {
			break
		}
		// tableOID(4) + columnAttrNum(2) already skipped; dataTypeOID next(4)
		oid := binary.BigEndian.Uint32(payload[off+6 : off+10])
		off += 18 // tableOID(4)+attnum(2)+typeOID(4)+typeSize(2)+typeMod(4)+formatCode(2)
		names = append(names, name)
		oids = append(oids, oid)
	}
	return names, oids
}

// parseDataRow decodes a 'D' message into JSON-native values, coercing by
// column type OID where a lossless, unambiguous conversion exists (bool,
// integers, floats); everything else (text, numeric, timestamps, uuid, json)
// stays a string, which is both simplest and never loses precision.
func parseDataRow(payload []byte, oids []uint32) ([]any, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("pgwire: malformed DataRow")
	}
	count := binary.BigEndian.Uint16(payload[0:2])
	off := 2
	row := make([]any, 0, count)
	for i := 0; i < int(count); i++ {
		if off+4 > len(payload) {
			return nil, fmt.Errorf("pgwire: truncated DataRow")
		}
		length := int32(binary.BigEndian.Uint32(payload[off : off+4]))
		off += 4
		if length < 0 {
			row = append(row, nil)
			continue
		}
		if off+int(length) > len(payload) {
			return nil, fmt.Errorf("pgwire: truncated DataRow value")
		}
		raw := payload[off : off+int(length)]
		off += int(length)
		var oid uint32
		if i < len(oids) {
			oid = oids[i]
		}
		row = append(row, decodeValue(oid, raw))
	}
	return row, nil
}

// postgres type OIDs for the columns worth coercing out of text format.
const (
	oidBool   = 16
	oidBytea  = 17
	oidInt8   = 20
	oidInt2   = 21
	oidInt4   = 23
	oidFloat4 = 700
	oidFloat8 = 701
)

// decodeValue coerces one column's text-format value by its postgres type
// OID. bytea decodes to a Go []byte (postgres's default bytea_output is
// "hex": "\x..." — the only encoding this bridge understands; a server
// configured for the legacy "escape" output falls back to the raw string
// rather than mis-decoding it). Everything without an explicit case — text,
// numeric, timestamp, uuid, json, ... — stays a string: simplest, and never
// loses precision the way a lossy numeric-to-float64 coercion could.
func decodeValue(oid uint32, raw []byte) any {
	s := string(raw)
	switch oid {
	case oidBool:
		return s == "t"
	case oidBytea:
		if strings.HasPrefix(s, "\\x") {
			if data, err := hex.DecodeString(s[2:]); err == nil {
				return data
			}
		}
		return s
	case oidInt2, oidInt4, oidInt8:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	case oidFloat4, oidFloat8:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return s
}
