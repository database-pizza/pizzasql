package pgserver

import (
	"bytes"
	"encoding/binary"
	"net"
	"strconv"
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
		// Integer types narrower than the int64 carrier report int8 because the
		// engine does not range-check stored values.
		"SMALLINT":  20,
		"INT2":      20,
		"TINYINT":   20,
		"MEDIUMINT": 20,
		"INTEGER":   23,
		"INT":       23,
		"INT4":      23,
		"BIGINT":    20,
		"INT8":      20,
		// NUMERIC/DECIMAL carry float64 values, so they report float8.
		"NUMERIC": 701,
		"DECIMAL": 701,
		"REAL":    700,
		"FLOAT":   700,
		"DOUBLE":  701,
		"TEXT":    25,
		"VARCHAR": 25,
		"CHAR":    25,
		"UUID":    25,
		"BOOLEAN": 16,
		"BOOL":    16,
		"BLOB":    17,
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
		"SMALLINT":  8,
		"TINYINT":   8,
		"MEDIUMINT": 8,
		"INTEGER":   4,
		"BIGINT":    8,
		"REAL":      4,
		"DOUBLE":    8,
		"NUMERIC":   8,
		"DECIMAL":   8,
		"BOOLEAN":   1,
		"DATE":      4,
		"TIMESTAMP": 8,
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
			want: []int32{23, 20, 20, 701, 16, 25, 17, 1184, 1082},
		},
		{name: "count star", sql: "SELECT count(*) FROM typed", want: []int32{20}},
		{name: "sum integer", sql: "SELECT sum(i) FROM typed", want: []int32{20}},
		{name: "sum numeric", sql: "SELECT sum(price) FROM typed", want: []int32{701}},
		{name: "avg integer", sql: "SELECT avg(i) FROM typed", want: []int32{701}},
		{name: "comparison", sql: "SELECT i = 1 FROM typed", want: []int32{16}},
		{name: "string literal", sql: "SELECT 'text'", want: []int32{25}},
		{name: "integer literal", sql: "SELECT 42", want: []int32{23}},
		{name: "wide integer literal", sql: "SELECT 4294967296", want: []int32{20}},
		{name: "numeric literal", sql: "SELECT 1.5", want: []int32{701}},
		{name: "cast", sql: "SELECT CAST(i AS BIGINT) FROM typed", want: []int32{20}},
		{name: "narrow integer cast", sql: "SELECT CAST(i AS SMALLINT) FROM typed", want: []int32{20}},
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

// rowDescriptionColumns decodes the column names from a RowDescription message.
func rowDescriptionColumns(t *testing.T, data []byte) []string {
	t.Helper()
	if len(data) < 2 {
		t.Fatalf("row description too short")
	}
	count := int(binary.BigEndian.Uint16(data[:2]))
	pos := 2
	names := make([]string, 0, count)
	for i := 0; i < count; i++ {
		end := bytes.IndexByte(data[pos:], 0)
		if end < 0 {
			t.Fatalf("unterminated column name")
		}
		names = append(names, string(data[pos:pos+end]))
		pos += end + 1 // name
		pos += 4       // table OID
		pos += 2       // column attribute number
		pos += 4       // type OID
		pos += 2       // type size
		pos += 4       // type modifier
		pos += 2       // format code
	}
	return names
}

// countMessageType counts messages of a type in a query response.
func countMessageType(msgs []*Message, msgType byte) int {
	count := 0
	for _, msg := range msgs {
		if msg.Type == msgType {
			count++
		}
	}
	return count
}

// TestFloatCarrierOIDsMatchValues asserts that projections carried as float64
// are advertised as float8 and that their text values parse as float8. NUMERIC
// (1700) is never advertised because the engine has no exact decimal carrier.
func TestFloatCarrierOIDsMatchValues(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(
		`CREATE TABLE floats (i INTEGER PRIMARY KEY, price NUMERIC, amount DECIMAL, score DOUBLE)`), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(
		`INSERT INTO floats VALUES (1, 19.5, 3.25, 0.1)`), 0)})

	cases := []struct {
		name string
		sql  string
	}{
		{name: "numeric column", sql: "SELECT price FROM floats"},
		{name: "decimal column", sql: "SELECT amount FROM floats"},
		{name: "double column", sql: "SELECT score FROM floats"},
		{name: "avg integer", sql: "SELECT avg(i) FROM floats"},
		{name: "avg numeric", sql: "SELECT avg(price) FROM floats"},
		{name: "sum numeric", sql: "SELECT sum(price) FROM floats"},
		{name: "decimal literal", sql: "SELECT 1.5"},
		{name: "float arithmetic", sql: "SELECT price + 0.5 FROM floats"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := resultMessages(runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(tc.sql), 0)}))
			oids := rowDescriptionOIDs(t, findRowDescription(t, msgs).Data)
			if len(oids) != 1 {
				t.Fatalf("%s: OIDs = %v, want one column", tc.sql, oids)
			}
			if oids[0] == 1700 {
				t.Fatalf("%s: OID = 1700 (numeric) for a float64 carrier", tc.sql)
			}
			if oids[0] != 701 {
				t.Fatalf("%s: OID = %d, want 701 (float8)", tc.sql, oids[0])
			}
			values := dataRowValues(t, msgs[1])
			if len(values) != 1 {
				t.Fatalf("%s: values = %v, want one value", tc.sql, values)
			}
			if _, err := strconv.ParseFloat(values[0], 64); err != nil {
				t.Fatalf("%s: float8 value %q does not parse: %v", tc.sql, values[0], err)
			}
		})
	}
}

