package pgserver

import (
	"bufio"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
)

// paramTypeConnection returns a connection whose catalog has the tables the
// inference cases reference.
func paramTypeConnection(t *testing.T) *Connection {
	t.Helper()
	c, _ := arrayConnection(t)
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, big BIGINT, name TEXT, doc JSONB)`,
		`CREATE TABLE arr (tags TEXT[], nums INTEGER[])`,
	} {
		if _, err := c.executor.Execute(parseStmt(t, sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return c
}

// negotiatedOIDs returns negotiateParameterOIDs or fails the test.
func negotiatedOIDs(t *testing.T, c *Connection, query string, declared []int32) []int32 {
	t.Helper()
	got, err := c.negotiateParameterOIDs(query, declared)
	if err != nil {
		t.Fatalf("negotiateParameterOIDs(%q): %v", query, err)
	}
	return got
}

func TestNegotiatedParameterOIDs(t *testing.T) {
	c := paramTypeConnection(t)
	cases := []struct {
		name     string
		query    string
		declared []int32
		want     []int32
	}{
		{"cast_integer", `SELECT $1::INTEGER`, nil, []int32{23}},
		{"cast_text_array", `SELECT $1::TEXT[]`, nil, []int32{1009}},
		{"cast_jsonb", `SELECT $1::JSONB`, nil, []int32{3802}},
		{"comparison_integer", `SELECT id FROM t WHERE id = $1`, nil, []int32{23}},
		{"comparison_bigint", `SELECT id FROM t WHERE big = $1`, nil, []int32{20}},
		{"comparison_reversed", `SELECT id FROM t WHERE $1 = id`, nil, []int32{23}},
		{"comparison_jsonb", `SELECT id FROM t WHERE doc = $1`, nil, []int32{3802}},
		{"jsonb_containment", `SELECT id FROM t WHERE doc @> $1`, nil, []int32{3802}},
		{"insert_column_list", `INSERT INTO t (name) VALUES ($1)`, nil, []int32{25}},
		{"insert_positional", `INSERT INTO t VALUES ($1, $2, $3, $4)`, nil, []int32{23, 20, 25, 3802}},
		{"update_assignment", `UPDATE t SET doc = $1 WHERE id = $2`, nil, []int32{3802, 23}},
		{"any_integer", `SELECT id FROM t WHERE id = ANY($1)`, nil, []int32{1007}},
		{"any_bigint", `SELECT id FROM t WHERE big = ANY($1)`, nil, []int32{1016}},
		{"all_text", `SELECT id FROM t WHERE name <> ALL($1)`, nil, []int32{1009}},
		{"subscript_integer", `SELECT tags[$1] FROM arr`, nil, []int32{23}},
		{"cast_beats_column", `SELECT id FROM t WHERE id = $1::TEXT`, nil, []int32{25}},
		{"expr_between", `SELECT id FROM t WHERE id BETWEEN $1 AND $2`, nil, []int32{23, 23}},
		{"in_list", `SELECT id FROM t WHERE id IN ($1, $2)`, nil, []int32{23, 23}},
		{"unresolvable", `SELECT $1`, nil, []int32{25}},
		{"unknown_table", `SELECT id FROM missing WHERE id = $1`, nil, []int32{25}},
		{"declared_wins", `SELECT id FROM t WHERE id = $1`, []int32{20}, []int32{20}},
		{"declared_fills_zero", `SELECT id FROM t WHERE big = $1 AND id = $2`, []int32{20, 0}, []int32{20, 23}},
		{"declared_extra_kept", `SELECT 1`, []int32{20, 21}, []int32{20, 21}},
		{"placeholder_in_literal", `SELECT '$1', 1`, nil, []int32{}},
		{"placeholder_in_comment", `SELECT id FROM t WHERE id = 1 -- $1`, nil, []int32{}},
		{"first_occurrence_wins", `SELECT id FROM t WHERE id = $1 OR big = $1`, nil, []int32{23}},
		{"array_column_any", `SELECT tags FROM arr WHERE tags = ANY($1)`, nil, []int32{1009}},
		{"array_containment", `SELECT id FROM arr WHERE nums @> $1`, nil, []int32{1007}},
		{"array_containment_reversed", `SELECT id FROM arr WHERE $1 <@ nums`, nil, []int32{1007}},
		{"limit_bigint", `SELECT id FROM t LIMIT $1 OFFSET $2`, nil, []int32{20, 20}},
		{"join_condition", `SELECT t.id FROM t JOIN arr ON t.big = $1`, nil, []int32{20}},
		{"join_subquery_body", `SELECT t.id FROM t JOIN (SELECT id FROM arr WHERE nums[1] = $1) AS x ON x.id = t.id`, nil, []int32{23}},
		{"derived_table_body", `SELECT x.id FROM (SELECT id FROM t WHERE big = $1) AS x`, nil, []int32{20}},
		{"subscript_comparison", `SELECT id FROM arr WHERE nums[1] = $1`, nil, []int32{23}},
		{"ten_parameters", `SELECT $10::BIGINT, $1::INTEGER`, nil, []int32{23, 25, 25, 25, 25, 25, 25, 25, 25, 20}},
		{"declared_conflicts_with_cast", `SELECT $1::INTEGER, id FROM t WHERE id = $2`, []int32{25, 0}, []int32{25, 23}},
		{"declared_conflicts_with_comparison", `SELECT name FROM t WHERE name = $1 AND id = $2`, []int32{23, 0}, []int32{23, 23}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := negotiatedOIDs(t, c, tc.query, tc.declared)
			if len(got) != len(tc.want) {
				t.Fatalf("negotiateParameterOIDs(%q) = %v, want %v", tc.query, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("negotiateParameterOIDs(%q) = %v, want %v", tc.query, got, tc.want)
				}
				if got[i] == 0 {
					t.Fatalf("negotiateParameterOIDs(%q)[%d] = 0; a parameter type must never be unknown", tc.query, i)
				}
			}
		})
	}
}

// A statement the parser rejects still gets a parameter count from the
// placeholder scan, and every entry falls back to TEXT.
func TestNegotiatedParameterOIDsUnparsableStatement(t *testing.T) {
	c := paramTypeConnection(t)
	got := negotiatedOIDs(t, c, `SELEC $1, $2`, nil)
	if len(got) != 2 || got[0] != 25 || got[1] != 25 {
		t.Fatalf("unparsable statement OIDs = %v, want [25 25]", got)
	}
}

func parameterDescriptionOIDs(t *testing.T, msg *Message) []int32 {
	t.Helper()
	if msg.Type != MsgParameterDescription {
		t.Fatalf("message type = %c, want ParameterDescription", msg.Type)
	}
	if len(msg.Data) < 2 {
		t.Fatal("short ParameterDescription")
	}
	count := int(binary.BigEndian.Uint16(msg.Data[:2]))
	if len(msg.Data) != 2+count*4 {
		t.Fatalf("ParameterDescription has %d bytes for %d parameters", len(msg.Data), count)
	}
	oids := make([]int32, count)
	for i := range oids {
		oids[i] = int32(binary.BigEndian.Uint32(msg.Data[2+i*4:]))
	}
	return oids
}

func findParameterDescription(t *testing.T, messages []*Message) []int32 {
	t.Helper()
	for _, message := range messages {
		if message.Type == MsgParameterDescription {
			return parameterDescriptionOIDs(t, message)
		}
	}
	t.Fatal("no ParameterDescription message found")
	return nil
}

// Statement Describe reports the negotiated parameter types; portal Describe
// must not, matching PostgreSQL, which sends ParameterDescription only for
// Describe('S').
func TestDescribeReportsInferredParameterOIDs(t *testing.T) {
	c := paramTypeConnection(t)
	conn := &countingConn{}
	c.conn = conn
	c.writer = bufio.NewWriter(conn)

	parse := NewMessageBuilder()
	parse.WriteString("s1")
	parse.WriteString(`SELECT name FROM t WHERE id = $1`)
	parse.WriteInt16(0)
	if err := c.handleMessage(&Message{Type: MsgParse, Data: parse.Bytes()}); err != nil {
		t.Fatal(err)
	}

	describeStatement := NewMessageBuilder()
	describeStatement.WriteBytes([]byte("S"))
	describeStatement.WriteString("s1")
	if err := c.handleMessage(&Message{Type: MsgDescribe, Data: describeStatement.Bytes()}); err != nil {
		t.Fatal(err)
	}
	if err := c.writer.Flush(); err != nil {
		t.Fatal(err)
	}
	statementMessages := readAllMessages(t, conn.bytes())
	statementOIDs := findParameterDescription(t, statementMessages)
	if len(statementOIDs) != 1 || statementOIDs[0] != 23 {
		t.Fatalf("statement ParameterDescription = %v, want [23]", statementOIDs)
	}
	if !hasMessageType(statementMessages, MsgNoData) {
		t.Fatal("statement Describe did not send NoData after ParameterDescription")
	}

	conn.reset()
	bind := NewMessageBuilder()
	bind.WriteString("p1")
	bind.WriteString("s1")
	bind.WriteInt16(0)
	bind.WriteInt16(1)
	bind.WriteInt32(1)
	bind.WriteBytes([]byte("1"))
	bind.WriteInt16(0)
	if err := c.handleMessage(&Message{Type: MsgBind, Data: bind.Bytes()}); err != nil {
		t.Fatal(err)
	}
	describePortal := NewMessageBuilder()
	describePortal.WriteBytes([]byte("P"))
	describePortal.WriteString("p1")
	if err := c.handleMessage(&Message{Type: MsgDescribe, Data: describePortal.Bytes()}); err != nil {
		t.Fatal(err)
	}
	if err := c.writer.Flush(); err != nil {
		t.Fatal(err)
	}
	portalMessages := readAllMessages(t, conn.bytes())
	if hasMessageType(portalMessages, MsgParameterDescription) {
		t.Fatalf("portal Describe sent ParameterDescription: %v", portalMessages)
	}
	if !hasMessageType(portalMessages, MsgNoData) {
		t.Fatal("portal Describe did not send NoData")
	}
}

func hasMessageType(messages []*Message, messageType byte) bool {
	for _, message := range messages {
		if message.Type == messageType {
			return true
		}
	}
	return false
}

// The negotiated type is what Bind decodes with: a text-format parameter that
// used to be sniffed from its spelling now follows the described type.
func TestBindUsesNegotiatedParameterOID(t *testing.T) {
	c := paramTypeConnection(t)
	conn := &countingConn{}
	c.conn = conn
	c.writer = bufio.NewWriter(conn)
	if _, err := c.executor.Execute(parseStmt(t, `INSERT INTO t (id, name) VALUES (5, '007')`)); err != nil {
		t.Fatal(err)
	}
	// The comparison names a TEXT column, so "007" binds as quoted text. An
	// untyped parameter would have been sniffed as a number and matched nothing.
	messages := runExtendedParseBindExecute(t, c, conn, `SELECT name FROM t WHERE name = $1`, 0, "007")
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("bind failed: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "007" {
		t.Fatalf("row = %v, want [007]", row)
	}
}

// DDL staged by the current transaction is visible to inference, exactly as it
// is to the analyzer and executor.
func TestNegotiatedParameterOIDsSeeStagedDDL(t *testing.T) {
	c := paramTypeConnection(t)
	if _, err := c.executor.Execute(parseStmt(t, `BEGIN`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.executor.Execute(parseStmt(t, `CREATE TABLE staged (id INTEGER PRIMARY KEY, flag BOOLEAN)`)); err != nil {
		t.Fatal(err)
	}
	got := negotiatedOIDs(t, c, `SELECT id FROM staged WHERE id = $1 AND flag = $2`, nil)
	if want := []int32{23, 16}; !reflect.DeepEqual(got, want) {
		t.Fatalf("staged DDL parameter OIDs = %v, want %v", got, want)
	}
}

// parseQuery sends Parse with the declared OID list, flushes, returns the
// messages it produced, and closes the extended cycle with Sync so the next
// case starts from an idle connection.
func parseQuery(t *testing.T, c *Connection, conn *countingConn, name, query string, declared []int32) []*Message {
	t.Helper()
	conn.reset()
	parse := NewMessageBuilder()
	parse.WriteString(name)
	parse.WriteString(query)
	parse.WriteInt16(int16(len(declared)))
	for _, oid := range declared {
		parse.WriteInt32(oid)
	}
	if err := c.handleMessage(&Message{Type: MsgParse, Data: parse.Bytes()}); err != nil {
		t.Fatalf("handle Parse %q: %v", query, err)
	}
	if err := c.writer.Flush(); err != nil {
		t.Fatalf("flush Parse %q: %v", query, err)
	}
	messages := readAllMessages(t, conn.bytes())
	if err := c.handleMessage(&Message{Type: MsgSync}); err != nil {
		t.Fatalf("handle Sync after %q: %v", query, err)
	}
	if err := c.writer.Flush(); err != nil {
		t.Fatalf("flush Sync after %q: %v", query, err)
	}
	conn.reset()
	return messages
}

// A placeholder number above PostgreSQL's 65535-parameter protocol limit is
// rejected before the type list is sized, so a hostile Parse cannot allocate
// unbounded memory or overflow the int conversion.
func TestParameterNumberAboveProtocolLimitRejected(t *testing.T) {
	c := paramTypeConnection(t)
	conn := &countingConn{}
	c.conn = conn
	c.writer = bufio.NewWriter(conn)

	for _, query := range []string{
		`SELECT $65536`,
		`SELECT id FROM t WHERE id = $65536`,
		`SELECT $9223372036854775807`,
		`SELECT $99999999999999999999999999`,
	} {
		t.Run(query, func(t *testing.T) {
			messages := parseQuery(t, c, conn, "limit", query, nil)
			errMsg := findError(messages)
			if errMsg == nil {
				t.Fatalf("%s was accepted", query)
			}
			if code := errorCode(errMsg); code != ErrCodeProgramLimitExceeded {
				t.Fatalf("%s SQLSTATE = %q, want %q", query, code, ErrCodeProgramLimitExceeded)
			}
			if message := errorMessage(errMsg); !strings.Contains(message, "65535") {
				t.Fatalf("%s message = %q, want the protocol limit", query, message)
			}
			if _, stored := c.statements["limit"]; stored {
				t.Fatalf("%s stored a statement despite the rejection", query)
			}
		})
	}
}

// A statement at the limit is accepted and its ParameterDescription is a
// well-formed frame: the 16-bit count field holds 32768 and the payload is
// exactly 2 + count*4 bytes.
func TestParameterDescriptionAtProtocolLimit(t *testing.T) {
	c := paramTypeConnection(t)
	conn := &countingConn{}
	c.conn = conn
	c.writer = bufio.NewWriter(conn)

	if messages := parseQuery(t, c, conn, "wide", `SELECT $32768`, nil); findError(messages) != nil {
		t.Fatalf("Parse of $32768 failed: %s", errorCode(findError(messages)))
	}
	describe := NewMessageBuilder()
	describe.WriteBytes([]byte("S"))
	describe.WriteString("wide")
	if err := c.handleMessage(&Message{Type: MsgDescribe, Data: describe.Bytes()}); err != nil {
		t.Fatal(err)
	}
	if err := c.writer.Flush(); err != nil {
		t.Fatal(err)
	}
	messages := readAllMessages(t, conn.bytes())
	var description *Message
	for _, message := range messages {
		if message.Type == MsgParameterDescription {
			description = message
		}
	}
	if description == nil {
		t.Fatal("Describe did not send ParameterDescription")
	}
	count := int(binary.BigEndian.Uint16(description.Data[:2]))
	if count != 32768 {
		t.Fatalf("ParameterDescription count = %d, want 32768", count)
	}
	if len(description.Data) != 2+count*4 {
		t.Fatalf("ParameterDescription payload = %d bytes, want %d", len(description.Data), 2+count*4)
	}
	if last := int32(binary.BigEndian.Uint32(description.Data[len(description.Data)-4:])); last != 25 {
		t.Fatalf("last parameter OID = %d, want TEXT (25)", last)
	}
	if err := c.handleMessage(&Message{Type: MsgSync}); err != nil {
		t.Fatal(err)
	}
}
