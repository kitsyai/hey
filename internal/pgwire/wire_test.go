package pgwire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWriteReadMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeMessage(&buf, 'Q', []byte("SELECT 1")); err != nil {
		t.Fatal(err)
	}
	msg, err := readMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if msg.typ != 'Q' {
		t.Errorf("typ = %q, want Q", msg.typ)
	}
	if string(msg.payload) != "SELECT 1" {
		t.Errorf("payload = %q", msg.payload)
	}
}

func TestWriteStartupFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStartup(&buf, map[string]string{"user": "hey_abc"}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	length := binary.BigEndian.Uint32(data[0:4])
	if int(length) != len(data) {
		t.Errorf("declared length %d != actual %d", length, len(data))
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version != 196608 {
		t.Errorf("protocol version = %d, want 196608 (3.0)", version)
	}
	if !bytes.Contains(data, []byte("user\x00hey_abc\x00")) {
		t.Error("startup message missing user param")
	}
	if data[len(data)-1] != 0 {
		t.Error("startup message must end with a NUL terminator")
	}
}

func TestCString(t *testing.T) {
	buf := []byte("hello\x00world\x00")
	s, next, err := cString(buf, 0)
	if err != nil || s != "hello" || next != 6 {
		t.Fatalf("cString = %q, %d, %v", s, next, err)
	}
	s2, next2, err := cString(buf, next)
	if err != nil || s2 != "world" || next2 != 12 {
		t.Fatalf("cString = %q, %d, %v", s2, next2, err)
	}
}

func TestCStringUnterminated(t *testing.T) {
	if _, _, err := cString([]byte("no terminator"), 0); err == nil {
		t.Error("expected an error for an unterminated string")
	}
}

func TestErrorFromFields(t *testing.T) {
	// Field layout: 1 byte code + \0-terminated string, repeated, trailing \0.
	var payload []byte
	payload = append(payload, 'S')
	payload = append(payload, "ERROR\x00"...)
	payload = append(payload, 'C')
	payload = append(payload, "42501\x00"...)
	payload = append(payload, 'M')
	payload = append(payload, "permission denied\x00"...)
	payload = append(payload, 0)

	err := errorFromFields(payload)
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	msg := err.Error()
	for _, want := range []string{"ERROR", "42501", "permission denied"} {
		if !bytes.Contains([]byte(msg), []byte(want)) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func TestReadMessageTruncated(t *testing.T) {
	// A header claiming more payload than actually follows must error, not panic.
	buf := bytes.NewBuffer([]byte{'Q', 0, 0, 0, 20, 'a', 'b'})
	if _, err := readMessage(buf); err == nil {
		t.Error("expected an error reading a truncated message")
	}
}
