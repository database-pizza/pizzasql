package pgserver

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// extendedResultConnection returns a connection whose protocol output is
// captured in memory, so Bind/Execute responses can be decoded.
func extendedResultConnection(t *testing.T) (*Connection, *countingConn) {
	t.Helper()
	c, _ := newDataConnection(t)
	conn := &countingConn{}
	c.conn = conn
	c.writer = bufio.NewWriter(conn)
	return c, conn
}

func parseMessage(name, sql string) *Message {
	mb := NewMessageBuilder()
	mb.WriteString(name)
	mb.WriteString(sql)
	mb.WriteInt16(0) // no parameter OIDs
	return &Message{Type: MsgParse, Data: mb.Bytes()}
}

// bindResultMessage binds a portal with text parameters and the given result
// format list.
func bindResultMessage(portal, statement string, params []string, resultFormats []int16) *Message {
	mb := NewMessageBuilder()
	mb.WriteString(portal)
	mb.WriteString(statement)
	mb.WriteInt16(0) // all-text parameter formats
	mb.WriteInt16(int16(len(params)))
	for _, param := range params {
		mb.WriteInt32(int32(len(param)))
		mb.WriteBytes([]byte(param))
	}
	mb.WriteInt16(int16(len(resultFormats)))
	for _, format := range resultFormats {
		mb.WriteInt16(format)
	}
	return &Message{Type: MsgBind, Data: mb.Bytes()}
}

func executeMessage(portal string) *Message {
	mb := NewMessageBuilder()
	mb.WriteString(portal)
	mb.WriteInt32(0)
	return &Message{Type: MsgExecute, Data: mb.Bytes()}
}

func sendExtended(t *testing.T, c *Connection, messages ...*Message) {
	t.Helper()
	for _, message := range messages {
		if err := c.handleMessage(message); err != nil {
			t.Fatalf("handle %c: %v", message.Type, err)
		}
	}
}

// dataRowRawValues splits a DataRow into its raw field bytes; nil means SQL
// NULL.
func dataRowRawValues(t *testing.T, msg *Message) [][]byte {
	t.Helper()
	if msg.Type != MsgDataRow {
		t.Fatalf("message type = %c, want DataRow", msg.Type)
	}
	if len(msg.Data) < 2 {
		t.Fatal("short DataRow")
	}
	count := int(binary.BigEndian.Uint16(msg.Data[:2]))
	pos := 2
	values := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if pos+4 > len(msg.Data) {
			t.Fatal("short DataRow field length")
		}
		length := int32(binary.BigEndian.Uint32(msg.Data[pos : pos+4]))
		pos += 4
		if length == -1 {
			values = append(values, nil)
			continue
		}
		if length < 0 || pos+int(length) > len(msg.Data) {
			t.Fatalf("field %d length %d out of range", i, length)
		}
		values = append(values, msg.Data[pos:pos+int(length)])
		pos += int(length)
	}
	if pos != len(msg.Data) {
		t.Fatalf("DataRow has %d trailing bytes", len(msg.Data)-pos)
	}
	return values
}

func dataRowsRaw(t *testing.T, messages []*Message) [][][]byte {
	t.Helper()
	var rows [][][]byte
	for _, message := range messages {
		if message.Type == MsgDataRow {
			rows = append(rows, dataRowRawValues(t, message))
		}
	}
	return rows
}

func rowDescriptionFormats(t *testing.T, data []byte) []int16 {
	t.Helper()
	if len(data) < 2 {
		t.Fatal("row description too short")
	}
	count := int(binary.BigEndian.Uint16(data[:2]))
	pos := 2
	formats := make([]int16, 0, count)
	for i := 0; i < count; i++ {
		end := bytes.IndexByte(data[pos:], 0)
		if end < 0 {
			t.Fatal("unterminated column name")
		}
		pos += end + 1 // name
		pos += 4       // table OID
		pos += 2       // column attribute number
		pos += 4       // type OID
		pos += 2       // type size
		pos += 4       // type modifier
		if pos+2 > len(data) {
			t.Fatal("truncated row description")
		}
		formats = append(formats, int16(binary.BigEndian.Uint16(data[pos:pos+2])))
		pos += 2
	}
	return formats
}

func messageTypes(messages []*Message) string {
	var types []byte
	for _, message := range messages {
		types = append(types, message.Type)
	}
	return string(types)
}

