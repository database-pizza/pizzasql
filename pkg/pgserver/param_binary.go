package pgserver

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

// binaryArrayLiteral decodes a PostgreSQL binary array parameter into the typed
// array literal the executor consumes. Element text is re-encoded into array
// input text and canonicalized, so a malformed element fails at Bind with
// 22P02 exactly like a malformed text-format array.
func binaryArrayLiteral(elementType string, value []byte) (string, error) {
	text, err := binaryArrayText(elementType, value)
	if err != nil {
		return "", err
	}
	arrayType := elementType + "[]"
	canonical, err := executor.CanonicalizeArrayText(text, arrayType)
	if err != nil {
		return "", err
	}
	return "'" + strings.ReplaceAll(canonical, "'", "''") + "'::" + arrayType, nil
}

// binaryArrayText converts a binary array parameter to PostgreSQL array text.
// The envelope is ndim, hasnull, element OID, one dimension header, then
// length-prefixed elements; every read is bounds-checked and any structural
// problem is reported as invalid text input rather than a partial decode.
func binaryArrayText(elementType string, value []byte) (string, error) {
	if len(value) < 12 {
		return "", pgerror.InvalidText("binary array parameter is truncated")
	}
	dimensions := int32(binary.BigEndian.Uint32(value[0:4]))
	hasNull := int32(binary.BigEndian.Uint32(value[4:8]))
	elementOID := binary.BigEndian.Uint32(value[8:12])
	if dimensions < 0 || hasNull < 0 || hasNull > 1 {
		return "", pgerror.InvalidText("invalid binary array header")
	}
	if dimensions > 1 {
		// Text-format arrays reject nested braces with 0A000; binary arrays
		// report the same unsupported-feature error for parity.
		return "", fmt.Errorf("%w: multidimensional arrays are not supported", pgerror.ErrFeatureNotSupported)
	}
	expectedOID, ok := binaryArrayElementOID(elementType)
	if !ok {
		return "", fmt.Errorf("%w: binary array element type %s", pgerror.ErrFeatureNotSupported, elementType)
	}
	if elementOID != expectedOID {
		return "", pgerror.InvalidText("binary array element OID %d does not match %s[]", elementOID, elementType)
	}
	position := 12
	count := 0
	if dimensions == 1 {
		if position+8 > len(value) {
			return "", pgerror.InvalidText("binary array parameter is truncated")
		}
		length := int32(binary.BigEndian.Uint32(value[position : position+4]))
		if length < 0 {
			return "", pgerror.InvalidText("invalid binary array dimension length")
		}
		count = int(length)
		position += 8 // length and lower bound; elements are read in order
	}
	// Every element has at least its 4-byte length prefix, so a corrupt count
	// cannot drive a long loop over bytes that are not there.
	if count > (len(value)-position)/4 {
		return "", pgerror.InvalidText("binary array parameter is truncated")
	}
	var builder strings.Builder
	builder.WriteByte('{')
	for i := 0; i < count; i++ {
		if position+4 > len(value) {
			return "", pgerror.InvalidText("binary array parameter is truncated")
		}
		length := int32(binary.BigEndian.Uint32(value[position : position+4]))
		position += 4
		if i > 0 {
			builder.WriteByte(',')
		}
		if length == -1 {
			if hasNull == 0 {
				return "", pgerror.InvalidText("binary array has a NULL element without the null flag")
			}
			builder.WriteString("NULL")
			continue
		}
		if length < 0 || position+int(length) > len(value) {
			return "", pgerror.InvalidText("binary array parameter is truncated")
		}
		text, err := binaryArrayElementText(elementOID, value[position:position+int(length)])
		if err != nil {
			return "", err
		}
		position += int(length)
		builder.WriteString(arrayElementLiteral(text))
	}
	if position != len(value) {
		return "", pgerror.InvalidText("binary array parameter has trailing bytes")
	}
	builder.WriteByte('}')
	return builder.String(), nil
}

// binaryArrayElementOID reports the PostgreSQL element OID a supported array
// parameter type must carry in its binary envelope.
func binaryArrayElementOID(elementType string) (uint32, bool) {
	switch elementType {
	case "TEXT":
		return 25, true
	case "VARCHAR":
		return 1043, true
	case "INTEGER":
		return 23, true
	case "BIGINT":
		return 20, true
	case "SMALLINT":
		return 21, true
	case "BOOLEAN":
		return 16, true
	case "REAL":
		return 700, true
	case "DOUBLE":
		return 701, true
	case "NUMERIC":
		return 1700, true
	default:
		return 0, false
	}
}

