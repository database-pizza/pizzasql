package executor

import "testing"

func TestOrderByNullsFirstLast(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, pos INTEGER)")
	execMust(t, e, "INSERT INTO t VALUES (1, NULL)")
	execMust(t, e, "INSERT INTO t VALUES (2, 5)")
	execMust(t, e, "INSERT INTO t VALUES (3, 1)")
	execMust(t, e, "INSERT INTO t VALUES (4, NULL)")

	ids := func(res *Result) []int64 {
		out := make([]int64, len(res.Rows))
		for i, row := range res.Rows {
			out[i] = row[0].(int64)
		}
		return out
	}

	got := ids(execMust(t, e, "SELECT id FROM t ORDER BY pos ASC NULLS LAST, id ASC"))
	want := []int64{3, 2, 1, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NULLS LAST order = %v, want %v", got, want)
		}
	}

	got = ids(execMust(t, e, "SELECT id FROM t ORDER BY pos ASC NULLS FIRST, id ASC"))
	want = []int64{1, 4, 3, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NULLS FIRST order = %v, want %v", got, want)
		}
	}

	// Default SQLite ordering: NULLs are smallest.
	got = ids(execMust(t, e, "SELECT id FROM t ORDER BY pos ASC, id ASC"))
	want = []int64{1, 4, 3, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("default ASC order = %v, want %v", got, want)
		}
	}
}
