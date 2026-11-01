package executor

import (
	"reflect"
	"testing"
)

func TestIndexPredicateMatchesScan(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v REAL, txt TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (1,1.0000001,'01'),(2,1.0000002,'1'),(3,NULL,'other')")
	queries := []string{
		"SELECT id FROM t WHERE v=1.0000001 ORDER BY id",
		"SELECT id FROM t WHERE v=NULL ORDER BY id",
		"SELECT id FROM t WHERE 1.0000002=v ORDER BY id",
		"SELECT id FROM t WHERE txt='1' ORDER BY id",
		"SELECT id FROM t WHERE id=NULL ORDER BY id",
		"SELECT id FROM t AS x WHERE x.v=1.0000001 ORDER BY id",
	}
	want := make([][][]interface{}, len(queries))
	for i, q := range queries {
		want[i] = execMust(t, e, q).Rows
	}
	execMust(t, e, "CREATE INDEX v_idx ON t(v)")
	execMust(t, e, "CREATE INDEX txt_idx ON t(txt)")
	for _, tx := range []bool{false, true} {
		if tx {
			execMust(t, e, "BEGIN")
		}
		for i, q := range queries {
			if got := execMust(t, e, q).Rows; !reflect.DeepEqual(got, want[i]) {
				t.Errorf("tx=%v %s: got %v want %v", tx, q, got, want[i])
			}
		}
		if tx {
			execMust(t, e, "ROLLBACK")
		}
	}
}

func TestQualifiedJoinPredicateDoesNotRestrictLeftTable(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE lhs (id INTEGER PRIMARY KEY, x INTEGER)")
	execMust(t, e, "CREATE TABLE rhs (id INTEGER PRIMARY KEY, x INTEGER)")
	execMust(t, e, "INSERT INTO lhs VALUES (1,7),(2,7)")
	execMust(t, e, "INSERT INTO rhs VALUES (1,7)")
	for _, join := range []string{"JOIN", "LEFT JOIN"} {
		q := "SELECT lhs.id,rhs.id FROM lhs " + join + " rhs ON lhs.x=rhs.x WHERE rhs.id=1 ORDER BY lhs.id"
		got := execMust(t, e, q).Rows
		want := [][]interface{}{{int64(1), int64(1)}, {int64(2), int64(1)}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v want %v", q, got, want)
		}
	}
}
