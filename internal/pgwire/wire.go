// Package pgwire implements the minimal subset of the PostgreSQL frontend/
// backend wire protocol (v3) the hub's postgres HTTP bridge needs: startup,
// SCRAM-SHA-256 authentication (the auth method internal/svc's postgres pack
// initializes with, -A scram-sha-256), the extended query protocol
// (parameterized statements — no string-interpolated SQL), and simple-query
// for statements that return no rows.
//
// hey ships with zero third-party dependencies (see go.mod) — the same
// discipline cmd/hey/svc_integration_test.go's hand-rolled SSLRequest probe
// already follows for a lighter proof of the same principle — so this
// package hand-rolls the protocol rather than vendoring a postgres driver.
// It is deliberately not a general-purpose client: no COPY, no LISTEN/
// NOTIFY, no prepared-statement caching across requests — exactly the "no
// DDL/superuser path reachable from a browser" scope
// docs/design/services.md §8 asks the bridge for, and nothing more.
package pgwire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// message is one backend (server->client) protocol message: a type byte
// (absent only for the client's initial StartupMessage) plus its payload,
// length-prefixed already stripped.
type message struct {
	typ     byte
	payload []byte
}

// writeStartup sends the (type-byte-less) StartupMessage: protocol version
// 3.0 plus "key\0value\0" pairs, terminated by a final \0.
func writeStartup(w io.Writer, params map[string]string) error {
	var body []byte
	body = appendUint32(body, 196608) // 3<<16 | 0 : protocol version 3.0
	for k, v := range params {
		body = append(body, k...)
		body = append(body, 0)
		body = append(body, v...)
		body = append(body, 0)
	}
	body = append(body, 0) // terminator

	var out []byte
	out = appendUint32(out, uint32(len(body)+4))
	out = append(out, body...)
	_, err := w.Write(out)
	return err
}

// writeMessage sends a typed frontend (client->server) message: 1 byte type
// + int32 length (includes itself, excludes the type byte) + payload.
func writeMessage(w io.Writer, typ byte, payload []byte) error {
	out := make([]byte, 0, 5+len(payload))
	out = append(out, typ)
	out = appendUint32(out, uint32(len(payload)+4))
	out = append(out, payload...)
	_, err := w.Write(out)
	return err
}

// readMessage reads one typed backend message.
func readMessage(r io.Reader) (message, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return message{}, err
	}
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length < 4 {
		return message{}, fmt.Errorf("pgwire: invalid message length %d", length)
	}
	payload := make([]byte, length-4)
	if len(payload) > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return message{}, err
		}
	}
	return message{typ: hdr[0], payload: payload}, nil
}

func appendUint32(b []byte, v uint32) []byte {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendInt16(b []byte, v int16) []byte {
	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], uint16(v))
	return append(b, tmp[:]...)
}

// cString reads a NUL-terminated string from buf starting at off, returning
// the string and the offset just past its terminator.
func cString(buf []byte, off int) (string, int, error) {
	i := off
	for i < len(buf) && buf[i] != 0 {
		i++
	}
	if i >= len(buf) {
		return "", 0, fmt.Errorf("pgwire: unterminated string in message")
	}
	return string(buf[off:i]), i + 1, nil
}

// errorFromFields decodes a backend ErrorResponse/NoticeResponse payload (a
// sequence of "code byte + \0-terminated string" fields, terminated by a
// trailing \0) into a readable message.
func errorFromFields(payload []byte) error {
	var severity, code, msg string
	i := 0
	for i < len(payload) && payload[i] != 0 {
		field := payload[i]
		i++
		var s string
		var err error
		s, i, err = cString(payload, i)
		if err != nil {
			break
		}
		switch field {
		case 'S':
			severity = s
		case 'C':
			code = s
		case 'M':
			msg = s
		}
	}
	if msg == "" {
		return fmt.Errorf("pgwire: server error (unparseable)")
	}
	return fmt.Errorf("pgwire: %s %s: %s", severity, code, msg)
}
