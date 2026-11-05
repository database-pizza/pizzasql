package pgserver

import (
	"bufio"
	"testing"
)

// arrayConnection returns a connection whose output is captured in memory, so
// both simple and extended protocol responses can be decoded.
func arrayConnection(t *testing.T) (*Connection, *countingConn) {
	t.Helper()
	c, _ := newDataConnection(t)
	conn := &countingConn{}
	c.conn = conn
	c.writer = bufio.NewWriter(conn)
	return c, conn
}

// runSimple sends a simple Query and returns the decoded response messages.
func runSimple(t *testing.T, c *Connection, conn *countingConn, sql string) []*Message {
	t.Helper()
	if err := c.handleMessage(&Message{Type: MsgQuery, Data: append([]byte(sql), 0)}); err != nil {
		t.Fatalf("handleQuery %q: %v", sql, err)
	}
	return readAllMessages(t, conn.bytes())
}

// runExtendedParseBindExecute drives one extended-protocol cycle with a single
// text-format parameter of the given OID.
func runExtendedParseBindExecute(t *testing.T, c *Connection, conn *countingConn, sql string, oid int32, value string) []*Message {
	t.Helper()
	parse := NewMessageBuilder()
	parse.WriteString("s1")
	parse.WriteString(sql)
	parse.WriteInt16(1)
	parse.WriteInt32(oid)

	bind := NewMessageBuilder()
	bind.WriteString("p1")
	bind.WriteString("s1")
	bind.WriteInt16(0) // all-text parameter formats
	bind.WriteInt16(1)
	bind.WriteInt32(int32(len(value)))
	bind.WriteBytes([]byte(value))
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

func firstDataRow(t *testing.T, messages []*Message) []string {
	t.Helper()
	for _, msg := range messages {
		if msg.Type == MsgDataRow {
			return dataRowValues(t, msg)
		}
	}
	return nil
}

func findError(messages []*Message) *Message {
	for _, msg := range messages {
		if msg.Type == MsgErrorResponse {
			return msg
		}
	}
	return nil
}

// A pg client that binds text[] for "id = ANY($1)" gets the rows the array
// describes instead of a rewritten query.
func TestArrayParameterOverExtendedQuery(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, tags TEXT[])`,
		`INSERT INTO t VALUES (1, '{a}'), (2, '{b}'), (3, '{c}')`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	messages := runExtendedParseBindExecute(t, c, conn, `SELECT id FROM t WHERE id = ANY($1) ORDER BY id`, 1007, "{1,3}")
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("extended ANY($1) failed: %s", errorCode(errMsg))
	}
	rows := [][]string{}
	for _, msg := range messages {
		if msg.Type == MsgDataRow {
			rows = append(rows, dataRowValues(t, msg))
		}
	}
	if len(rows) != 2 || rows[0][0] != "1" || rows[1][0] != "3" {
		t.Fatalf("ANY($1) rows = %v, want [[1] [3]]", rows)
	}

	// text[] uses the same text binding path.
	conn.reset()
	messages = runExtendedParseBindExecute(t, c, conn, `SELECT id FROM t WHERE id = ANY($1)`, 1009, "{1,2}")
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("extended text[] ANY($1) failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 {
		t.Fatalf("text[] ANY($1) returned %v", row)
	}

	// An unspecified parameter OID keeps the array literal untyped; ANY still
	// reads it as array text, which is what drivers that omit OIDs rely on.
	conn.reset()
	messages = runExtendedParseBindExecute(t, c, conn, `SELECT id FROM t WHERE id = ANY($1) ORDER BY id`, 0, "{2,3}")
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("extended untyped ANY($1) failed: %s", errorCode(errMsg))
	}
	rows = nil
	for _, msg := range messages {
		if msg.Type == MsgDataRow {
			rows = append(rows, dataRowValues(t, msg))
		}
	}
	if len(rows) != 2 || rows[0][0] != "2" || rows[1][0] != "3" {
		t.Fatalf("untyped ANY($1) rows = %v, want [[2] [3]]", rows)
	}

	// NULL elements follow three-valued logic through the parameter path: rows
	// whose comparison is NULL are not returned, rows that match are.
	conn.reset()
	messages = runExtendedParseBindExecute(t, c, conn, `SELECT id FROM t WHERE id = ANY($1) ORDER BY id`, 1007, "{NULL,2}")
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("extended NULL-element ANY($1) failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "2" {
		t.Fatalf("NULL-element ANY($1) row = %v, want [2]", row)
	}

	// An empty array parameter matches nothing and is not an error.
	conn.reset()
	messages = runExtendedParseBindExecute(t, c, conn, `SELECT id FROM t WHERE id = ANY($1)`, 1007, "{}")
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("extended empty-array ANY($1) failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 0 {
		t.Fatalf("empty-array ANY($1) returned %v, want no rows", row)
	}

	// ALL keeps its dual semantics through the parameter path.
	conn.reset()
	messages = runExtendedParseBindExecute(t, c, conn, `SELECT id FROM t WHERE id <> ALL($1)`, 1007, "{1,2}")
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("extended ALL($1) failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "3" {
		t.Fatalf("ALL($1) row = %v, want [3]", row)
	}
}

func TestArrayParameterOverSimpleQuery(t *testing.T) {
	c, conn := arrayConnection(t)
	if _, err := c.executor.Execute(parseStmt(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO t VALUES (1), (2), (3)`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	messages := runSimple(t, c, conn, `SELECT id FROM t WHERE id = ANY('{1,3}') ORDER BY id`)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("simple ANY failed: %s", errorCode(errMsg))
	}
	rows := [][]string{}
	for _, msg := range messages {
		if msg.Type == MsgDataRow {
			rows = append(rows, dataRowValues(t, msg))
		}
	}
	if len(rows) != 2 || rows[0][0] != "1" || rows[1][0] != "3" {
		t.Fatalf("simple ANY rows = %v", rows)
	}
}

