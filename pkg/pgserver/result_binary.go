package pgserver

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
)

// Binary result encoding.
//
// DataRow values are normally sent in the PostgreSQL text format. A client that
// asks for a binary result column (Bind result format code 1) gets the same
// value in PostgreSQL's binary representation for the OID RowDescription
// reports. Values are appended directly into the DataRow buffer, and a value
// that cannot be represented fails the row before it is written.
//
// The supported OIDs are the ones this engine reports (see getOIDForType) plus
// the array element types that appear in binary array headers. Types without a
// binary representation here (timestamptz 1184, date 1082) are rejected at
// execution time with 0A000 rather than being sent as text.

// canEncodeBinaryResult reports whether the wire layer can encode a result
// column of this OID in the binary format.
func canEncodeBinaryResult(oid int32) bool {
	switch oid {
	case 16, // bool
		17,   // bytea
		20,   // int8
		21,   // int2
		23,   // int4
		25,   // text
		700,  // float4
		701,  // float8
		1043, // varchar
		1700, // numeric
		3802: // jsonb
		return true
	}
	_, ok := binaryResultElementOID(oid)
	return ok
}

// binaryResultElementOID maps a one-dimensional array OID to the element OID its
// binary envelope must carry.
func binaryResultElementOID(arrayOID int32) (int32, bool) {
	switch arrayOID {
	case 1000: // bool[]
		return 16, true
	case 1005: // int2[]
		return 21, true
	case 1007: // int4[]
		return 23, true
	case 1009: // text[]
		return 25, true
	case 1015: // varchar[]
		return 1043, true
	case 1016: // int8[]
		return 20, true
	case 1021: // float4[]
		return 700, true
	case 1022: // float8[]
		return 701, true
	case 1231: // numeric[]
		return 1700, true
	default:
		return 0, false
	}
}

// binaryResultEncodeError reports a value the requested binary format cannot
// represent, such as text left in an INTEGER column by SQLite affinity. The
// connection reports it as invalid text input and sends no further rows.
type binaryResultEncodeError struct {
	err error
}

func (e *binaryResultEncodeError) Error() string { return e.err.Error() }
func (e *binaryResultEncodeError) Unwrap() error { return e.err }

func binaryEncodeErrorf(format string, args ...interface{}) error {
	return &binaryResultEncodeError{err: fmt.Errorf(format, args...)}
}

// failBinaryEncode reports a value the requested binary format cannot represent.
// Rows already written stay written (an error discards the rest of the result),
// but the failing DataRow is never emitted.
func (c *Connection) failBinaryEncode(err error) error {
	var encodeErr *binaryResultEncodeError
	if errors.As(err, &encodeErr) {
		return c.failExtended(ErrCodeInvalidText, err)
	}
	return err
}

// appendBinaryResultValue appends the PostgreSQL binary representation of value
// for oid.
func appendBinaryResultValue(dst []byte, oid int32, value interface{}) ([]byte, error) {
	if elementOID, ok := binaryResultElementOID(oid); ok {
		return appendBinaryArray(dst, elementOID, value)
	}
	switch oid {
	case 16: // bool
		flag, err := binaryBool(value)
		if err != nil {
			return dst, err
		}
		if flag {
			return append(dst, 1), nil
		}
		return append(dst, 0), nil
	case 17: // bytea
		return append(dst, binaryBytes(value)...), nil
	case 20, 21, 23: // int8, int2, int4
		width := 8
		switch oid {
		case 21:
			width = 2
		case 23:
			width = 4
		}
		n, err := binaryInt64(value, oid)
		if err != nil {
			return dst, err
		}
		switch width {
		case 2:
			return append(dst, byte(n>>8), byte(n)), nil
		case 4:
			return append(dst, byte(n>>24), byte(n>>16), byte(n>>8), byte(n)), nil
		default:
			return appendUint64(dst, uint64(n)), nil
		}
	case 25, 1043: // text, varchar
		return append(dst, textValueString(value)...), nil
	case 700: // float4
		f, err := binaryFloat64(value)
		if err != nil {
			return dst, err
		}
		// A value beyond float4's range would otherwise convert to ±Inf and be
		// decoded as infinity instead of the stored value. NaN compares false
		// here and stays NaN, which float4 can represent.
		if math.Abs(f) > math.MaxFloat32 {
			return dst, binaryEncodeErrorf("value %v does not fit float4", f)
		}
		bits := math.Float32bits(float32(f))
		return append(dst, byte(bits>>24), byte(bits>>16), byte(bits>>8), byte(bits)), nil
	case 701: // float8
		f, err := binaryFloat64(value)
		if err != nil {
			return dst, err
		}
		return appendUint64(dst, math.Float64bits(f)), nil
	case 1700: // numeric
		return appendNumericValue(dst, value)
	case 3802: // jsonb: version byte plus canonical JSON text
		dst = append(dst, 1)
		return append(dst, binaryJSONText(value)...), nil
	default:
		return dst, binaryEncodeErrorf("binary result format is not supported for OID %d", oid)
	}
}