func prepareBinaryResultTable(t *testing.T, c *Connection) {
	t.Helper()
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, flag BOOLEAN, name TEXT, amount REAL, score DOUBLE, doc JSONB, data BLOB, tags TEXT[], nums NUMERIC[], flags BOOLEAN[])`,
		`INSERT INTO t VALUES (1, 1, 'alpha', 1.5, 2.25, '{"a": 1}', X'00ff', '{a,"b c"}', '{1.10,12345678901234567890}', '{t,f}')`,
		`INSERT INTO t VALUES (2, 0, NULL, NULL, NULL, NULL, NULL, '{}', '{NULL,2}', '{f,NULL}')`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

const binaryResultSelect = `SELECT id, flag, name, amount, score, doc, data, tags, nums, flags FROM t ORDER BY id`

// An empty Bind result format list keeps every column in text format, and the
// RowDescription reports format 0 for all of them.
func TestBinaryResultsAllTextByDefault(t *testing.T) {
	c, conn := extendedResultConnection(t)
	prepareBinaryResultTable(t, c)
	sendExtended(t, c,
		parseMessage("s", binaryResultSelect),
		bindResultMessage("p", "s", nil, nil),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if msg := findError(messages); msg != nil {
		t.Fatalf("text result failed: %s", errorCode(msg))
	}
	rd := findRowDescription(t, messages)
	for i, format := range rowDescriptionFormats(t, rd.Data) {
		if format != 0 {
			t.Fatalf("column %d format = %d, want text", i, format)
		}
	}
	rows := dataRowsRaw(t, messages)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if string(rows[0][2]) != "alpha" || string(rows[0][3]) != "1.5" {
		t.Fatalf("text row = %q", rows[0])
	}
}

// A single result format code repeats for every column, so one binary entry
// makes the whole result binary. Every supported scalar and array type is
// checked against its exact wire representation.
func TestBinaryResultsSingleFormatRepeats(t *testing.T) {
	c, conn := extendedResultConnection(t)
	prepareBinaryResultTable(t, c)
	sendExtended(t, c,
		parseMessage("s", binaryResultSelect),
		bindResultMessage("p", "s", nil, []int16{1}),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if msg := findError(messages); msg != nil {
		t.Fatalf("binary result failed: %s", errorCode(msg))
	}
	rd := findRowDescription(t, messages)
	formats := rowDescriptionFormats(t, rd.Data)
	if len(formats) != 10 {
		t.Fatalf("RowDescription has %d columns, want 10", len(formats))
	}
	for i, format := range formats {
		if format != 1 {
			t.Fatalf("column %d format = %d, want binary", i, format)
		}
	}
	// The OIDs do not change with the format.
	oids := rowDescriptionOIDs(t, rd.Data)
	wantOIDs := []int32{23, 16, 25, 700, 701, 3802, 17, 1009, 1231, 1000}
	for i, want := range wantOIDs {
		if oids[i] != want {
			t.Errorf("column %d OID = %d, want %d", i, oids[i], want)
		}
	}

	rows := dataRowsRaw(t, messages)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	row := rows[0]
	assertBytes(t, "int4", row[0], []byte{0, 0, 0, 1})
	assertBytes(t, "bool", row[1], []byte{1})
	assertBytes(t, "text", row[2], []byte("alpha"))
	assertBytes(t, "float4", row[3], binary.BigEndian.AppendUint32(nil, math.Float32bits(1.5)))
	assertBytes(t, "float8", row[4], binary.BigEndian.AppendUint64(nil, math.Float64bits(2.25)))
	if len(row[5]) == 0 || row[5][0] != 1 {
		t.Fatalf("jsonb value %v does not start with the version byte", row[5])
	}
	if !bytes.Contains(row[5][1:], []byte(`"a"`)) {
		t.Fatalf("jsonb value %q does not carry the JSON text", row[5])
	}
	assertBytes(t, "bytea", row[6], []byte{0x00, 0xff})

	// text[]: one dimension, two elements, lower bound 1.
	assertArrayHeader(t, "text[]", row[7], 25, 2, false)
	if got := arrayElements(t, row[7]); len(got) != 2 || string(got[0]) != "a" || string(got[1]) != "b c" {
		t.Fatalf("text[] elements = %q", got)
	}

	// numeric[]: 1.10 keeps its scale, and the 20-digit value stays exact.
	assertArrayHeader(t, "numeric[]", row[8], 1700, 2, false)
	numericElements := arrayElements(t, row[8])
	assertNumericBinary(t, "1.10", numericElements[0], 2, 0, 2, []uint16{1, 1000})
	assertNumericBinary(t, "12345678901234567890", numericElements[1], 5, 4, 0, []uint16{1234, 5678, 9012, 3456, 7890})

	// bool[]: t/f.
	assertArrayHeader(t, "bool[]", row[9], 16, 2, false)
	if got := arrayElements(t, row[9]); len(got) != 2 || got[0][0] != 1 || got[1][0] != 0 {
		t.Fatalf("bool[] elements = %v", got)
	}

	// Row 2 exercises NULL columns, an empty array, and NULL array elements.
	second := rows[1]
	assertBytes(t, "int4 second row", second[0], []byte{0, 0, 0, 2})
	assertBytes(t, "bool second row", second[1], []byte{0})
	for _, index := range []int{2, 3, 4, 5, 6} {
		if second[index] != nil {
			t.Errorf("column %d = %v, want NULL", index, second[index])
		}
	}
	// An empty array has no dimension header.
	assertBytes(t, "empty text[]", second[7], []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 25})
	assertArrayHeader(t, "numeric[] with NULL", second[8], 1700, 2, true)
	numericSecond := arrayElements(t, second[8])
	if numericSecond[0] != nil {
		t.Fatalf("numeric[] first element = %v, want NULL", numericSecond[0])
	}
	assertNumericBinary(t, "2", numericSecond[1], 1, 0, 0, []uint16{2})
	assertArrayHeader(t, "bool[] with NULL", second[9], 16, 2, true)
	boolSecond := arrayElements(t, second[9])
	if boolSecond[0] == nil || boolSecond[0][0] != 0 || boolSecond[1] != nil {
		t.Fatalf("bool[] elements = %v, want [f NULL]", boolSecond)
	}
}

// Mixed lists choose text and binary per column in one result.
func TestBinaryResultsMixedPerColumnFormats(t *testing.T) {
	c, conn := extendedResultConnection(t)
	prepareBinaryResultTable(t, c)
	// id text, flag binary, name text, amount binary, score text, doc binary,
	// data text, tags binary, nums text, flags binary.
	formats := []int16{0, 1, 0, 1, 0, 1, 0, 1, 0, 1}
	sendExtended(t, c,
		parseMessage("s", binaryResultSelect),
		bindResultMessage("p", "s", nil, formats),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if msg := findError(messages); msg != nil {
		t.Fatalf("mixed result failed: %s", errorCode(msg))
	}
	rd := findRowDescription(t, messages)
	if got := rowDescriptionFormats(t, rd.Data); !equalInt16(got, formats) {
		t.Fatalf("RowDescription formats = %v, want %v", got, formats)
	}
	rows := dataRowsRaw(t, messages)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	row := rows[0]
	// Text columns carry their text spelling.
	assertBytes(t, "text id", row[0], []byte("1"))
	assertBytes(t, "text name", row[2], []byte("alpha"))
	assertBytes(t, "text score", row[4], []byte("2.25"))
	assertBytes(t, "text data", row[6], []byte(`\x00ff`))
	assertBytes(t, "text nums", row[8], []byte("{1.10,12345678901234567890}"))
	// Binary columns carry their binary representation.
	assertBytes(t, "binary flag", row[1], []byte{1})
	assertBytes(t, "binary amount", row[3], binary.BigEndian.AppendUint32(nil, math.Float32bits(1.5)))
	if len(row[5]) == 0 || row[5][0] != 1 {
		t.Fatalf("jsonb value %v does not start with the version byte", row[5])
	}
	assertArrayHeader(t, "binary tags", row[7], 25, 2, false)
	assertArrayHeader(t, "binary flags", row[9], 16, 2, false)
}

// Formats belong to the portal, not the statement: two portals over one
// prepared statement can use different formats, and re-binding a portal
// replaces its list.
func TestBinaryResultsArePerPortal(t *testing.T) {
	c, conn := extendedResultConnection(t)
	prepareBinaryResultTable(t, c)
	sql := `SELECT id, flag FROM t ORDER BY id`
	sendExtended(t, c,
		parseMessage("s", sql),
		bindResultMessage("text_portal", "s", nil, nil),
		bindResultMessage("binary_portal", "s", nil, []int16{1}),
		executeMessage("text_portal"),
		executeMessage("binary_portal"),
		bindResultMessage("text_portal", "s", nil, []int16{0}),
		executeMessage("text_portal"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if msg := findError(messages); msg != nil {
		t.Fatalf("per-portal result failed: %s", errorCode(msg))
	}
	var descriptions [][]int16
	var rows [][][]byte
	for _, message := range messages {
		switch message.Type {
		case MsgRowDescription:
			descriptions = append(descriptions, rowDescriptionFormats(t, message.Data))
		case MsgDataRow:
			rows = append(rows, dataRowRawValues(t, message))
		}
	}
	if len(descriptions) != 3 {
		t.Fatalf("got %d RowDescriptions, want 3", len(descriptions))
	}
	if !equalInt16(descriptions[0], []int16{0, 0}) {
		t.Fatalf("text portal formats = %v", descriptions[0])
	}
	if !equalInt16(descriptions[1], []int16{1, 1}) {
		t.Fatalf("binary portal formats = %v", descriptions[1])
	}
	if !equalInt16(descriptions[2], []int16{0, 0}) {
		t.Fatalf("re-bound text portal formats = %v", descriptions[2])
	}
	// The two text portals put "1" and "1" in the first row; the binary portal
	// puts four bytes.
	if len(rows) != 6 {
		t.Fatalf("got %d rows, want 6", len(rows))
	}
	if string(rows[0][0]) != "1" || string(rows[1][0]) != "2" {
		t.Fatalf("text portal rows = %q %q", rows[0][0], rows[1][0])
	}
	assertBytes(t, "binary portal id", rows[2][0], []byte{0, 0, 0, 1})
	if string(rows[4][0]) != "1" || string(rows[5][0]) != "2" {
		t.Fatalf("re-bound text portal rows = %q %q", rows[4][0], rows[5][0])
	}
}

// A format list whose length is neither one nor the column count is a protocol
// violation, and no RowDescription or DataRow is sent before the error.
func TestBinaryResultsFormatCountMismatch(t *testing.T) {
	c, conn := extendedResultConnection(t)
	prepareBinaryResultTable(t, c)
	sendExtended(t, c,
		parseMessage("s", `SELECT id, flag FROM t`),
		bindResultMessage("p", "s", nil, []int16{0, 1, 0}),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if got := messageTypes(messages); got != "12"+string(MsgErrorResponse)+string(MsgReadyForQuery) {
		t.Fatalf("messages = %q", got)
	}
	if code := errorCode(findError(messages)); code != ErrCodeProtocolViolation {
		t.Fatalf("SQLSTATE = %s, want %s", code, ErrCodeProtocolViolation)
	}
	if c.extendedFailed {
		t.Fatal("Sync did not clear the extended-failure state")
	}

	// The connection is reusable after the rejected cycle.
	conn.reset()
	sendExtended(t, c,
		parseMessage("s2", `SELECT id FROM t ORDER BY id`),
		bindResultMessage("p2", "s2", nil, []int16{1}),
		executeMessage("p2"),
		&Message{Type: MsgSync},
	)
	messages = readAllMessages(t, conn.bytes())
	if msg := findError(messages); msg != nil {
		t.Fatalf("query after format error failed: %s", errorCode(msg))
	}
	rows := dataRowsRaw(t, messages)
	if len(rows) != 2 || !bytes.Equal(rows[0][0], []byte{0, 0, 0, 1}) {
		t.Fatalf("rows after format error = %v", rows)
	}
}

// A binary request for a type the wire layer cannot encode fails with 0A000
// before any result frame, including when the statement has rows to return.
func TestBinaryResultsUnsupportedOID(t *testing.T) {
	c, conn := extendedResultConnection(t)
	if _, err := c.executor.Execute(parseStmt(t, `CREATE TABLE stamps (id INTEGER PRIMARY KEY, created_at TIMESTAMP)`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.executor.Execute(parseStmt(t, `INSERT INTO stamps VALUES (1, '2024-01-02 03:04:05')`)); err != nil {
		t.Fatal(err)
	}
	sendExtended(t, c,
		parseMessage("s", `SELECT id, created_at FROM stamps`),
		bindResultMessage("p", "s", nil, []int16{1}),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if got := messageTypes(messages); got != "12"+string(MsgErrorResponse)+string(MsgReadyForQuery) {
		t.Fatalf("messages = %q", got)
	}
	if code := errorCode(findError(messages)); code != ErrCodeFeatureNotSupported {
		t.Fatalf("SQLSTATE = %s, want %s", code, ErrCodeFeatureNotSupported)
	}
	// Text format for the same statement still works.
	conn.reset()
	sendExtended(t, c,
		parseMessage("s2", `SELECT created_at FROM stamps`),
		bindResultMessage("p2", "s2", nil, nil),
		executeMessage("p2"),
		&Message{Type: MsgSync},
	)
	messages = readAllMessages(t, conn.bytes())
	if msg := findError(messages); msg != nil {
		t.Fatalf("text query after unsupported binary failed: %s", errorCode(msg))
	}
	if rows := dataRowsRaw(t, messages); len(rows) != 1 || string(rows[0][0]) != "2024-01-02 03:04:05" {
		t.Fatalf("text rows = %q", rows)
	}
}

// A rejected format inside an explicit transaction block marks the transaction
// failed, as any statement error does, and no rows are emitted.
func TestBinaryResultsErrorFailsTransaction(t *testing.T) {
	c, conn := extendedResultConnection(t)
	if _, err := c.executor.Execute(parseStmt(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)); err != nil {
		t.Fatal(err)
	}
	sendExtended(t, c,
		&Message{Type: MsgQuery, Data: []byte("BEGIN\x00")},
		parseMessage("s", `SELECT id FROM t`),
		bindResultMessage("p", "s", nil, []int16{0, 1}),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	ready := messages[len(messages)-1]
	if ready.Type != MsgReadyForQuery || ready.Data[0] != TxStatusFailed {
		t.Fatalf("transaction status = %q, want failed", messageTypes(messages))
	}
	if code := errorCode(findError(messages)); code != ErrCodeProtocolViolation {
		t.Fatalf("SQLSTATE = %s, want %s", code, ErrCodeProtocolViolation)
	}
	// While failed, the next statement is refused with 25P02.
	conn.reset()
	sendExtended(t, c,
		parseMessage("s2", `SELECT id FROM t`),
		bindResultMessage("p2", "s2", nil, nil),
		executeMessage("p2"),
		&Message{Type: MsgSync},
	)
	messages = readAllMessages(t, conn.bytes())
	if code := errorCode(findError(messages)); code != ErrCodeTransactionAborted {
		t.Fatalf("SQLSTATE = %s, want %s", code, ErrCodeTransactionAborted)
	}
}

// appendNumericText encodes exact decimal text into base-10000 groups, keeping
// the display scale and rejecting values that are not decimal numbers.
func TestAppendNumericTextEncoding(t *testing.T) {
	cases := []struct {
		text    string
		ndigits uint16
		weight  int16
		sign    uint16
		dscale  uint16
		digits  []uint16
	}{
		{"0", 0, 0, 0, 0, nil},
		{"1.10", 2, 0, 0, 2, []uint16{1, 1000}},
		{"-2.5", 2, 0, 0x4000, 1, []uint16{2, 5000}},
		{"12345678901234567890", 5, 4, 0, 0, []uint16{1234, 5678, 9012, 3456, 7890}},
		{"1e5", 1, 1, 0, 0, []uint16{10}},
		{"0.00001", 1, -2, 0, 5, []uint16{1000}},
		{"-0.000", 0, 0, 0, 3, nil},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			encoded, err := appendNumericText(nil, tc.text)
			if err != nil {
				t.Fatalf("appendNumericText(%q): %v", tc.text, err)
			}
			assertNumericBinary(t, tc.text, encoded, tc.ndigits, tc.weight, tc.dscale, tc.digits, tc.sign)
		})
	}
	for _, text := range []string{"", "abc", "1.2.3", "NaN", "1e999999"} {
		if _, err := appendNumericText(nil, text); err == nil {
			t.Errorf("appendNumericText(%q) succeeded, want an error", text)
		}
	}
}

// assertNumericBinary checks a binary numeric payload.
func assertNumericBinary(t *testing.T, label string, data []byte, ndigits uint16, weight int16, dscale uint16, digits []uint16, sign ...uint16) {
	t.Helper()
	wantSign := uint16(0)
	if len(sign) > 0 {
		wantSign = sign[0]
	}
	if len(data) != 8+2*len(digits) {
		t.Fatalf("%s numeric length = %d, want %d (%v)", label, len(data), 8+2*len(digits), data)
	}
	if got := binary.BigEndian.Uint16(data[0:2]); got != ndigits {
		t.Errorf("%s ndigits = %d, want %d", label, got, ndigits)
	}
	if got := int16(binary.BigEndian.Uint16(data[2:4])); got != weight {
		t.Errorf("%s weight = %d, want %d", label, got, weight)
	}
	if got := binary.BigEndian.Uint16(data[4:6]); got != wantSign {
		t.Errorf("%s sign = %#x, want %#x", label, got, wantSign)
	}
	if got := binary.BigEndian.Uint16(data[6:8]); got != dscale {
		t.Errorf("%s dscale = %d, want %d", label, got, dscale)
	}
	for i, want := range digits {
		if got := binary.BigEndian.Uint16(data[8+2*i : 10+2*i]); got != want {
			t.Errorf("%s digit %d = %d, want %d", label, i, got, want)
		}
	}
}

func assertArrayHeader(t *testing.T, label string, data []byte, elementOID int32, count int, hasNull bool) {
	t.Helper()
	if len(data) < 12 {
		t.Fatalf("%s array is %d bytes, want a header", label, len(data))
	}
	if ndim := int32(binary.BigEndian.Uint32(data[0:4])); ndim != 1 {
		t.Fatalf("%s ndim = %d, want 1", label, ndim)
	}
	wantNull := uint32(0)
	if hasNull {
		wantNull = 1
	}
	if got := binary.BigEndian.Uint32(data[4:8]); got != wantNull {
		t.Errorf("%s hasnull = %d, want %d", label, got, wantNull)
	}
	if got := int32(binary.BigEndian.Uint32(data[8:12])); got != elementOID {
		t.Errorf("%s element OID = %d, want %d", label, got, elementOID)
	}
	if len(data) < 20 {
		t.Fatalf("%s array is %d bytes, want a dimension header", label, len(data))
	}
	if got := int32(binary.BigEndian.Uint32(data[12:16])); got != int32(count) {
		t.Errorf("%s dimension length = %d, want %d", label, got, count)
	}
	if got := int32(binary.BigEndian.Uint32(data[16:20])); got != 1 {
		t.Errorf("%s lower bound = %d, want 1", label, got)
	}
}

// arrayElements splits the element payloads of a one-dimensional binary array.
func arrayElements(t *testing.T, data []byte) [][]byte {
	t.Helper()
	if len(data) < 20 {
		t.Fatalf("array is %d bytes, want a dimension header", len(data))
	}
	count := int(binary.BigEndian.Uint32(data[12:16]))
	pos := 20
	elements := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if pos+4 > len(data) {
			t.Fatalf("array element %d is truncated", i)
		}
		length := int32(binary.BigEndian.Uint32(data[pos : pos+4]))
		pos += 4
		if length == -1 {
			elements = append(elements, nil)
			continue
		}
		if length < 0 || pos+int(length) > len(data) {
			t.Fatalf("array element %d length %d out of range", i, length)
		}
		elements = append(elements, data[pos:pos+int(length)])
		pos += int(length)
	}
	if pos != len(data) {
		t.Fatalf("array has %d trailing bytes", len(data)-pos)
	}
	return elements
}

func assertBytes(t *testing.T, label string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q (%v), want %q (%v)", label, got, got, want, want)
	}
}

func equalInt16(left, right []int16) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// Describe never exposes the portal's bound formats: statement and portal
// Describe both return NoData, and the formats travel with the RowDescription
// emitted at Execute.
func TestBinaryResultsDescribeStaysNoData(t *testing.T) {
	c, conn := extendedResultConnection(t)
	prepareBinaryResultTable(t, c)
	sendExtended(t, c,
		parseMessage("s", `SELECT id, flag FROM t`),
		bindResultMessage("p", "s", nil, []int16{1}),
		&Message{Type: MsgDescribe, Data: []byte{'S', 's', 0}},
		&Message{Type: MsgDescribe, Data: []byte{'P', 'p', 0}},
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if got := messageTypes(messages); got != "12tnnZ" {
		t.Fatalf("messages = %q, want ParseComplete/BindComplete/ParameterDescription/NoData/NoData/ReadyForQuery", got)
	}
}

// Simple-query results keep text format; only a Bind result format list can
// request binary columns.
func TestSimpleQueryResultsStayText(t *testing.T) {
	c, conn := extendedResultConnection(t)
	prepareBinaryResultTable(t, c)
	messages := runSimple(t, c, conn, `SELECT id, flag, tags FROM t ORDER BY id`)
	if msg := findError(messages); msg != nil {
		t.Fatalf("simple query failed: %s", errorCode(msg))
	}
	rd := findRowDescription(t, messages)
	for i, format := range rowDescriptionFormats(t, rd.Data) {
		if format != 0 {
			t.Fatalf("column %d format = %d, want text", i, format)
		}
	}
	rows := dataRowsRaw(t, messages)
	if len(rows) != 2 || string(rows[0][2]) != `{a,"b c"}` {
		t.Fatalf("simple rows = %q", rows)
	}
}

// VARCHAR arrays report OID 1015 and carry element OID 1043.
func TestBinaryResultsVarcharArray(t *testing.T) {
	c, conn := extendedResultConnection(t)
	if _, err := c.executor.Execute(parseStmt(t, `CREATE TABLE v (id INTEGER PRIMARY KEY, names VARCHAR(10)[])`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.executor.Execute(parseStmt(t, `INSERT INTO v VALUES (1, '{a,"b c"}')`)); err != nil {
		t.Fatal(err)
	}
	sendExtended(t, c,
		parseMessage("s", `SELECT names FROM v`),
		bindResultMessage("p", "s", nil, []int16{1}),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if msg := findError(messages); msg != nil {
		t.Fatalf("varchar[] result failed: %s", errorCode(msg))
	}
	rd := findRowDescription(t, messages)
	if oid := rowDescriptionOIDs(t, rd.Data)[0]; oid != 1015 {
		t.Fatalf("varchar[] OID = %d, want 1015", oid)
	}
	rows := dataRowsRaw(t, messages)
	assertArrayHeader(t, "varchar[]", rows[0][0], 1043, 2, false)
	elements := arrayElements(t, rows[0][0])
	if len(elements) != 2 || string(elements[0]) != "a" || string(elements[1]) != "b c" {
		t.Fatalf("varchar[] elements = %q", elements)
	}
}

// TestAppendBinaryResultValue covers the encoders that no SQL type reaches
// directly, such as int2 results and int2 arrays.
func TestAppendBinaryResultValue(t *testing.T) {
	cases := []struct {
		name  string
		oid   int32
		value interface{}
		want  []byte
	}{
		{"bool true", 16, bool(true), []byte{1}},
		{"bool from int64", 16, int64(0), []byte{0}},
		{"bool from text", 16, "yes", []byte{1}},
		{"int2", 21, int64(-2), []byte{0xff, 0xfe}},
		{"int2 from text", 21, "12", []byte{0, 12}},
		{"int4", 23, int64(1), []byte{0, 0, 0, 1}},
		{"int8", 20, int64(-1), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
		{"float4", 700, float64(1.5), binary.BigEndian.AppendUint32(nil, math.Float32bits(1.5))},
		{"float8", 701, int64(2), binary.BigEndian.AppendUint64(nil, math.Float64bits(2))},
		{"text", 25, "alpha", []byte("alpha")},
		{"varchar", 1043, "beta", []byte("beta")},
		{"bytea bytes", 17, []byte{0x00, 0xff}, []byte{0x00, 0xff}},
		{"bytea string", 17, "ab", []byte("ab")},
		{"numeric text", 1700, "1.10", []byte{0, 2, 0, 0, 0, 0, 0, 2, 0, 1, 0x03, 0xe8}},
		{"numeric int", 1700, int64(2), []byte{0, 1, 0, 0, 0, 0, 0, 0, 0, 2}},
		{"jsonb", 3802, `{"a": 1}`, []byte(`{"a": 1}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := appendBinaryResultValue(nil, tc.oid, tc.value)
			if err != nil {
				t.Fatalf("appendBinaryResultValue(%d, %v): %v", tc.oid, tc.value, err)
			}
			want := tc.want
			if tc.oid == 3802 {
				want = append([]byte{1}, tc.want...)
			}
			assertBytes(t, tc.name, got, want)
		})
	}

	for _, tc := range []struct {
		name  string
		oid   int32
		value interface{}
	}{
		{"int4 from text", 23, "abc"},
		{"int2 out of range", 21, int64(70000)},
		{"int4 out of range", 23, int64(1) << 40},
		{"bool from text", 16, "maybe"},
		{"float from text", 701, "abc"},
		{"unsupported oid", 1184, "2024-01-02 03:04:05"},
		{"numeric from bool", 1700, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := appendBinaryResultValue(nil, tc.oid, tc.value); err == nil {
				t.Fatalf("%s: encoding succeeded, want an error", tc.name)
			}
		})
	}
}

