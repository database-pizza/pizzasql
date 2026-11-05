package pgserver

import (
	"encoding/binary"
	"math"
	"testing"
)

// encodeBinaryArray builds a PostgreSQL binary array parameter. A nil elements
// slice is the empty array (ndim 0); a nil element is SQL NULL.
func encodeBinaryArray(elementOID uint32, elements [][]byte) []byte {
	hasNull := false
	for _, element := range elements {
		if element == nil {
			hasNull = true
		}
	}
	var buf []byte
	if elements == nil {
		buf = binary.BigEndian.AppendUint32(buf, 0)
		buf = binary.BigEndian.AppendUint32(buf, 0)
		buf = binary.BigEndian.AppendUint32(buf, elementOID)
		return buf
	}
	buf = binary.BigEndian.AppendUint32(buf, 1)
	flags := uint32(0)
	if hasNull {
		flags = 1
	}
	buf = binary.BigEndian.AppendUint32(buf, flags)
	buf = binary.BigEndian.AppendUint32(buf, elementOID)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(elements)))
	buf = binary.BigEndian.AppendUint32(buf, 1)
	for _, element := range elements {
		if element == nil {
			buf = binary.BigEndian.AppendUint32(buf, 0xFFFFFFFF)
			continue
		}
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(element)))
		buf = append(buf, element...)
	}
	return buf
}

func encodeInt2(value int16) []byte {
	return binary.BigEndian.AppendUint16(nil, uint16(value))
}

func encodeInt4(value int32) []byte {
	return binary.BigEndian.AppendUint32(nil, uint32(value))
}

func encodeInt8(value int64) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(value))
}

func encodeFloat4(value float32) []byte {
	return binary.BigEndian.AppendUint32(nil, math.Float32bits(value))
}

func encodeFloat8(value float64) []byte {
	return binary.BigEndian.AppendUint64(nil, math.Float64bits(value))
}

// encodeNumeric mirrors PostgreSQL's binary numeric layout: digit count,
// base-10000 weight, sign, display scale, then base-10000 digits.
func encodeNumeric(sign uint16, weight int16, scale uint16, digits ...uint16) []byte {
	buf := binary.BigEndian.AppendUint16(nil, uint16(len(digits)))
	buf = binary.BigEndian.AppendUint16(buf, uint16(weight))
	buf = binary.BigEndian.AppendUint16(buf, sign)
	buf = binary.BigEndian.AppendUint16(buf, scale)
	for _, digit := range digits {
		buf = binary.BigEndian.AppendUint16(buf, digit)
	}
	return buf
}

// runBinaryParameter drives one extended-protocol cycle with a single
// binary-format parameter.
func runBinaryParameter(t *testing.T, c *Connection, conn *countingConn, sql string, oid int32, value []byte) []*Message {
	t.Helper()
	conn.reset()
	parse := NewMessageBuilder()
	parse.WriteString("s1")
	parse.WriteString(sql)
	parse.WriteInt16(1)
	parse.WriteInt32(oid)

	bind := NewMessageBuilder()
	bind.WriteString("p1")
	bind.WriteString("s1")
	bind.WriteInt16(1)
	bind.WriteInt16(1) // binary parameter format
	bind.WriteInt16(1)
	bind.WriteInt32(int32(len(value)))
	bind.WriteBytes(value)
	bind.WriteInt16(0)

	execute := NewMessageBuilder()
	execute.WriteString("p1")
	execute.WriteInt32(0)

	for _, msg := range []*Message{
		{Type: MsgParse, Data: parse.Bytes()},
		{Type: MsgBind, Data: bind.Bytes()},
		{Type: MsgExecute, Data: execute.Bytes()},
		{Type: MsgSync},
	} {
		if err := c.handleMessage(msg); err != nil {
			t.Fatalf("handle %c: %v", msg.Type, err)
		}
	}
	return readAllMessages(t, conn.bytes())
}

