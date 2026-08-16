package pgwire

import (
	"reflect"
	"testing"
)

// buildRowDescription constructs a minimal 'T' RowDescription payload for
// the given column names/OIDs — enough of the real shape (name, tableOID,
// attnum, typeOID, typeSize, typeMod, formatCode) for parseRowDescription.
func buildRowDescription(names []string, oids []uint32) []byte {
	var buf []byte
	buf = appendInt16(buf, int16(len(names)))
	for i, name := range names {
		buf = append(buf, name...)
		buf = append(buf, 0)
		buf = appendUint32(buf, 0)       // tableOID
		buf = appendInt16(buf, 0)        // attnum
		buf = appendUint32(buf, oids[i]) // dataTypeOID
		buf = appendInt16(buf, 0)        // typeSize
		buf = appendUint32(buf, 0)       // typeMod
		buf = appendInt16(buf, 0)        // formatCode
	}
	return buf
}

func TestParseRowDescription(t *testing.T) {
	payload := buildRowDescription([]string{"id", "name"}, []uint32{oidInt4, 25 /* text */})
	names, oids := parseRowDescription(payload)
	if !reflect.DeepEqual(names, []string{"id", "name"}) {
		t.Errorf("names = %v", names)
	}
	if !reflect.DeepEqual(oids, []uint32{oidInt4, 25}) {
		t.Errorf("oids = %v", oids)
	}
}

// buildDataRow constructs a 'D' DataRow payload; values[i]==nil encodes SQL NULL.
func buildDataRow(values [][]byte) []byte {
	var buf []byte
	buf = appendInt16(buf, int16(len(values)))
	for _, v := range values {
		if v == nil {
			buf = append(buf, 0xFF, 0xFF, 0xFF, 0xFF)
			continue
		}
		buf = appendUint32(buf, uint32(len(v)))
		buf = append(buf, v...)
	}
	return buf
}

func TestParseDataRow_TypeCoercion(t *testing.T) {
	oids := []uint32{oidInt4, oidBool, oidFloat8, 25 /* text */, oidInt4 /* NULL */}
	payload := buildDataRow([][]byte{[]byte("42"), []byte("t"), []byte("3.5"), []byte("hello"), nil})
	row, err := parseDataRow(payload, oids)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{int64(42), true, 3.5, "hello", nil}
	if !reflect.DeepEqual(row, want) {
		t.Errorf("row = %#v, want %#v", row, want)
	}
}

func TestParseDataRow_Truncated(t *testing.T) {
	// Declares 2 columns but only provides bytes for a length header, no value.
	var buf []byte
	buf = appendInt16(buf, 1)
	buf = appendUint32(buf, 100) // claims 100 bytes but none follow
	if _, err := parseDataRow(buf, []uint32{25}); err == nil {
		t.Error("expected an error for a truncated DataRow value")
	}
}

func TestDecodeValue_BoolFalse(t *testing.T) {
	if v := decodeValue(oidBool, []byte("f")); v != false {
		t.Errorf("decodeValue(bool, f) = %v, want false", v)
	}
}

func TestDecodeValue_UnknownOIDStaysString(t *testing.T) {
	// e.g. numeric/timestamp/uuid/json — anything without an explicit case
	// stays a string, never silently mangled.
	if v := decodeValue(1700 /* numeric */, []byte("12345678901234567890.123")); v != "12345678901234567890.123" {
		t.Errorf("decodeValue(numeric) = %v", v)
	}
}