// TestNarrowIntegerOIDsUseCarrierType pins the chosen narrow-integer behavior:
// the engine carries int64 without range checks, so SMALLINT/TINYINT/MEDIUMINT
// report int8 (20) rather than a width a stored value may not fit.
func TestNarrowIntegerOIDsUseCarrierType(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(
		`CREATE TABLE narrow (id INTEGER PRIMARY KEY, small SMALLINT, tiny TINYINT, medium MEDIUMINT)`), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(
		`INSERT INTO narrow VALUES (1, 40000, 200, 9000000)`), 0)})

	cases := []struct {
		name  string
		sql   string
		value string
	}{
		{name: "smallint column", sql: "SELECT small FROM narrow", value: "40000"},
		{name: "tinyint column", sql: "SELECT tiny FROM narrow", value: "200"},
		{name: "mediumint column", sql: "SELECT medium FROM narrow", value: "9000000"},
		{name: "cast smallint", sql: "SELECT CAST(small AS SMALLINT) FROM narrow", value: "40000"},
		{name: "cast integer", sql: "SELECT CAST(small AS INTEGER) FROM narrow", value: "40000"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := resultMessages(runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(tc.sql), 0)}))
			oids := rowDescriptionOIDs(t, findRowDescription(t, msgs).Data)
			if len(oids) != 1 || oids[0] != 20 {
				t.Fatalf("%s: OIDs = %v, want [20] (int8 carrier)", tc.sql, oids)
			}
			values := dataRowValues(t, msgs[1])
			if len(values) != 1 || values[0] != tc.value {
				t.Fatalf("%s: values = %v, want [%s]", tc.sql, values, tc.value)
			}
			if _, err := strconv.ParseInt(values[0], 10, 64); err != nil {
				t.Fatalf("%s: int8 value %q does not parse: %v", tc.sql, values[0], err)
			}
		})
	}
}

// TestEmptyResultWildcardMatchesNames guards the constant-false WHERE path:
// wildcard projections must expand names and types into the same columns.
func TestEmptyResultWildcardMatchesNames(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(
		`CREATE TABLE star_t (id INTEGER PRIMARY KEY, created_at TIMESTAMP, name TEXT)`), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(
		`CREATE TABLE star_b (b_id BIGINT PRIMARY KEY, score DOUBLE)`), 0)})

	cases := []struct {
		name      string
		sql       string
		wantNames []string
		wantOIDs  []int32
	}{
		{
			name:      "qualified star",
			sql:       "SELECT star_t.* FROM star_t WHERE 1 = 0",
			wantNames: []string{"id", "created_at", "name"},
			wantOIDs:  []int32{23, 1184, 25},
		},
		{
			name:      "aliased qualified star",
			sql:       "SELECT s.* FROM star_t AS s WHERE 1 = 0",
			wantNames: []string{"id", "created_at", "name"},
			wantOIDs:  []int32{23, 1184, 25},
		},
		{
			name:      "star",
			sql:       "SELECT * FROM star_t WHERE 1 = 0",
			wantNames: []string{"id", "created_at", "name"},
			wantOIDs:  []int32{23, 1184, 25},
		},
		{
			name:      "column and expression",
			sql:       "SELECT star_t.name, star_t.id + 1 FROM star_t WHERE 1 = 0",
			wantNames: []string{"name", "column2"},
			wantOIDs:  []int32{25, 20},
		},
		{
			name:      "join star",
			sql:       "SELECT * FROM star_t JOIN star_b ON star_t.id = star_b.b_id WHERE 1 = 0",
			wantNames: []string{"id", "created_at", "name", "b_id", "score"},
			wantOIDs:  []int32{23, 1184, 25, 20, 701},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := resultMessages(runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(tc.sql), 0)}))
			rd := findRowDescription(t, msgs)
			names := rowDescriptionColumns(t, rd.Data)
			oids := rowDescriptionOIDs(t, rd.Data)
			if len(names) != len(tc.wantNames) {
				t.Fatalf("%s: names = %v, want %v", tc.sql, names, tc.wantNames)
			}
			for i := range tc.wantNames {
				if names[i] != tc.wantNames[i] {
					t.Fatalf("%s: names = %v, want %v", tc.sql, names, tc.wantNames)
				}
			}
			if len(oids) != len(tc.wantOIDs) {
				t.Fatalf("%s: OIDs = %v, want %v", tc.sql, oids, tc.wantOIDs)
			}
			for i := range tc.wantOIDs {
				if oids[i] != tc.wantOIDs[i] {
					t.Fatalf("%s: OIDs = %v, want %v", tc.sql, oids, tc.wantOIDs)
				}
			}
			if rows := countMessageType(msgs, MsgDataRow); rows != 0 {
				t.Fatalf("%s: got %d data rows, want 0", tc.sql, rows)
			}
		})
	}
}

func TestGetOIDForTypeArrays(t *testing.T) {
	c := &Connection{}
	cases := map[string]int32{
		"TEXT[]":             1009,
		"VARCHAR[]":          1015,
		"VARCHAR(10)[]":      1015,
		"CHAR[]":             1009,
		"INTEGER[]":          1007,
		"INT[]":              1007,
		"BIGINT[]":           1016,
		"SMALLINT[]":         1016,
		"BOOLEAN[]":          1000,
		"BOOL[]":             1000,
		"REAL[]":             1021,
		"DOUBLE[]":           1022,
		"DOUBLE PRECISION[]": 1022,
		"NUMERIC[]":          1231,
	}
	for name, want := range cases {
		if got := c.getOIDForType(name); got != want {
			t.Errorf("getOIDForType(%q) = %d, want %d", name, got, want)
		}
		if size := c.getTypeSizeForType(name); size != -1 {
			t.Errorf("getTypeSizeForType(%q) = %d, want -1", name, size)
		}
	}
}
