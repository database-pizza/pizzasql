package executor

import "testing"

func TestGroupConcatAggregate(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER, name TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (1, 'a')")
	execMust(t, e, "INSERT INTO t VALUES (2, 'b')")
	execMust(t, e, "INSERT INTO t VALUES (3, NULL)")
	execMust(t, e, "INSERT INTO t VALUES (4, 'a')")

	tests := []struct {
		sql  string
		want interface{}
	}{
		{"SELECT GROUP_CONCAT(name) FROM t", "a,b,a"},
		{"SELECT GROUP_CONCAT(name, '|') FROM t", "a|b|a"},
		{"SELECT GROUP_CONCAT(DISTINCT name) FROM t", "a,b"},
		{"SELECT GROUP_CONCAT(id) FROM t", "1,2,3,4"},
		{"SELECT COALESCE(GROUP_CONCAT(name), 'empty') FROM t WHERE id = 99", "empty"},
		{"SELECT GROUP_CONCAT(name) FROM t WHERE id = 99", nil},
	}
	for _, tc := range tests {
		res := execMust(t, e, tc.sql)
		if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
			t.Fatalf("%s returned %#v, want one row with one column", tc.sql, res.Rows)
		}
		if res.Rows[0][0] != tc.want {
			t.Fatalf("%s = %#v, want %#v", tc.sql, res.Rows[0][0], tc.want)
		}
	}
}

func TestTotalAggregate(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (n INTEGER)")
	execMust(t, e, "INSERT INTO t VALUES (1)")
	execMust(t, e, "INSERT INTO t VALUES (2)")
	execMust(t, e, "INSERT INTO t VALUES (NULL)")

	res := execMust(t, e, "SELECT TOTAL(n) FROM t")
	if len(res.Rows) != 1 || res.Rows[0][0] != float64(3) {
		t.Fatalf("TOTAL(n) = %#v, want 3.0", res.Rows)
	}

	res = execMust(t, e, "SELECT TOTAL(n) FROM t WHERE n > 99")
	if len(res.Rows) != 1 || res.Rows[0][0] != float64(0) {
		t.Fatalf("empty TOTAL(n) = %#v, want 0.0", res.Rows)
	}
}
