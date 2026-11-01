package executor

import (
	"reflect"
	"testing"
)

func checkAtomicMutations(t *testing.T, e *Executor) {
	t.Helper()
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT, j TEXT)")
	execMust(t, e, `INSERT INTO t VALUES (1,'old','{"x":"new"}'),(2,'old','invalid')`)
	want := [][]interface{}{{int64(1), "old"}, {int64(2), "old"}}
	for _, sql := range []string{
		"UPDATE t SET v=json_extract(j,'$.x')",
		"UPDATE t SET v='new' WHERE json_extract(j,'$.x')='new'",
		"DELETE FROM t WHERE json_extract(j,'$.x')='new'",
		"UPDATE t SET v='new' RETURNING json_extract(j,'$.x')",
		"DELETE FROM t RETURNING json_extract(j,'$.x')",
		"UPDATE t SET id=CASE WHEN id=1 THEN 3 ELSE 3 END",
	} {
		if _, err := execSQL(e, sql); err == nil {
			t.Fatalf("%s: expected failure", sql)
		}
		if got := execMust(t, e, "SELECT id,v FROM t ORDER BY id").Rows; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s left partial effects: %v", sql, got)
		}
	}
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO t VALUES (4,'before','{}')")
	if _, err := execSQL(e, "UPDATE t SET v=json_extract(j,'$.x') WHERE id<4"); err == nil {
		t.Fatal("expected failed in-transaction update")
	}
	execMust(t, e, "COMMIT")
	if got := execMust(t, e, "SELECT id,v FROM t WHERE id<4 ORDER BY id").Rows; !reflect.DeepEqual(got, want) {
		t.Fatalf("failed statement committed partial overlay: %v", got)
	}
	if got := execMust(t, e, "SELECT id FROM t WHERE id=4").RowCount; got != 1 {
		t.Fatal("earlier transaction work was lost")
	}
}

func TestAtomicMutationEvaluationAndReturning(t *testing.T) {
	_, schema, table := newTestDB(t)
	checkAtomicMutations(t, newExec(schema, table))
}

func TestAtomicMutationRealKV(t *testing.T) {
	e := newOptExec(t, "atomic_mutation_regressions")
	execSQL(e, "DROP TABLE IF EXISTS t")
	t.Cleanup(func() { execSQL(e, "DROP TABLE IF EXISTS t") })
	checkAtomicMutations(t, e)
}
