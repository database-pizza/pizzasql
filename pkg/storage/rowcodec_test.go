package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"github.com/goccy/go-json"
)

func TestEncodeRowDeterministic(t *testing.T) {
	row := Row{
		"b": int64(2),
		"a": int64(1),
		"c": int64(3),
	}
	first, err := encodeRow(row)
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	// Rebuild with the same pairs in a different insertion order.
	rowAgain := Row{}
	rowAgain["c"] = int64(3)
	rowAgain["a"] = int64(1)
	rowAgain["b"] = int64(2)
	second, err := encodeRow(rowAgain)
	if err != nil {
		t.Fatalf("encodeRow again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("encoding is not deterministic:\n%x\n%x", first, second)
	}
}

func TestEncodeRowBinaryBytes(t *testing.T) {
	got, err := encodeRow(Row{"a": int64(1)})
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	want := []byte{
		'P', 'Z', 'S', 'Q', 'L', 'R', 'O', 'W', // magic
		0x01,                   // version
		0x01, 0x00, 0x00, 0x00, // count = 1
		0x01, 0x00, 0x00, 0x00, // nameLen = 1
		'a',                                            // name
		0x03,                                           // tagInt
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // int64(1)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("binary bytes = %x, want %x", got, want)
	}
}

func TestEncodeDecodeRoundTripAllTypes(t *testing.T) {
	in := Row{
		"nil":   nil,
		"bt":    true,
		"bf":    false,
		"i":     int(42),
		"i8":    int8(-8),
		"i16":   int16(-1600),
		"i32":   int32(-70000),
		"i64":   int64(-9000000000000000000),
		"u":     uint(7),
		"u8":    uint8(200),
		"u16":   uint16(60000),
		"u32":   uint32(4000000000),
		"u64":   uint64(18446744073709551615),
		"f32":   float32(1.5),
		"f64":   float64(-2.25),
		"str":   "hello",
		"bytes": []byte{0x00, 0xff, 0x01, '\n'},
		"num":   json.Number("12345678901234567890"),
	}
	want := Row{
		"nil":   nil,
		"bt":    true,
		"bf":    false,
		"i":     int64(42),
		"i8":    int64(-8),
		"i16":   int64(-1600),
		"i32":   int64(-70000),
		"i64":   int64(-9000000000000000000),
		"u":     uint64(7),
		"u8":    uint64(200),
		"u16":   uint64(60000),
		"u32":   uint64(4000000000),
		"u64":   uint64(18446744073709551615),
		"f32":   float32(1.5),
		"f64":   float64(-2.25),
		"str":   "hello",
		"bytes": []byte{0x00, 0xff, 0x01, '\n'},
		"num":   json.Number("12345678901234567890"),
	}

	data, err := encodeRow(in)
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	if len(data) < len(rowMagic) || string(data[:len(rowMagic)]) != rowMagic {
		t.Fatalf("binary row missing magic prefix: %x", data)
	}

	got, err := decodeRow(data)
	if err != nil {
		t.Fatalf("decodeRow: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got = %#v\nwant = %#v", got, want)
	}
}

func TestEncodeRowJSONNumberExact(t *testing.T) {
	// A decimal that would lose precision as a float64 must round-trip exactly.
	row := Row{"n": json.Number("0.123456789012345678901234567890")}
	data, err := encodeRow(row)
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	got, err := decodeRow(data)
	if err != nil {
		t.Fatalf("decodeRow: %v", err)
	}
	if got["n"] != json.Number("0.123456789012345678901234567890") {
		t.Fatalf("number = %#v, want exact json.Number", got["n"])
	}
}

func TestDecodeLegacyJSON(t *testing.T) {
	legacy := []byte(`{"_rowid_":7,"name":"alice","score":12.5,"active":true,"extra":null}`)
	got, err := decodeRow(legacy)
	if err != nil {
		t.Fatalf("decodeRow: %v", err)
	}
	if got["_rowid_"] != float64(7) {
		t.Fatalf("_rowid_ = %#v, want float64(7)", got["_rowid_"])
	}
	if got["name"] != "alice" {
		t.Fatalf("name = %#v", got["name"])
	}
	if got["score"] != float64(12.5) {
		t.Fatalf("score = %#v", got["score"])
	}
	if got["active"] != true {
		t.Fatalf("active = %#v", got["active"])
	}
	if got["extra"] != nil {
		t.Fatalf("extra = %#v", got["extra"])
	}
}

func TestEncodeRowUnsupportedValueFallsBackToJSON(t *testing.T) {
	row := Row{"id": int64(1), "tags": []string{"a", "b"}}
	data, err := encodeRow(row)
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	if len(data) >= len(rowMagic) && string(data[:len(rowMagic)]) == rowMagic {
		t.Fatalf("expected JSON fallback, got binary magic: %x", data)
	}

	var decoded Row
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("fallback is not valid JSON: %v", err)
	}
	if decoded["id"] != float64(1) {
		t.Fatalf("id = %#v", decoded["id"])
	}
	tags, ok := decoded["tags"].([]interface{})
	if !ok || len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Fatalf("tags = %#v", decoded["tags"])
	}
}