// binaryArrayElementText decodes one element to its PostgreSQL text spelling.
func binaryArrayElementText(elementOID uint32, data []byte) (string, error) {
	switch elementOID {
	case 25, 1043: // text, varchar
		return string(data), nil
	case 16: // boolean
		if len(data) != 1 || data[0] > 1 {
			return "", pgerror.InvalidText("invalid binary boolean array element")
		}
		if data[0] == 0 {
			return "f", nil
		}
		return "t", nil
	case 21: // int2
		if len(data) != 2 {
			return "", pgerror.InvalidText("invalid binary int2 array element")
		}
		return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(data))), 10), nil
	case 23: // int4
		if len(data) != 4 {
			return "", pgerror.InvalidText("invalid binary int4 array element")
		}
		return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(data))), 10), nil
	case 20: // int8
		if len(data) != 8 {
			return "", pgerror.InvalidText("invalid binary int8 array element")
		}
		return strconv.FormatInt(int64(binary.BigEndian.Uint64(data)), 10), nil
	case 700: // float4
		if len(data) != 4 {
			return "", pgerror.InvalidText("invalid binary float4 array element")
		}
		return strconv.FormatFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(data))), 'g', -1, 32), nil
	case 701: // float8
		if len(data) != 8 {
			return "", pgerror.InvalidText("invalid binary float8 array element")
		}
		return strconv.FormatFloat(math.Float64frombits(binary.BigEndian.Uint64(data)), 'g', -1, 64), nil
	case 1700: // numeric
		return binaryNumericText(data)
	default:
		return "", fmt.Errorf("%w: binary array element OID %d", pgerror.ErrFeatureNotSupported, elementOID)
	}
}

// arrayElementLiteral quotes one element for array input text. The result is
// still validated by CanonicalizeArrayText, so this only has to be parseable.
func arrayElementLiteral(text string) string {
	needsQuote := text == "" || strings.EqualFold(text, "NULL")
	for i := 0; i < len(text) && !needsQuote; i++ {
		switch text[i] {
		case '{', '}', ',', '"', '\\', ' ', '\t', '\n', '\r':
			needsQuote = true
		}
	}
	if !needsQuote {
		return text
	}
	var builder strings.Builder
	builder.WriteByte('"')
	for i := 0; i < len(text); i++ {
		if text[i] == '"' || text[i] == '\\' {
			builder.WriteByte('\\')
		}
		builder.WriteByte(text[i])
	}
	builder.WriteByte('"')
	return builder.String()
}

// binaryNumericText converts PostgreSQL's binary numeric representation to its
// exact decimal text. NUMERIC elements stay text in the engine, so the digits
// must not pass through float64: the representation is base-10000 digits, a
// base-10000 weight, a sign, and a display scale.
func binaryNumericText(data []byte) (string, error) {
	if len(data) < 8 {
		return "", pgerror.InvalidText("binary numeric parameter is truncated")
	}
	ndigits := int(binary.BigEndian.Uint16(data[0:2]))
	weight := int(int16(binary.BigEndian.Uint16(data[2:4])))
	sign := binary.BigEndian.Uint16(data[4:6])
	scale := int(binary.BigEndian.Uint16(data[6:8]))
	if len(data) != 8+ndigits*2 {
		return "", pgerror.InvalidText("binary numeric parameter has an inconsistent digit count")
	}
	if scale > 16383 {
		return "", pgerror.InvalidText("binary numeric parameter has an unsupported scale")
	}
	digits := make([]int, ndigits)
	for i := range digits {
		digits[i] = int(binary.BigEndian.Uint16(data[8+i*2:]))
		if digits[i] > 9999 {
			return "", pgerror.InvalidText("binary numeric parameter has an invalid digit")
		}
	}
	switch sign {
	case 0x0000, 0x4000:
	case 0xC000, 0xD000, 0xF000:
		// The engine has no numeric representation for NaN or infinities;
		// splicing the bare word into SQL would fail later as an unknown
		// column, so reject the value as invalid text input here.
		return "", pgerror.InvalidText("binary numeric parameter has an unsupported special value")
	default:
		return "", pgerror.InvalidText("binary numeric parameter has an invalid sign")
	}

	var integer strings.Builder
	if weight >= 0 {
		for position := weight; position >= 0; position-- {
			index := weight - position
			digit := 0
			if index < ndigits {
				digit = digits[index]
			}
			if position == weight {
				integer.WriteString(strconv.Itoa(digit))
			} else {
				fmt.Fprintf(&integer, "%04d", digit)
			}
		}
	}
	integerPart := strings.TrimLeft(integer.String(), "0")
	if integerPart == "" {
		integerPart = "0"
	}

	var fractional strings.Builder
	group := 1
	for fractional.Len() < scale {
		digit := 0
		if index := weight + group; index >= 0 && index < ndigits {
			digit = digits[index]
		}
		fmt.Fprintf(&fractional, "%04d", digit)
		group++
	}
	fractionPart := fractional.String()[:scale]

	var result strings.Builder
	if sign == 0x4000 {
		result.WriteByte('-')
	}
	result.WriteString(integerPart)
	if fractionPart != "" {
		result.WriteByte('.')
		result.WriteString(fractionPart)
	}
	return result.String(), nil
}

// binaryFloatText decodes a scalar binary float4 or float8 parameter to the
// decimal text the executor parses.
func binaryFloatText(oid int32, value []byte) (string, error) {
	switch oid {
	case 700:
		if len(value) != 4 {
			return "", pgerror.InvalidText("invalid binary float4 parameter")
		}
		return strconv.FormatFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(value))), 'g', -1, 32), nil
	case 701:
		if len(value) != 8 {
			return "", pgerror.InvalidText("invalid binary float8 parameter")
		}
		return strconv.FormatFloat(math.Float64frombits(binary.BigEndian.Uint64(value)), 'g', -1, 64), nil
	default:
		return "", pgerror.InvalidText("unsupported binary floating-point OID %d", oid)
	}
}
