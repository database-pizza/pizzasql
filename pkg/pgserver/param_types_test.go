package pgserver

import (
	"bufio"
	"encoding/binary"
	"reflect"
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.negotiateParameterOIDs(tc.query, tc.declared)
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
	got := c.negotiateParameterOIDs(`SELEC $1, $2`, nil)
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

// Describe reports the inferred types for a statement and for the portal bound
// from it, so a driver that describes either one sees the same list.
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
	statementOIDs := findParameterDescription(t, readAllMessages(t, conn.bytes()))
	if len(statementOIDs) != 1 || statementOIDs[0] != 23 {
		t.Fatalf("statement ParameterDescription = %v, want [23]", statementOIDs)
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
	portalOIDs := findParameterDescription(t, readAllMessages(t, conn.bytes()))
	if len(portalOIDs) != 1 || portalOIDs[0] != 23 {
		t.Fatalf("portal ParameterDescription = %v, want [23]", portalOIDs)
	}
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
	got := c.negotiateParameterOIDs(`SELECT id FROM staged WHERE id = $1 AND flag = $2`, nil)
	if want := []int32{23, 16}; !reflect.DeepEqual(got, want) {
		t.Fatalf("staged DDL parameter OIDs = %v, want %v", got, want)
	}
}
