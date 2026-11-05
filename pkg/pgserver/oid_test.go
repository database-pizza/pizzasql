package pgserver

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
)

func TestGetOIDForTypeDatetime(t *testing.T) {
	c := &Connection{}
	cases := map[string]int32{
		"DATETIME":  1184,
		"TIMESTAMP": 1184,
		"DATE":      1082,
		"SMALLINT":  21,
		"INT2":      21,
		"TINYINT":   21,
		"MEDIUMINT": 23,
		"INTEGER":   23,
		"INT":       23,
		"INT4":      23,
		"BIGINT":    20,
		"INT8":      20,
		"NUMERIC":   1700,
		"DECIMAL":   1700,
		"REAL":      700,
		"FLOAT":     700,
		"DOUBLE":    701,
		"TEXT":      25,
		"VARCHAR":   25,
		"CHAR":      25,
		"UUID":      25,
		"BOOLEAN":   16,
		"BOOL":      16,
		"BLOB":      17,
	}
	for name, want := range cases {
		if got := c.getOIDForType(name); got != want {
			t.Errorf("getOIDForType(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestSendRowDescriptionTimestampOID(t *testing.T) {
	c, cc := newCountingTestConnection(t)
	if err := c.sendRowDescription([]string{"created_at", "id"}, []string{"TIMESTAMP", "BIGINT"}); err != nil {
		t.Fatal(err)
	}
	if err := c.sendReadyForQuery(); err != nil {
		t.Fatal(err)
	}
	msgs := readAllMessages(t, cc.bytes())
	rd := findRowDescription(t, msgs)
	oids := rowDescriptionOIDs(t, rd.Data)
	if len(oids) != 2 {
		t.Fatalf("got %d columns, want 2", len(oids))
	}
	if oids[0] != 1184 {
		t.Errorf("timestamp OID = %d, want 1184", oids[0])
	}
	if oids[1] != 20 {
		t.Errorf("bigint OID = %d, want 20", oids[1])
	}
}

func TestSendResultEmptyProjectionSendsRowDescription(t *testing.T) {
	c, cc := newCountingTestConnection(t)
	result := executor.NewResult("SELECT")
	result.AddColumnWithType("id", "BIGINT")
	result.AddColumnWithType("created_at", "DATETIME")
	if err := c.sendResult(result, &parser.SelectStmt{}); err != nil {
		t.Fatal(err)
	}
	if err := c.sendReadyForQuery(); err != nil {
		t.Fatal(err)
	}
	msgs := readAllMessages(t, cc.bytes())
	rd := findRowDescription(t, msgs)
	oids := rowDescriptionOIDs(t, rd.Data)
	if len(oids) != 2 || oids[0] != 20 || oids[1] != 1184 {
		t.Fatalf("oids = %v, want [20 1184]", oids)
	}
}

func findRowDescription(t *testing.T, msgs []*Message) *Message {
	t.Helper()
	for _, m := range msgs {
		if m.Type == MsgRowDescription {
			return m
		}
	}
	t.Fatal("no RowDescription message found")
	return nil
}

func rowDescriptionOIDs(t *testing.T, data []byte) []int32 {
	t.Helper()
	if len(data) < 2 {
		t.Fatalf("row description too short")
	}
	count := int(binary.BigEndian.Uint16(data[:2]))
	pos := 2
	oids := make([]int32, 0, count)
	for i := 0; i < count; i++ {
		end := bytes.IndexByte(data[pos:], 0)
		if end < 0 {
			t.Fatalf("unterminated column name")
		}
		pos += end + 1 // name
		pos += 4       // table OID
		pos += 2       // column attribute number
		if pos+4 > len(data) {
			t.Fatalf("truncated row description")
		}
		oids = append(oids, int32(binary.BigEndian.Uint32(data[pos:pos+4])))
		pos += 4 // type OID
		pos += 2 // type size
		pos += 4 // type modifier
		pos += 2 // format code
	}
	return oids
}

func TestGetTypeSizeForType(t *testing.T) {
	c := &Connection{}
	cases := map[string]int16{
		"SMALLINT":  2,
		"INTEGER":   4,
		"BIGINT":    8,
		"REAL":      4,
		"DOUBLE":    8,
		"BOOLEAN":   1,
		"DATE":      4,
		"TIMESTAMP": 8,
		"NUMERIC":   -1,
		"TEXT":      -1,
		"BLOB":      -1,
	}
	for name, want := range cases {
		if got := c.getTypeSizeForType(name); got != want {
			t.Errorf("getTypeSizeForType(%q) = %d, want %d", name, got, want)
		}
	}
}

// projectionOIDs runs sql and returns the type OIDs in its RowDescription.
func projectionOIDs(t *testing.T, c *Connection, client net.Conn, sql string) []int32 {
	t.Helper()
	msgs := resultMessages(runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(sql), 0)}))
	return rowDescriptionOIDs(t, findRowDescription(t, msgs).Data)
}

// TestRowDescriptionProjectionOIDs asserts the wire OIDs for column
// projections, expression projections, and empty result sets.
func TestRowDescriptionProjectionOIDs(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(
		`CREATE TABLE typed (
			i INTEGER PRIMARY KEY,
			big BIGINT,
			small SMALLINT,
			price NUMERIC,
			flag BOOLEAN,
			name TEXT,
			data BLOB,
			created_at TIMESTAMP,
			day DATE
		)`), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(
		`INSERT INTO typed VALUES (1, 9000000000, 7, 19.5, TRUE, 'x', X'00', '2024-05-06 07:08:09', '2024-05-06')`), 0)})

	cases := []struct {
		name string
		sql  string
		want []int32
	}{
		{
			name: "columns",
			sql:  "SELECT i, big, small, price, flag, name, data, created_at, day FROM typed",
			want: []int32{23, 20, 21, 1700, 16, 25, 17, 1184, 1082},
		},
		{name: "count star", sql: "SELECT count(*) FROM typed", want: []int32{20}},
		{name: "sum integer", sql: "SELECT sum(i) FROM typed", want: []int32{20}},
		{name: "avg integer", sql: "SELECT avg(i) FROM typed", want: []int32{1700}},
		{name: "comparison", sql: "SELECT i = 1 FROM typed", want: []int32{16}},
		{name: "string literal", sql: "SELECT 'text'", want: []int32{25}},
		{name: "integer literal", sql: "SELECT 42", want: []int32{23}},
		{name: "wide integer literal", sql: "SELECT 4294967296", want: []int32{20}},
		{name: "numeric literal", sql: "SELECT 1.5", want: []int32{1700}},
		{name: "cast", sql: "SELECT CAST(i AS BIGINT) FROM typed", want: []int32{20}},
		{
			name: "empty result",
			sql:  "SELECT i, created_at, i + 1 FROM typed WHERE i = 999",
			want: []int32{23, 1184, 20},
		},
		{
			name: "aggregate empty result",
			sql:  "SELECT count(*), max(created_at) FROM typed WHERE i = 999",
			want: []int32{20, 1184},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projectionOIDs(t, c, client, tc.sql)
			if len(got) != len(tc.want) {
				t.Fatalf("%s: OIDs = %v, want %v", tc.sql, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("%s: OIDs = %v, want %v", tc.sql, got, tc.want)
				}
			}
		})
	}
}