// TestBinaryResultArrayOIDs covers every array OID in the encoding table,
// including the int2/varchar forms the engine reports for declared arrays.
func TestBinaryResultArrayOIDs(t *testing.T) {
	cases := []struct {
		name       string
		arrayOID   int32
		elementOID int32
		text       string
		want       [][]byte
	}{
		{"bool[]", 1000, 16, "{t,f}", [][]byte{{1}, {0}}},
		{"int2[]", 1005, 21, "{1,-2}", [][]byte{{0, 1}, {0xff, 0xfe}}},
		{"int4[]", 1007, 23, "{1,2}", [][]byte{{0, 0, 0, 1}, {0, 0, 0, 2}}},
		{"text[]", 1009, 25, `{a,"b c"}`, [][]byte{[]byte("a"), []byte("b c")}},
		{"varchar[]", 1015, 1043, "{a}", [][]byte{[]byte("a")}},
		{"int8[]", 1016, 20, "{1}", [][]byte{{0, 0, 0, 0, 0, 0, 0, 1}}},
		{"float4[]", 1021, 700, "{1.5}", [][]byte{binary.BigEndian.AppendUint32(nil, math.Float32bits(1.5))}},
		{"float8[]", 1022, 701, "{1.5}", [][]byte{binary.BigEndian.AppendUint64(nil, math.Float64bits(1.5))}},
		{"numeric[]", 1231, 1700, "{1.10}", [][]byte{{0, 2, 0, 0, 0, 0, 0, 2, 0, 1, 0x03, 0xe8}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := appendBinaryResultValue(nil, tc.arrayOID, tc.text)
			if err != nil {
				t.Fatalf("encode %s: %v", tc.name, err)
			}
			assertArrayHeader(t, tc.name, got, tc.elementOID, len(tc.want), false)
			elements := arrayElements(t, got)
			if len(elements) != len(tc.want) {
				t.Fatalf("%s elements = %d, want %d", tc.name, len(elements), len(tc.want))
			}
			for i := range tc.want {
				assertBytes(t, tc.name+" element", elements[i], tc.want[i])
			}
		})
	}

	// Unsupported array OIDs are not encodable.
	for _, oid := range []int32{1001 /* bytea[] */, 1185 /* timestamptz[] */} {
		if canEncodeBinaryResult(oid) {
			t.Errorf("canEncodeBinaryResult(%d) = true", oid)
		}
	}
	for _, oid := range []int32{16, 17, 20, 21, 23, 25, 700, 701, 1043, 1700, 3802, 1000, 1005, 1007, 1009, 1015, 1016, 1021, 1022, 1231} {
		if !canEncodeBinaryResult(oid) {
			t.Errorf("canEncodeBinaryResult(%d) = false", oid)
		}
	}
	for _, oid := range []int32{1082, 1184} {
		if canEncodeBinaryResult(oid) {
			t.Errorf("canEncodeBinaryResult(%d) = true", oid)
		}
	}
}