func TestEncodeRowJSONFallbackRoundTripsThroughDecode(t *testing.T) {
	row := Row{"nested": map[string]interface{}{"x": 1, "y": []interface{}{true, nil}}}
	data, err := encodeRow(row)
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	got, err := decodeRow(data)
	if err != nil {
		t.Fatalf("decodeRow: %v", err)
	}
	if _, ok := got["nested"].(map[string]interface{}); !ok {
		t.Fatalf("nested = %#v, want map", got["nested"])
	}
}

func TestDecodeBinaryRowTruncated(t *testing.T) {
	data, err := encodeRow(Row{"name": "alice", "id": int64(5), "payload": []byte("data")})
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	for _, n := range []int{1, len(rowMagic), rowHeaderLen, rowHeaderLen + 1, len(data) - 1} {
		trunc := data[:n]
		if _, err := decodeRow(trunc); err == nil {
			t.Fatalf("decodeRow(truncated to %d bytes) succeeded, want error", n)
		}
	}
}

func TestDecodeBinaryRowTrailingBytes(t *testing.T) {
	data, err := encodeRow(Row{"id": int64(1)})
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	withTrailing := append(append([]byte(nil), data...), 0x00, 0x01, 0x02)
	if _, err := decodeRow(withTrailing); err == nil {
		t.Fatalf("decodeRow with trailing bytes succeeded, want error")
	}
}

func TestDecodeBinaryRowUnknownTag(t *testing.T) {
	var buf []byte
	buf = append(buf, rowMagic...)
	buf = append(buf, rowVersion)
	buf = appendU32(buf, 1)
	buf = appendU32(buf, 2)
	buf = append(buf, "id"...)
	buf = append(buf, 0x7f) // unknown tag
	buf = append(buf, 0, 0, 0, 0, 0, 0, 0, 0)
	if _, err := decodeRow(buf); !errors.Is(err, errMalformedRow) {
		t.Fatalf("err = %v, want errMalformedRow", err)
	}
}

func TestDecodeBinaryRowUnknownVersion(t *testing.T) {
	data, err := encodeRow(Row{"id": int64(1)})
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	corrupted := append([]byte(nil), data...)
	corrupted[len(rowMagic)] = 0x7f
	if _, err := decodeRow(corrupted); !errors.Is(err, errMalformedRow) {
		t.Fatalf("err = %v, want errMalformedRow", err)
	}
}

func TestDecodeBinaryRowOversizedNameLength(t *testing.T) {
	var buf []byte
	buf = append(buf, rowMagic...)
	buf = append(buf, rowVersion)
	buf = appendU32(buf, 1)
	buf = appendU32(buf, uint32(maxRowFieldLen+1)) // oversized name length
	buf = append(buf, 'x')
	if _, err := decodeRow(buf); err == nil {
		t.Fatalf("decodeRow with oversized name length succeeded, want error")
	}
}

func TestDecodeBinaryRowOversizedValueLength(t *testing.T) {
	var buf []byte
	buf = append(buf, rowMagic...)
	buf = append(buf, rowVersion)
	buf = appendU32(buf, 1)
	buf = appendU32(buf, 1)
	buf = append(buf, 'a')
	buf = append(buf, tagString)
	buf = appendU32(buf, uint32(maxRowFieldLen+1)) // oversized string length
	if _, err := decodeRow(buf); err == nil {
		t.Fatalf("decodeRow with oversized value length succeeded, want error")
	}
}

