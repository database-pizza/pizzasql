package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/goccy/go-json"
)

// rowMagic prefixes every versioned binary row value. It is chosen so it can
// never be the first bytes of a legacy JSON row (which always begins with '{',
// '[', '"', a digit, 't', 'f', or 'n'), so decodeRow can disambiguate the two
// encodings unambiguously.
const rowMagic = "PZSQLROW"

// rowVersion is the format version. It must be bumped whenever the binary
// layout changes in a way that would make old bytes undecodable.
const rowVersion = 1

// rowHeaderLen is the fixed size of the binary header: magic + version + count.
const rowHeaderLen = len(rowMagic) + 1 + 4

// maxRowFieldLen caps the encoded length of a field name or a variable-length
// value (string, bytes, json.Number). It is far larger than any value the KV
// layer can return (64 MiB), so it only ever rejects adversarial lengths.
const maxRowFieldLen = 1 << 30

// minFieldEncodedSize is the smallest possible on-disk size of a single field:
// a 4-byte name length, an empty name, and a 1-byte type tag.
const minFieldEncodedSize = 4 + 1

// Value type tags. A tag occupies one byte and precedes the value payload.
const (
	tagNil     = 0x00
	tagFalse   = 0x01
	tagTrue    = 0x02
	tagInt     = 0x03 // signed integer, normalized to int64
	tagUint    = 0x04 // unsigned integer, normalized to uint64
	tagFloat32 = 0x05
	tagFloat64 = 0x06
	tagString  = 0x07
	tagBytes   = 0x08
	tagNumber  = 0x09 // json.Number, preserved verbatim as decimal bytes
)

var errMalformedRow = errors.New("malformed row encoding")

// encodeRow serializes a row into a deterministic, compact, versioned binary
// value. Field names are sorted so identical rows always encode to identical
// bytes. If any value cannot be represented exactly in the binary format
// (e.g. a slice, map, struct, or time), the entire row is encoded as legacy
// JSON instead so no data is lost.
func encodeRow(row Row) ([]byte, error) {
	if len(row) > math.MaxUint32 {
		return nil, fmt.Errorf("row has too many fields")
	}
	names := make([]string, 0, len(row))
	for name := range row {
		if len(name) > maxRowFieldLen {
			return nil, fmt.Errorf("row field name is too long")
		}
		names = append(names, name)
	}
	sort.Strings(names)

	// Encode values first; fall back to JSON if any is unrepresentable.
	encoded := make([][]byte, len(names))
	for i, name := range names {
		enc, ok := encodeValue(row[name])
		if !ok {
			return json.Marshal(row)
		}
		encoded[i] = enc
	}

	buf := make([]byte, 0, rowHeaderLen+len(names)*8)
	buf = append(buf, rowMagic...)
	buf = append(buf, rowVersion)
	buf = appendU32(buf, uint32(len(names)))
	for i, name := range names {
		buf = appendU32(buf, uint32(len(name)))
		buf = append(buf, name...)
		buf = append(buf, encoded[i]...)
	}
	return buf, nil
}

// encodeValue returns the type tag plus payload for v, and reports whether v
// can be represented exactly. All Go integer widths are normalized to their
// fixed-width equivalents; every other supported type is self-describing.
func encodeValue(v interface{}) ([]byte, bool) {
	switch t := v.(type) {
	case nil:
		return []byte{tagNil}, true
	case bool:
		if t {
			return []byte{tagTrue}, true
		}
		return []byte{tagFalse}, true
	case int:
		return appendU64([]byte{tagInt}, uint64(int64(t))), true
	case int8:
		return appendU64([]byte{tagInt}, uint64(int64(t))), true
	case int16:
		return appendU64([]byte{tagInt}, uint64(int64(t))), true
	case int32:
		return appendU64([]byte{tagInt}, uint64(int64(t))), true
	case int64:
		return appendU64([]byte{tagInt}, uint64(t)), true
	case uint:
		return appendU64([]byte{tagUint}, uint64(t)), true
	case uint8:
		return appendU64([]byte{tagUint}, uint64(t)), true
	case uint16:
		return appendU64([]byte{tagUint}, uint64(t)), true
	case uint32:
		return appendU64([]byte{tagUint}, uint64(t)), true
	case uint64:
		return appendU64([]byte{tagUint}, t), true
	case uintptr:
		return appendU64([]byte{tagUint}, uint64(t)), true
	case float32:
		var b [5]byte
		b[0] = tagFloat32
		binary.LittleEndian.PutUint32(b[1:], math.Float32bits(t))
		return b[:], true
	case float64:
		var b [9]byte
		b[0] = tagFloat64
		binary.LittleEndian.PutUint64(b[1:], math.Float64bits(t))
		return b[:], true
	case string:
		if len(t) > maxRowFieldLen {
			return nil, false
		}
		return appendBytesField([]byte{tagString}, []byte(t)), true
	case []byte:
		if len(t) > maxRowFieldLen {
			return nil, false
		}
		return appendBytesField([]byte{tagBytes}, t), true
	case json.Number:
		if len(t) > maxRowFieldLen {
			return nil, false
		}
		return appendBytesField([]byte{tagNumber}, []byte(string(t))), true
	default:
		return nil, false
	}
}