func TestArrayResultMetadataOIDs(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, tags TEXT[], nums INTEGER[], big BIGINT[], small SMALLINT[], flags BOOLEAN[], reals REAL[], doubles DOUBLE PRECISION[], nums2 NUMERIC[], names VARCHAR(10)[])`,
		`INSERT INTO t VALUES (1, '{a}', '{1}', '{1}', '{1}', '{t}', '{1.5}', '{1.5}', '{1.5}', '{x}')`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	messages := runSimple(t, c, conn, `SELECT tags, nums, big, small, flags, reals, doubles, nums2, names FROM t`)
	rd := findRowDescription(t, messages)
	oids := rowDescriptionOIDs(t, rd.Data)
	want := []int32{1009, 1007, 1016, 1016, 1000, 1021, 1022, 1231, 1015}
	if len(oids) != len(want) {
		t.Fatalf("array OIDs = %v, want %v", oids, want)
	}
	for i, oid := range want {
		if oids[i] != oid {
			t.Errorf("column %d OID = %d, want %d", i, oids[i], oid)
		}
	}

	conn.reset()
	messages = runSimple(t, c, conn, `SELECT '{1,2}'::INTEGER[] AS arr`)
	rd = findRowDescription(t, messages)
	if oid := rowDescriptionOIDs(t, rd.Data)[0]; oid != 1007 {
		t.Fatalf("cast array OID = %d, want 1007", oid)
	}
	if row := firstDataRow(t, messages); row[0] != "{1,2}" {
		t.Fatalf("cast array text = %v, want {1,2}", row)
	}
}

// A binary int4[] parameter is decoded from PostgreSQL's array envelope, so
// drivers that always encode arrays in binary (pgx) can bind ANY($1).
func TestArrayParameterBinaryFormatAccepted(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY)`,
		`INSERT INTO t VALUES (1), (2), (3)`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	parse := NewMessageBuilder()
	parse.WriteString("s1")
	parse.WriteString(`SELECT id FROM t WHERE id = ANY($1) ORDER BY id`)
	parse.WriteInt16(1)
	parse.WriteInt32(1007) // int4[]

	value := encodeBinaryArray(23, [][]byte{encodeInt4(1), encodeInt4(3)})
	bind := NewMessageBuilder()
	bind.WriteString("p1")
	bind.WriteString("s1")
	bind.WriteInt16(1) // one parameter format code
	bind.WriteInt16(1) // binary
	bind.WriteInt16(1) // one parameter
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
	messages := readAllMessages(t, conn.bytes())
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("binary array parameter failed: %s", errorCode(errMsg))
	}
	rows := [][]string{}
	for _, message := range messages {
		if message.Type == MsgDataRow {
			rows = append(rows, dataRowValues(t, message))
		}
	}
	if len(rows) != 2 || rows[0][0] != "1" || rows[1][0] != "3" {
		t.Fatalf("binary ANY($1) rows = %v, want [[1] [3]]", rows)
	}
}

