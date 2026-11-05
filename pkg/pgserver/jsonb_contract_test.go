package pgserver

import (
	"testing"
)

// runExtendedJSONBBind drives one extended-protocol cycle with a single
// parameter of the given OID, format, and raw value.
func runExtendedJSONBBind(t *testing.T, c *Connection, conn *countingConn, sql string, oid int32, format int16, value []byte) []*Message {
	t.Helper()
	parse := NewMessageBuilder()
	parse.WriteString("s1")
	parse.WriteString(sql)
	parse.WriteInt16(1)
	parse.WriteInt32(oid)

	bind := NewMessageBuilder()
	bind.WriteString("p1")
	bind.WriteString("s1")
	bind.WriteInt16(1) // one parameter format
	bind.WriteInt16(format)
	bind.WriteInt16(1) // one parameter
	bind.WriteInt32(int32(len(value)))
	bind.WriteBytes(value)
	bind.WriteInt16(0) // all-text result formats

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

// A text-format jsonb parameter (OID 3802) is canonicalized at Bind, so the
// query sees canonical jsonb bytes and an invalid document fails before
// execution.
func TestJSONBTextParameterOverExtendedQuery(t *testing.T) {
	c, conn := arrayConnection(t)

	conn.reset()
	messages := runExtendedJSONBBind(t, c, conn, `SELECT $1`, 3802, 0, []byte(`{ "b" : 2, "a" : 1.50 }`))
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("jsonb parameter failed: %s %s", errorCode(errMsg), jsonErrorField(t, errMsg, ErrorFieldMessage))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != `{"a":1.5,"b":2}` {
		t.Fatalf("canonical jsonb parameter = %v", row)
	}
	if oids := rowDescriptionOIDs(t, findRowDescription(t, messages).Data); len(oids) != 1 || oids[0] != 3802 {
		t.Fatalf("jsonb parameter result OIDs = %v, want [3802]", oids)
	}

	// The bound value is a real jsonb value for operators and functions.
	conn.reset()
	messages = runExtendedJSONBBind(t, c, conn, `SELECT $1 -> 'a', $1 ? 'b'`, 3802, 0, []byte(`{"b":2,"a":1}`))
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("jsonb operator over parameter failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 2 || row[0] != "1" || row[1] != "true" {
		t.Fatalf("jsonb operators over parameter = %v", row)
	}

	// Invalid text is 22P02 at Bind.
	conn.reset()
	messages = runExtendedJSONBBind(t, c, conn, `SELECT $1`, 3802, 0, []byte(`not json`))
	errMsg := findError(messages)
	if errMsg == nil || errorCode(errMsg) != ErrCodeInvalidText {
		t.Fatalf("invalid jsonb parameter error = %v, want 22P02", messages)
	}

	// An INSERT binds the canonical document into the JSONB column, and a later
	// read returns the same canonical bytes.
	conn.reset()
	if _, err := c.executor.Execute(parseStmt(t, `CREATE TABLE docs (id INTEGER PRIMARY KEY, doc JSONB)`)); err != nil {
		t.Fatal(err)
	}
	messages = runExtendedJSONBBind(t, c, conn, `INSERT INTO docs VALUES (1, $1)`, 3802, 0, []byte(`{ "b" : 2, "a" : 1.50 }`))
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("jsonb INSERT parameter failed: %s", errorCode(errMsg))
	}
	conn.reset()
	messages = runSimple(t, c, conn, `SELECT doc FROM docs`)
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != `{"a":1.5,"b":2}` {
		t.Fatalf("jsonb INSERT round trip = %v", row)
	}

	// PostgreSQL binary jsonb is a version byte plus the document text. pgx sends
	// this form by default, so it is accepted and canonicalized.
	conn.reset()
	messages = runExtendedJSONBBind(t, c, conn, `SELECT $1 -> 'a'`, 3802, 1, append([]byte{0x01}, []byte(`{ "a" : 1.50 }`)...))
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("binary jsonb parameter failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "1.5" {
		t.Fatalf("binary jsonb parameter = %v, want [1.5]", row)
	}
	conn.reset()
	messages = runExtendedJSONBBind(t, c, conn, `SELECT $1`, 3802, 1, append([]byte{0x01}, []byte(`[1, 2]`)...))
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != `[1,2]` {
		t.Fatalf("binary jsonb canonicalization = %v, want [1,2]", row)
	}

	// A wrong version and a malformed body are invalid text (22P02), not 0A000.
	for _, tc := range []struct {
		value []byte
		code  string
	}{
		{[]byte{0x02, '{', '}'}, ErrCodeInvalidText},
		{[]byte{0x01}, ErrCodeInvalidText},
		{[]byte{0x01, 'n', 'o', 't'}, ErrCodeInvalidText},
		{[]byte{}, ErrCodeInvalidText},
	} {
		conn.reset()
		messages = runExtendedJSONBBind(t, c, conn, `SELECT $1`, 3802, 1, tc.value)
		errMsg := findError(messages)
		if errMsg == nil || errorCode(errMsg) != tc.code {
			t.Fatalf("binary jsonb %x error = %v, want %s", tc.value, messages, tc.code)
		}
	}
}

// JSONB columns report jsonb metadata (OID 3802) while JSON columns keep their
// text-backed behavior (OID 25).
func TestJSONBColumnTypeOIDs(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, sql := range []string{
		`CREATE TABLE docs (id INTEGER PRIMARY KEY, doc JSONB, body JSON)`,
		`INSERT INTO docs VALUES (1, '{"b":2, "a":1}', '{"b":2, "a":1}')`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	conn.reset()
	messages := runSimple(t, c, conn, `SELECT doc, body, to_jsonb(1), doc -> 'a' FROM docs`)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("select failed: %s", errorCode(errMsg))
	}
	oids := rowDescriptionOIDs(t, findRowDescription(t, messages).Data)
	want := []int32{3802, 25, 3802, 3802}
	if len(oids) != len(want) {
		t.Fatalf("jsonb/json projection OIDs = %v, want %v", oids, want)
	}
	for i := range want {
		if oids[i] != want[i] {
			t.Fatalf("column %d OID = %d, want %d (all: %v)", i, oids[i], want[i], oids)
		}
	}
	if row := firstDataRow(t, messages); len(row) != 4 || row[0] != `{"a":1,"b":2}` || row[1] != `{"b":2, "a":1}` || row[3] != `1` {
		t.Fatalf("jsonb/json projection values = %v", row)
	}

	// Text-format parameters for JSON (114) keep their previous passthrough
	// behavior rather than being canonicalized.
	conn.reset()
	messages = runExtendedJSONBBind(t, c, conn, `SELECT $1`, 114, 0, []byte(`{ "b" : 1 }`))
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("JSON parameter failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != `{ "b" : 1 }` {
		t.Fatalf("JSON parameter value = %v, want the original text", row)
	}
}
