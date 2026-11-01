package executor

import "testing"

func TestDisabledIndexFallsBackRatherThanReturningEmpty(t *testing.T) {
	_, schemas, tables := newTestDB(t)
	e := newExec(schemas, tables)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)")
	execMust(t, e, "INSERT INTO t VALUES (1,7)")
	execMust(t, e, "CREATE INDEX idx ON t(v)")
	execMust(t, e, "SELECT id FROM t WHERE v=7")
	// DROP clears derived entries before removing metadata. A concurrent SELECT
	// in that interval must fall back, not mistake an unavailable cache for empty.
	if err := tables.ClearIndex("idx", "t", []string{"v"}); err != nil {
		t.Fatal(err)
	}
	if got := execMust(t, e, "SELECT id FROM t WHERE v=7").Rows; len(got) != 1 || got[0][0] != int64(1) {
		t.Fatalf("disabled cache changed query result: %v", got)
	}
}
