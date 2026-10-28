package pgserver

import "testing"

func TestGroupConcatAndTotalOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id INTEGER, name TEXT, n INTEGER)"), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO t VALUES (1, 'a', 1), (2, 'b', 2), (3, NULL, NULL), (4, 'a', 3)"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT GROUP_CONCAT(name), GROUP_CONCAT(name, '|'), GROUP_CONCAT(DISTINCT name), TOTAL(n) FROM t"), 0)})
	values := dataRowValues(t, msgs[1])
	want := []string{"a,b,a", "a|b|a", "a,b", "6"}
	if len(values) != len(want) {
		t.Fatalf("values = %v, want %v", values, want)
	}
	for i := range want {
		if values[i] != want[i] {
			t.Fatalf("values = %v, want %v", values, want)
		}
	}

	// Empty input: GROUP_CONCAT is NULL, TOTAL is 0.
	msgs = runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT GROUP_CONCAT(name), TOTAL(n) FROM t WHERE id = 99"), 0)})
	values = dataRowValues(t, msgs[1])
	if len(values) != 2 || values[0] != "<null>" || values[1] != "0" {
		t.Fatalf("empty aggregate values = %v, want [<null> 0]", values)
	}
}