func TestMalformedArrayParameterReportsInvalidText(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY)`,
		`INSERT INTO t VALUES (1)`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	messages := runExtendedParseBindExecute(t, c, conn, `SELECT id FROM t WHERE id = ANY($1)`, 1007, "{1,2")
	errMsg := findError(messages)
	if errMsg == nil {
		t.Fatal("malformed array parameter was accepted")
	}
	if code := errorCode(errMsg); code != ErrCodeInvalidText {
		t.Fatalf("malformed array error code = %q, want %q", code, ErrCodeInvalidText)
	}

	conn.reset()
	messages = runExtendedParseBindExecute(t, c, conn, `SELECT id FROM t WHERE id = ANY($1)`, 1007, "{not-an-int}")
	errMsg = findError(messages)
	if errMsg == nil {
		t.Fatal("non-integer element was accepted for int4[]")
	}
	if code := errorCode(errMsg); code != ErrCodeInvalidText {
		t.Fatalf("invalid element error code = %q, want %q", code, ErrCodeInvalidText)
	}
}

// reset discards captured output so one test can inspect several responses.
func (c *countingConn) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

// A parameter inside a subscript must bind: brackets are ordinary syntax, not a
// quoted region.
func TestArraySubscriptParameterBinding(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, tags TEXT[])`,
		`INSERT INTO t VALUES (1, '{a,b,c}')`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	messages := runExtendedParseBindExecute(t, c, conn, `SELECT tags[$1] FROM t`, 23, "2")
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("tags[$1] failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "b" {
		t.Fatalf("tags[$1] row = %v, want [b]", row)
	}

	// An untyped parameter binds the same way.
	conn.reset()
	messages = runExtendedParseBindExecute(t, c, conn, `SELECT tags[$1] FROM t`, 0, "3")
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("untyped tags[$1] failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "c" {
		t.Fatalf("untyped tags[$1] row = %v, want [c]", row)
	}
}

// Quotes and backslashes inside a bound array element survive canonicalization,
// SQL-literal interpolation, casting, and storage unchanged.
func TestArrayParameterEscapedElements(t *testing.T) {
	c, conn := arrayConnection(t)
	if _, err := c.executor.Execute(parseStmt(t, `CREATE TABLE t (id INTEGER PRIMARY KEY, tags TEXT[])`)); err != nil {
		t.Fatal(err)
	}
	const arrayText = "{\"it's\",\"back\\\\slash\"}"
	const canonical = "{it's,\"back\\\\slash\"}"

	messages := runExtendedParseBindExecute(t, c, conn, `SELECT $1::TEXT[]`, 1009, arrayText)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("escaped array cast failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != canonical {
		t.Fatalf("escaped array text = %v, want %s", row, canonical)
	}

	conn.reset()
	messages = runExtendedParseBindExecute(t, c, conn, `INSERT INTO t VALUES (1, $1) RETURNING tags`, 1009, arrayText)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("escaped array insert failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != canonical {
		t.Fatalf("escaped array insert = %v, want %s", row, canonical)
	}
}

func TestNumericArrayCastOID(t *testing.T) {
	c, conn := arrayConnection(t)
	messages := runSimple(t, c, conn, `SELECT '{1.10,12345678901234567890}'::NUMERIC[] AS amounts`)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("numeric array cast failed: %s", errorCode(errMsg))
	}
	rd := findRowDescription(t, messages)
	if oid := rowDescriptionOIDs(t, rd.Data)[0]; oid != 1231 {
		t.Fatalf("numeric array OID = %d, want 1231", oid)
	}
	if row := firstDataRow(t, messages); row[0] != "{1.10,12345678901234567890}" {
		t.Fatalf("numeric array value = %v", row[0])
	}
}

func TestArrayIndexTypeSQLState(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, nums INTEGER[])`,
		`INSERT INTO t VALUES (1, '{10,20}')`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, sql := range []string{
		`SELECT nums[1.5] FROM t`,
		`SELECT nums[TRUE] FROM t`,
		`SELECT nums['x'] FROM t`,
		`SELECT array_length(nums, 1.5) FROM t`,
	} {
		conn.reset()
		messages := runSimple(t, c, conn, sql)
		errMsg := findError(messages)
		if errMsg == nil {
			t.Errorf("%s was accepted", sql)
			continue
		}
		if code := errorCode(errMsg); code != ErrCodeDatatypeMismatch {
			t.Errorf("%s SQLSTATE = %q, want %q", sql, code, ErrCodeDatatypeMismatch)
		}
	}
}

func TestBooleanSubscriptPredicateOverSimpleQuery(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, flags BOOLEAN[])`,
		`INSERT INTO t VALUES (1, '{t,f}'), (2, '{f,t}')`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, tc := range []struct{ sql, want string }{
		{`SELECT count(*) FROM t WHERE flags[1]`, "1"},
		{`SELECT count(*) FROM t WHERE NOT flags[1]`, "1"},
		{`SELECT count(*) FROM t WHERE flags[1] = TRUE`, "1"},
	} {
		conn.reset()
		messages := runSimple(t, c, conn, tc.sql)
		if errMsg := findError(messages); errMsg != nil {
			t.Fatalf("%s failed: %s", tc.sql, errorCode(errMsg))
		}
		if row := firstDataRow(t, messages); len(row) != 1 || row[0] != tc.want {
			t.Errorf("%s = %v, want %s", tc.sql, row, tc.want)
		}
	}
}