// appendBinaryArray appends a one-dimensional binary array. The stored value is
// the canonical PostgreSQL array text, so parsing it yields the same elements
// the text format carries; NULL elements keep their -1 length and an empty
// array has no dimension header, as array_recv expects.
func appendBinaryArray(dst []byte, elementOID int32, value interface{}) ([]byte, error) {
	text := binaryJSONText(value)
	elements, err := executor.ParseArrayText(text)
	if err != nil {
		return dst, binaryEncodeErrorf("invalid array value %q: %v", text, err)
	}
	hasNull := false
	for _, element := range elements {
		if element == nil {
			hasNull = true
			break
		}
	}
	dimensions := int32(1)
	if len(elements) == 0 {
		dimensions = 0
	}
	dst = appendUint32(dst, uint32(dimensions))
	if hasNull {
		dst = appendUint32(dst, 1)
	} else {
		dst = appendUint32(dst, 0)
	}
	dst = appendUint32(dst, uint32(elementOID))
	if dimensions == 0 {
		return dst, nil
	}
	dst = appendUint32(dst, uint32(len(elements)))
	dst = appendUint32(dst, 1) // lower bound
	for _, element := range elements {
		if element == nil {
			dst = appendUint32(dst, math.MaxUint32) // -1: NULL element
			continue
		}
		start := len(dst)
		dst = appendUint32(dst, 0)
		dst, err = appendBinaryResultValue(dst, elementOID, *element)
		if err != nil {
			return dst, err
		}
		binary.BigEndian.PutUint32(dst[start:start+4], uint32(len(dst)-start-4))
	}
	return dst, nil
}

func appendUint32(dst []byte, n uint32) []byte {
	return append(dst, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

func appendUint64(dst []byte, n uint64) []byte {
	return append(dst, byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32), byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// binaryInt64 converts an engine value to an integer result of the given OID.
// Text is accepted because SQLite affinity can leave a text value in an integer
// column; a value that does not fit the reported OID is an encoding error
// instead of a silently truncated one.
func binaryInt64(value interface{}, oid int32) (int64, error) {
	var n int64
	switch v := value.(type) {
	case int64:
		n = v
	case int:
		n = int64(v)
	case int32:
		n = int64(v)
	case int16:
		n = int64(v)
	case int8:
		n = int64(v)
	case float64:
		if math.Trunc(v) != v || v < math.MinInt64 || v >= math.MaxInt64 {
			return 0, binaryEncodeErrorf("value %v is not an integer", v)
		}
		n = int64(v)
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, binaryEncodeErrorf("value %q is not an integer", v)
		}
		n = parsed
	case []byte:
		parsed, err := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
		if err != nil {
			return 0, binaryEncodeErrorf("value %q is not an integer", v)
		}
		n = parsed
	default:
		return 0, binaryEncodeErrorf("value of type %T is not an integer", value)
	}
	switch oid {
	case 21:
		if n < math.MinInt16 || n > math.MaxInt16 {
			return 0, binaryEncodeErrorf("value %d does not fit int2", n)
		}
	case 23:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return 0, binaryEncodeErrorf("value %d does not fit int4", n)
		}
	}
	return n, nil
}

// binaryFloat64 converts an engine value to a floating-point result.
func binaryFloat64(value interface{}) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int16:
		return float64(v), nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, binaryEncodeErrorf("value %q is not a floating-point number", v)
		}
		return parsed, nil
	case []byte:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
		if err != nil {
			return 0, binaryEncodeErrorf("value %q is not a floating-point number", v)
		}
		return parsed, nil
	default:
		return 0, binaryEncodeErrorf("value of type %T is not a floating-point number", value)
	}
}

// binaryBool converts an engine value to a boolean result. Boolean columns hold
// int64 0/1 from storage, bool from expressions, and text when the inserted
// literal did not convert.
func binaryBool(value interface{}) (bool, error) {
	switch v := value.(type) {
	case bool:
		return v, nil
	case int64:
		return v != 0, nil
	case int:
		return v != 0, nil
	case int32:
		return v != 0, nil
	case int16:
		return v != 0, nil
	case float64:
		return v != 0, nil
	case string:
		return boolText(v)
	case []byte:
		return boolText(string(v))
	default:
		return false, binaryEncodeErrorf("value of type %T is not a boolean", value)
	}
}

func boolText(text string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "t", "true", "1", "y", "yes", "on":
		return true, nil
	case "f", "false", "0", "n", "no", "off":
		return false, nil
	default:
		return false, binaryEncodeErrorf("value %q is not a boolean", text)
	}
}

// binaryBytes returns the bytes a binary bytea result carries. A non-[]byte
// value uses the same text the bytea text format would send, which is the
// escape-format interpretation of the value's spelling.
func binaryBytes(value interface{}) []byte {
	if b, ok := value.([]byte); ok {
		return b
	}
	return []byte(textValueString(value))
}

// binaryJSONText returns the JSON text a jsonb result carries.
func binaryJSONText(value interface{}) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return textValueString(value)
	}
}