func TestBinaryArrayParameterElements(t *testing.T) {
	c, conn := arrayConnection(t)
	cases := []struct {
		name     string
		sql      string
		arrayOID int32
		value    []byte
		want     string
	}{
		{
			name: "int4", sql: `SELECT $1::INTEGER[]`, arrayOID: 1007,
			value: encodeBinaryArray(23, [][]byte{encodeInt4(1), encodeInt4(-2)}), want: "{1,-2}",
		},
		{
			name: "int4_null_element", sql: `SELECT $1::INTEGER[]`, arrayOID: 1007,
			value: encodeBinaryArray(23, [][]byte{encodeInt4(1), nil, encodeInt4(3)}), want: "{1,NULL,3}",
		},
		{
			name: "int4_empty", sql: `SELECT $1::INTEGER[]`, arrayOID: 1007,
			value: encodeBinaryArray(23, nil), want: "{}",
		},
		{
			name: "int2", sql: `SELECT $1::SMALLINT[]`, arrayOID: 1005,
			value: encodeBinaryArray(21, [][]byte{encodeInt2(1), encodeInt2(-2)}), want: "{1,-2}",
		},
		{
			name: "int8_exact", sql: `SELECT $1::BIGINT[]`, arrayOID: 1016,
			value: encodeBinaryArray(20, [][]byte{encodeInt8(9007199254740993)}), want: "{9007199254740993}",
		},
		{
			name: "bool", sql: `SELECT $1::BOOLEAN[]`, arrayOID: 1000,
			value: encodeBinaryArray(16, [][]byte{{0}, {1}}), want: "{f,t}",
		},
		{
			name: "float4", sql: `SELECT $1::REAL[]`, arrayOID: 1021,
			value: encodeBinaryArray(700, [][]byte{encodeFloat4(1.5)}), want: "{1.5}",
		},
		{
			name: "float8", sql: `SELECT $1::DOUBLE PRECISION[]`, arrayOID: 1022,
			value: encodeBinaryArray(701, [][]byte{encodeFloat8(0.1)}), want: "{0.1}",
		},
		{
			name: "text", sql: `SELECT $1::TEXT[]`, arrayOID: 1009,
			value: encodeBinaryArray(25, [][]byte{
				[]byte("a,b"), []byte("NULL"), []byte(""), []byte(`q"t`), []byte(`back\slash`),
			}),
			want: `{"a,b","NULL","","q\"t","back\\slash"}`,
		},
		{
			name: "varchar", sql: `SELECT $1::VARCHAR(10)[]`, arrayOID: 1015,
			value: encodeBinaryArray(1043, [][]byte{[]byte("x"), []byte("y z")}), want: `{x,"y z"}`,
		},
		{
			name: "numeric_exact", sql: `SELECT $1::NUMERIC[]`, arrayOID: 1231,
			value: encodeBinaryArray(1700, [][]byte{
				encodeNumeric(0x0000, 4, 5, 1234, 5678, 9012, 3456, 7890, 1234, 5000),
				encodeNumeric(0x4000, 0, 2, 1, 5000),
			}),
			want: "{12345678901234567890.12345,-1.50}",
		},
		{
			name: "numeric_zero", sql: `SELECT $1::NUMERIC[]`, arrayOID: 1231,
			value: encodeBinaryArray(1700, [][]byte{
				encodeNumeric(0x0000, 0, 2),
				encodeNumeric(0x0000, -1, 1, 5000),
			}),
			want: "{0.00,0.5}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			messages := runBinaryParameter(t, c, conn, tc.sql, tc.arrayOID, tc.value)
			if errMsg := findError(messages); errMsg != nil {
				t.Fatalf("%s failed: %s (%s)", tc.sql, errorCode(errMsg), errorMessage(errMsg))
			}
			row := firstDataRow(t, messages)
			if len(row) != 1 || row[0] != tc.want {
				t.Fatalf("%s = %v, want [%s]", tc.sql, row, tc.want)
			}
		})
	}
}

