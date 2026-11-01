package pgserver

import "testing"

func TestIndexedEqualityPreservesSQLNullAndFloatSemantics(t *testing.T) {
	c, client := newDataConnection(t)
	for _, sql := range []string{
		"CREATE TABLE t (id INTEGER PRIMARY KEY, v REAL)",
		"INSERT INTO t VALUES (1,1.0000001),(2,1.0000002),(3,NULL)",
		"CREATE INDEX t_v ON t(v)",
	} {
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(sql), 0)})
	}
	for _, tc := range []struct {
		sql  string
		rows int
	}{
		{"SELECT id FROM t WHERE v=1.0000001", 1},
		{"SELECT id FROM t WHERE v=NULL", 0},
	} {
		msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(tc.sql), 0)})
		count := 0
		for _, msg := range msgs {
			if msg.Type == MsgErrorResponse {
				t.Fatal(errorMessage(msg))
			}
			if msg.Type == MsgDataRow {
				count++
			}
		}
		if count != tc.rows {
			t.Fatalf("%s: got %d rows, want %d", tc.sql, count, tc.rows)
		}
	}
}