func TestDecodeBinaryRowStringLengthExceedsInput(t *testing.T) {
	var buf []byte
	buf = append(buf, rowMagic...)
	buf = append(buf, rowVersion)
	buf = appendU32(buf, 1)
	buf = appendU32(buf, 1)
	buf = append(buf, 'a')
	buf = append(buf, tagString)
	buf = appendU32(buf, 100) // claims 100 bytes but only 0 follow
	if _, err := decodeRow(buf); err == nil {
		t.Fatalf("decodeRow with lying string length succeeded, want error")
	}
}

func TestDecodeBinaryRowImpossibleFieldCount(t *testing.T) {
	var buf []byte
	buf = append(buf, rowMagic...)
	buf = append(buf, rowVersion)
	buf = appendU32(buf, 0xffffffff) // far more fields than bytes available
	if _, err := decodeRow(buf); err == nil {
		t.Fatalf("decodeRow with impossible field count succeeded, want error")
	}
}

func TestDecodeMalformedTaggedBinaryNotReinterpretedAsJSON(t *testing.T) {
	// Bytes carrying the magic prefix must never fall back to the JSON path,
	// even if the tail happens to look JSON-ish.
	corrupted := append([]byte(nil), rowMagic...)
	corrupted = append(corrupted, rowVersion)
	corrupted = append(corrupted, 0xff, 0xff, 0xff, 0xff) // bogus count
	corrupted = append(corrupted, 'g', 'a', 'r', 'b', 'a', 'g', 'e')

	if _, err := decodeRow(corrupted); err == nil {
		t.Fatalf("decodeRow succeeded on malformed tagged binary, want error")
	}
}

func TestDecodeNonJSONNonBinaryInput(t *testing.T) {
	// No magic prefix and not valid JSON must fail rather than panic or return
	// a partial row.
	if _, err := decodeRow([]byte{0x01, 0x02, 0x03, 0x04}); err == nil {
		t.Fatalf("decodeRow on garbage succeeded, want error")
	}
}

func TestEncodeDecodeEmptyRow(t *testing.T) {
	data, err := encodeRow(Row{})
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	got, err := decodeRow(data)
	if err != nil {
		t.Fatalf("decodeRow: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty row decoded to %#v", got)
	}
}

func TestEncodeDecodeNilRow(t *testing.T) {
	data, err := encodeRow(nil)
	if err != nil {
		t.Fatalf("encodeRow(nil): %v", err)
	}
	got, err := decodeRow(data)
	if err != nil {
		t.Fatalf("decodeRow: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("nil row decoded to %#v", got)
	}
}

func TestEncodeRowSortedFieldNames(t *testing.T) {
	data, err := encodeRow(Row{"z": int64(3), "a": int64(1), "m": int64(2)})
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	// Verify the field names appear in sorted order by walking the encoding.
	pos := rowHeaderLen
	count := binary.LittleEndian.Uint32(data[pos-4 : pos])
	names := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		nameLen := binary.LittleEndian.Uint32(data[pos : pos+4])
		pos += 4
		names = append(names, string(data[pos:pos+int(nameLen)]))
		pos += int(nameLen)
		pos++ // skip tag
		switch data[pos-1] {
		case tagInt, tagUint, tagFloat64:
			pos += 8
		case tagFloat32:
			pos += 4
		case tagString, tagBytes, tagNumber:
			l := binary.LittleEndian.Uint32(data[pos : pos+4])
			pos += 4 + int(l)
		}
	}
	if !reflect.DeepEqual(names, []string{"a", "m", "z"}) {
		t.Fatalf("field names = %v, want [a m z]", names)
	}
}

func BenchmarkRowCodec(b *testing.B) {
	row := Row{
		"_rowid_": int64(4812),
		"id":      int64(4812),
		"symbol":  "PIZZA",
		"price":   104.25,
		"active":  true,
		"payload": []byte{0, 1, 2, '|', '\r', '\n'},
	}
	binaryRow, err := encodeRow(row)
	if err != nil {
		b.Fatal(err)
	}
	jsonRow, err := json.Marshal(row)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("encode_binary", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := encodeRow(row); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("encode_json", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := json.Marshal(row); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("decode_binary", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := decodeRow(binaryRow); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("decode_json", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := decodeRow(jsonRow); err != nil {
				b.Fatal(err)
			}
		}
	})
}