func TestBinaryArrayParameterMalformed(t *testing.T) {
	c, conn := arrayConnection(t)
	valid := encodeBinaryArray(23, [][]byte{encodeInt4(1)})
	nullWithoutFlag := encodeBinaryArray(23, [][]byte{nil, encodeInt4(1)})
	if nullWithoutFlag[7] != 1 {
		t.Fatal("test fixture expected the null flag set")
	}
	nullWithoutFlag[7] = 0 // clear hasNull while leaving the NULL element
	cases := []struct {
		name  string
		value []byte
	}{
		{"truncated_header", valid[:10]},
		{"truncated_dimension", valid[:14]},
		{"truncated_elements", valid[:len(valid)-2]},
		{"trailing_bytes", append(append([]byte(nil), valid...), 0)},
		{"multidimensional", encodeBinaryArray(23, [][]byte{encodeInt4(1)})},
		{"element_oid_mismatch", encodeBinaryArray(25, [][]byte{[]byte("x")})},
		{"null_without_flag", nullWithoutFlag},
		{"invalid_boolean", encodeBinaryArray(16, [][]byte{{2}})},
		{"short_int4", encodeBinaryArray(23, [][]byte{{0, 0}})},
	}
	// ndim = 2 is rejected before any dimension header is read.
	multiDimensional := encodeBinaryArray(23, [][]byte{encodeInt4(1)})
	binary.BigEndian.PutUint32(multiDimensional[0:4], 2)
	cases[4].value = multiDimensional

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			messages := runBinaryParameter(t, c, conn, `SELECT $1::INTEGER[]`, 1007, tc.value)
			errMsg := findError(messages)
			if errMsg == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if code := errorCode(errMsg); code != ErrCodeInvalidText {
				t.Fatalf("%s SQLSTATE = %q, want %q", tc.name, code, ErrCodeInvalidText)
			}
		})
	}
}

// Array types the engine does not decode in binary form keep the explicit
// feature-not-supported error.
func TestBinaryArrayParameterUnsupportedElementType(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, oid := range []int32{1014 /* bytea[] */, 1182 /* date[] */} {
		messages := runBinaryParameter(t, c, conn, `SELECT $1`, oid, []byte{0, 0, 0, 0})
		errMsg := findError(messages)
		if errMsg == nil {
			t.Fatalf("binary array OID %d was accepted", oid)
		}
		if code := errorCode(errMsg); code != ErrCodeFeatureNotSupported {
			t.Fatalf("binary array OID %d SQLSTATE = %q, want %q", oid, code, ErrCodeFeatureNotSupported)
		}
	}
}

// Scalar float parameters follow the same negotiated type: pgx sends float4 and
// float8 in binary form.
func TestBinaryFloatParameter(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, tc := range []struct {
		name  string
		sql   string
		oid   int32
		value []byte
		want  string
	}{
		{"float4", `SELECT $1::REAL`, 700, encodeFloat4(1.5), "1.5"},
		{"float8", `SELECT $1::DOUBLE PRECISION`, 701, encodeFloat8(0.1), "0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := runBinaryParameter(t, c, conn, tc.sql, tc.oid, tc.value)
			if errMsg := findError(messages); errMsg != nil {
				t.Fatalf("%s failed: %s", tc.sql, errorCode(errMsg))
			}
			if row := firstDataRow(t, messages); len(row) != 1 || row[0] != tc.want {
				t.Fatalf("%s = %v, want [%s]", tc.sql, row, tc.want)
			}
		})
	}
}

// Scalar binary numeric parameters keep their exact decimal digits. The
// capability document advertises binary numeric support, and NUMERIC elements
// use the same decoder inside arrays.
func TestBinaryNumericParameter(t *testing.T) {
	c, conn := arrayConnection(t)
	value := encodeNumeric(0x0000, 1, 1, 1, 2345, 5000)
	messages := runBinaryParameter(t, c, conn, `SELECT $1::NUMERIC`, 1700, value)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("binary numeric failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "12345.5" {
		t.Fatalf("binary numeric = %v, want [12345.5]", row)
	}

	messages = runBinaryParameter(t, c, conn, `SELECT $1::NUMERIC`, 1700, encodeNumeric(0x4000, -1, 1, 5000))
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("negative binary numeric failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "-0.5" {
		t.Fatalf("negative binary numeric = %v, want [-0.5]", row)
	}
}