// decodeRow decodes a row value in either the versioned binary format or the
// legacy untagged JSON format. The two are distinguished solely by the magic
// prefix: bytes carrying the magic are always parsed as binary and never fall
// back to JSON, while anything else is parsed as legacy JSON for backward
// compatibility.
func decodeRow(data []byte) (Row, error) {
	if len(data) >= len(rowMagic) && string(data[:len(rowMagic)]) == rowMagic {
		return decodeBinaryRow(data)
	}

	var row Row
	if err := json.Unmarshal(data, &row); err != nil {
		return nil, err
	}
	return row, nil
}

// decodeBinaryRow parses a versioned binary row, validating the magic,
// version, field count, per-field length bounds, and that no trailing bytes
// remain once every field has been consumed.
func decodeBinaryRow(data []byte) (Row, error) {
	if len(data) < rowHeaderLen {
		return nil, errMalformedRow
	}
	if string(data[:len(rowMagic)]) != rowMagic {
		return nil, errMalformedRow
	}
	version := data[len(rowMagic)]
	if version != rowVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", errMalformedRow, version)
	}

	count := binary.LittleEndian.Uint32(data[len(rowMagic)+1 : len(rowMagic)+5])
	pos := rowHeaderLen
	remaining := len(data) - pos

	// Reject impossible field counts up front so a hostile count cannot drive
	// an unbounded loop: every field occupies at least minFieldEncodedSize.
	if uint64(count)*minFieldEncodedSize > uint64(remaining) {
		return nil, errMalformedRow
	}

	row := make(Row, count)
	for i := uint32(0); i < count; i++ {
		if remaining < 4 {
			return nil, errMalformedRow
		}
		nameLen := binary.LittleEndian.Uint32(data[pos : pos+4])
		pos += 4
		remaining -= 4
		if nameLen > maxRowFieldLen || uint64(nameLen) > uint64(remaining) {
			return nil, errMalformedRow
		}
		name := string(data[pos : pos+int(nameLen)])
		pos += int(nameLen)
		remaining -= int(nameLen)

		if remaining < 1 {
			return nil, errMalformedRow
		}
		tag := data[pos]
		pos++
		remaining--

		value, n, err := decodeValue(tag, data[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		remaining -= n
		row[name] = value
	}

	if pos != len(data) {
		return nil, errMalformedRow
	}
	return row, nil
}

// decodeValue decodes a single tagged value from data, returning the value and
// the number of payload bytes consumed. Variable-length payloads are validated
// against both the global cap and the actual remaining input.
func decodeValue(tag byte, data []byte) (interface{}, int, error) {
	switch tag {
	case tagNil:
		return nil, 0, nil
	case tagFalse:
		return false, 0, nil
	case tagTrue:
		return true, 0, nil
	case tagInt:
		if len(data) < 8 {
			return nil, 0, errMalformedRow
		}
		return int64(binary.LittleEndian.Uint64(data[:8])), 8, nil
	case tagUint:
		if len(data) < 8 {
			return nil, 0, errMalformedRow
		}
		return binary.LittleEndian.Uint64(data[:8]), 8, nil
	case tagFloat32:
		if len(data) < 4 {
			return nil, 0, errMalformedRow
		}
		return math.Float32frombits(binary.LittleEndian.Uint32(data[:4])), 4, nil
	case tagFloat64:
		if len(data) < 8 {
			return nil, 0, errMalformedRow
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(data[:8])), 8, nil
	case tagString, tagBytes, tagNumber:
		if len(data) < 4 {
			return nil, 0, errMalformedRow
		}
		l := binary.LittleEndian.Uint32(data[:4])
		if l > maxRowFieldLen || uint64(l) > uint64(len(data)-4) {
			return nil, 0, errMalformedRow
		}
		content := data[4 : 4+int(l)]
		switch tag {
		case tagString:
			return string(content), 4 + int(l), nil
		case tagBytes:
			return append([]byte(nil), content...), 4 + int(l), nil
		case tagNumber:
			return json.Number(string(content)), 4 + int(l), nil
		}
	}
	return nil, 0, fmt.Errorf("%w: unknown tag %d", errMalformedRow, tag)
}

func appendU32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

func appendU64(dst []byte, v uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return append(dst, b[:]...)
}

func appendBytesField(dst []byte, data []byte) []byte {
	dst = appendU32(dst, uint32(len(data)))
	return append(dst, data...)
}