// appendNumericValue appends a PostgreSQL binary numeric value. Numeric values
// are stored as exact decimal text, so they are encoded digit by digit into
// base-10000 groups; converting through float64 would round digits above 2^53
// and lose the display scale (1.10 would become 1.1).
func appendNumericValue(dst []byte, value interface{}) ([]byte, error) {
	var text string
	switch v := value.(type) {
	case string:
		text = v
	case []byte:
		text = string(v)
	case int64:
		text = strconv.FormatInt(v, 10)
	case int:
		text = strconv.FormatInt(int64(v), 10)
	case float64:
		text = strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return dst, binaryEncodeErrorf("value of type %T is not numeric", value)
	}
	return appendNumericText(dst, text)
}

// appendNumericText encodes exact decimal text as a PostgreSQL numeric: ndigits,
// weight, sign, dscale, then base-10000 groups. Exponents are applied to the
// decimal point, trailing fraction zeros only affect dscale, and leading zero
// groups are dropped while adjusting weight.
func appendNumericText(dst []byte, text string) ([]byte, error) {
	invalid := func() ([]byte, error) {
		return dst, binaryEncodeErrorf("invalid numeric value %q", text)
	}
	s := strings.TrimSpace(text)
	if s == "" {
		return invalid()
	}
	negative := false
	if s[0] == '+' || s[0] == '-' {
		negative = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return invalid()
	}
	exponent := 0
	if index := strings.IndexAny(s, "eE"); index >= 0 {
		parsed, err := strconv.Atoi(s[index+1:])
		if err != nil {
			return invalid()
		}
		if parsed > math.MaxInt16*4 || parsed < math.MinInt16*4 {
			return dst, binaryEncodeErrorf("numeric exponent %d is out of range", parsed)
		}
		exponent = parsed
		s = s[:index]
	}
	point := strings.IndexByte(s, '.')
	var digits string
	if point < 0 {
		point = len(s)
		digits = s
	} else {
		digits = s[:point] + s[point+1:]
	}
	if digits == "" {
		return invalid()
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return invalid()
		}
	}
	point += exponent
	if point > math.MaxInt16*4 || point < math.MinInt16*4 {
		return dst, binaryEncodeErrorf("numeric exponent is out of range in %q", text)
	}

	// The display scale is the number of fraction digits after the exponent,
	// including zeros this encoder drops from the base-10000 groups (1.10 keeps
	// dscale 2).
	dscale := len(digits) - point
	if dscale < 0 {
		dscale = 0
	}
	if dscale > math.MaxInt16 {
		return dst, binaryEncodeErrorf("numeric scale %d is out of range", dscale)
	}

	integerDigits := ""
	if point >= len(digits) {
		integerDigits = digits + strings.Repeat("0", point-len(digits))
	} else if point > 0 {
		integerDigits = digits[:point]
	}
	fractionDigits := ""
	if point < 0 {
		fractionDigits = strings.Repeat("0", -point) + digits
	} else if point < len(digits) {
		fractionDigits = digits[point:]
	}

	var groups []uint16
	weight := 0
	if trimmed := strings.TrimLeft(integerDigits, "0"); trimmed != "" {
		padding := (4 - len(trimmed)%4) % 4
		padded := strings.Repeat("0", padding) + trimmed
		weight = len(padded)/4 - 1
		for i := 0; i < len(padded); i += 4 {
			groups = append(groups, uint16(decimalGroup(padded[i:i+4])))
		}
	}
	if trimmed := strings.TrimRight(fractionDigits, "0"); trimmed != "" {
		padding := (4 - len(trimmed)%4) % 4
		padded := trimmed + strings.Repeat("0", padding)
		if len(groups) == 0 {
			weight = -1
		}
		for i := 0; i < len(padded); i += 4 {
			groups = append(groups, uint16(decimalGroup(padded[i:i+4])))
		}
	}
	leading := 0
	for leading < len(groups) && groups[leading] == 0 {
		leading++
	}
	if leading == len(groups) {
		groups = nil
		weight = 0
	} else {
		groups = groups[leading:]
		weight -= leading
	}
	for len(groups) > 0 && groups[len(groups)-1] == 0 {
		groups = groups[:len(groups)-1]
	}
	if weight < math.MinInt16 || weight > math.MaxInt16 {
		return dst, binaryEncodeErrorf("numeric weight %d is out of range", weight)
	}
	if len(groups) > math.MaxInt16 {
		return dst, binaryEncodeErrorf("numeric has %d base-10000 groups, out of range", len(groups))
	}

	dst = append(dst, byte(len(groups)>>8), byte(len(groups)))
	dst = append(dst, byte(weight>>8), byte(weight))
	sign := 0
	if negative && len(groups) > 0 {
		sign = 0x4000
	}
	dst = append(dst, byte(sign>>8), byte(sign))
	dst = append(dst, byte(dscale>>8), byte(dscale))
	for _, group := range groups {
		dst = append(dst, byte(group>>8), byte(group))
	}
	return dst, nil
}

// decimalGroup parses four decimal digits, which are always in 0000-9999.
func decimalGroup(digits string) int {
	n := 0
	for i := 0; i < len(digits); i++ {
		n = n*10 + int(digits[i]-'0')
	}
	return n
}