// A statement that writes and returns rows executes before its Bind result
// formats are resolved, because the row metadata only exists at Execute. The
// rejected binary request therefore sends no rows but does not undo the write;
// this ordering is documented in the capability document.
func TestBinaryResultsRejectAfterDMLExecution(t *testing.T) {
	c, conn := extendedResultConnection(t)
	if _, err := c.executor.Execute(parseStmt(t, `CREATE TABLE w (id INTEGER PRIMARY KEY, created_at TIMESTAMP)`)); err != nil {
		t.Fatal(err)
	}
	sendExtended(t, c,
		parseMessage("s", `INSERT INTO w VALUES (1, '2024-01-02 03:04:05') RETURNING created_at`),
		bindResultMessage("p", "s", nil, []int16{1}),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if got := messageTypes(messages); got != "12"+string(MsgErrorResponse)+string(MsgReadyForQuery) {
		t.Fatalf("messages = %q, want no RowDescription or DataRow", got)
	}
	if code := errorCode(findError(messages)); code != ErrCodeFeatureNotSupported {
		t.Fatalf("SQLSTATE = %s, want %s", code, ErrCodeFeatureNotSupported)
	}
	result, err := c.executor.Execute(parseStmt(t, `SELECT COUNT(*) FROM w`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != int64(1) {
		t.Fatalf("write was rolled back: %v", result.Rows)
	}
}

// Catalog emulation results use the same per-portal formats as executor
// results: their text and boolean columns encode as binary.
func TestBinaryResultsCatalogQuery(t *testing.T) {
	c, conn := extendedResultConnection(t)
	if _, err := c.executor.Execute(parseStmt(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)); err != nil {
		t.Fatal(err)
	}
	sendExtended(t, c,
		parseMessage("s", `SELECT tablename, hasindexes FROM pg_tables`),
		bindResultMessage("p", "s", nil, []int16{1}),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if msg := findError(messages); msg != nil {
		t.Fatalf("binary catalog query failed: %s", errorCode(msg))
	}
	rd := findRowDescription(t, messages)
	if got := rowDescriptionFormats(t, rd.Data); !equalInt16(got, []int16{1, 1}) {
		t.Fatalf("catalog formats = %v", got)
	}
	rows := dataRowsRaw(t, messages)
	if len(rows) != 1 || string(rows[0][0]) != "t" {
		t.Fatalf("catalog rows = %q", rows)
	}
	// hasindexes is a binary boolean.
	if len(rows[0][1]) != 1 || rows[0][1][0] != 0 {
		t.Fatalf("hasindexes = %v, want false", rows[0][1])
	}
}

// A stored value the requested binary type cannot represent fails mid-result:
// the RowDescription and rows already written stay written, the failing DataRow
// is never emitted, the transaction is marked failed, and the connection stays
// usable after ROLLBACK.
func TestBinaryResultsEncodeFailureMidResult(t *testing.T) {
	c, conn := extendedResultConnection(t)
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, x INTEGER)`,
		`INSERT INTO t VALUES (1, 5), (2, 'abc'), (3, 7)`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if msg := findError(runSimple(t, c, conn, "BEGIN")); msg != nil {
		t.Fatalf("BEGIN failed: %s", errorCode(msg))
	}
	conn.reset()
	sendExtended(t, c,
		parseMessage("s", `SELECT id, x FROM t ORDER BY id`),
		bindResultMessage("p", "s", nil, []int16{1, 1}),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if got := messageTypes(messages); got != "12TDEZ" {
		t.Fatalf("messages = %q, want Parse/Bind/RowDescription/DataRow/Error/ReadyForQuery", got)
	}
	rows := dataRowsRaw(t, messages)
	if len(rows) != 1 {
		t.Fatalf("got %d DataRows, want only the rows before the failure", len(rows))
	}
	assertBytes(t, "row before the failure", rows[0][0], []byte{0, 0, 0, 1})
	assertBytes(t, "int4 before the failure", rows[0][1], []byte{0, 0, 0, 5})
	if code := errorCode(findError(messages)); code != ErrCodeInvalidText {
		t.Fatalf("SQLSTATE = %s, want %s", code, ErrCodeInvalidText)
	}
	ready := messages[len(messages)-1]
	if ready.Type != MsgReadyForQuery || ready.Data[0] != TxStatusFailed {
		t.Fatalf("transaction status = %c, want failed", ready.Data[0])
	}

	// The connection recovers after ROLLBACK and a new cycle.
	conn.reset()
	if err := c.handleMessage(&Message{Type: MsgQuery, Data: []byte("ROLLBACK\x00")}); err != nil {
		t.Fatal(err)
	}
	conn.reset()
	sendExtended(t, c,
		parseMessage("s2", `SELECT x FROM t WHERE id = 1`),
		bindResultMessage("p2", "s2", nil, []int16{1}),
		executeMessage("p2"),
		&Message{Type: MsgSync},
	)
	messages = readAllMessages(t, conn.bytes())
	if msg := findError(messages); msg != nil {
		t.Fatalf("query after the encode failure failed: %s", errorCode(msg))
	}
	if rows := dataRowsRaw(t, messages); len(rows) != 1 || !bytes.Equal(rows[0][0], []byte{0, 0, 0, 5}) {
		t.Fatalf("rows after recovery = %v", rows)
	}
}

// A value that does not fit int4 (2^40 in an INTEGER column) is an encoding
// failure even when the value converted cleanly at insert time.
func TestBinaryResultsEncodeFailureOutOfRange(t *testing.T) {
	c, conn := extendedResultConnection(t)
	for _, sql := range []string{
		`CREATE TABLE big (id INTEGER PRIMARY KEY, x INTEGER)`,
		`INSERT INTO big VALUES (1, 1099511627776)`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	sendExtended(t, c,
		parseMessage("s", `SELECT x FROM big`),
		bindResultMessage("p", "s", nil, []int16{1}),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if got := messageTypes(messages); got != "12TEZ" {
		t.Fatalf("messages = %q, want no DataRow for the failing value", got)
	}
	if code := errorCode(findError(messages)); code != ErrCodeInvalidText {
		t.Fatalf("SQLSTATE = %s, want %s", code, ErrCodeInvalidText)
	}
}

// Close('P') drops the portal and therefore its result formats; a later Bind
// under the same name starts from the new list.
func TestBinaryResultsCloseDropsPortalFormats(t *testing.T) {
	c, conn := extendedResultConnection(t)
	prepareBinaryResultTable(t, c)
	sendExtended(t, c,
		parseMessage("s", `SELECT id FROM t ORDER BY id`),
		bindResultMessage("p", "s", nil, []int16{1}),
		executeMessage("p"),
		&Message{Type: MsgClose, Data: []byte{'P', 'p', 0}},
		bindResultMessage("p", "s", nil, nil),
		executeMessage("p"),
		&Message{Type: MsgSync},
	)
	messages := readAllMessages(t, conn.bytes())
	if msg := findError(messages); msg != nil {
		t.Fatalf("close/re-bind failed: %s", errorCode(msg))
	}
	var formats [][]int16
	var rows [][][]byte
	for _, message := range messages {
		switch message.Type {
		case MsgRowDescription:
			formats = append(formats, rowDescriptionFormats(t, message.Data))
		case MsgDataRow:
			rows = append(rows, dataRowRawValues(t, message))
		}
	}
	if len(formats) != 2 || !equalInt16(formats[0], []int16{1}) {
		t.Fatalf("formats = %v, want the first portal binary", formats)
	}
	if !equalInt16(formats[1], []int16{0}) {
		t.Fatalf("formats after Close = %v, want text", formats[1])
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	assertBytes(t, "binary portal id", rows[0][0], []byte{0, 0, 0, 1})
	if string(rows[2][0]) != "1" {
		t.Fatalf("re-bound text portal id = %q", rows[2][0])
	}
}

// Float results keep their exact bit patterns, including -0 and NaN; float4
// rejects values whose conversion would become an infinity.
func TestBinaryFloatBitPatterns(t *testing.T) {
	negativeZero := math.Copysign(0, -1)
	nan := math.NaN()
	positiveInfinity := math.Inf(1)
	negativeInfinity := math.Inf(-1)
	cases := []struct {
		name  string
		oid   int32
		value interface{}
		want  []byte
	}{
		{"float8 -0", 701, negativeZero, binary.BigEndian.AppendUint64(nil, 0x8000000000000000)},
		{"float8 NaN", 701, nan, binary.BigEndian.AppendUint64(nil, math.Float64bits(nan))},
		{"float8 +Inf", 701, positiveInfinity, binary.BigEndian.AppendUint64(nil, 0x7ff0000000000000)},
		{"float8 -Inf", 701, negativeInfinity, binary.BigEndian.AppendUint64(nil, 0xfff0000000000000)},
		{"float8 Infinity text", 701, "Infinity", binary.BigEndian.AppendUint64(nil, 0x7ff0000000000000)},
		{"float4 -0", 700, negativeZero, []byte{0x80, 0, 0, 0}},
		{"float4 NaN", 700, nan, []byte{0x7f, 0xc0, 0, 0}},
		{"int8 -2^63", 20, float64(math.MinInt64), []byte{0x80, 0, 0, 0, 0, 0, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := appendBinaryResultValue(nil, tc.oid, tc.value)
			if err != nil {
				t.Fatalf("encode %s: %v", tc.name, err)
			}
			assertBytes(t, tc.name, got, tc.want)
		})
	}

	// float4 cannot represent an infinity, and 2^63 does not fit int8.
	for _, tc := range []struct {
		name  string
		oid   int32
		value interface{}
	}{
		{"float4 +Inf", 700, positiveInfinity},
		{"float4 -Inf", 700, negativeInfinity},
		{"float4 Infinity text", 700, "-Infinity"},
		{"int8 2^63", 20, math.Pow(2, 63)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := appendBinaryResultValue(nil, tc.oid, tc.value); err == nil {
				t.Fatalf("%s: encoding succeeded, want an error", tc.name)
			}
		})
	}
}
