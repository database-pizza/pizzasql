package executor

import (
	"reflect"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

func TestExclusiveCommitsMaintainWarmIndexesAndCounts(t *testing.T) {
	kv := testkv.New(t)
	pool := kv.Pool(8)
	defer pool.Close()
	schema := storage.NewSchemaManager(pool, "cache_commit")
	table := storage.NewTableManager(pool, schema, "cache_commit")
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (pk TEXT PRIMARY KEY, v INTEGER, u INTEGER UNIQUE)")
	execMust(t, e, "CREATE INDEX idx_v ON t(v)")
	execMust(t, e, "INSERT INTO t VALUES ('a',1,10),('b',2,20)")
	execMust(t, e, "SELECT pk FROM t WHERE v=1")
	execMust(t, e, "SELECT pk FROM t WHERE u=10")
	execMust(t, e, "SELECT COUNT(*) FROM t")
	check := func(sql string, want [][]interface{}) {
		t.Helper()
		before := kv.RequestCount(10)
		got := execMust(t, e, sql).Rows
		if len(got) != len(want) || (len(got) > 0 && !reflect.DeepEqual(got, want)) {
			t.Fatalf("%s: got %v want %v", sql, got, want)
		}
		if pages := kv.RequestCount(10) - before; pages != 0 {
			t.Fatalf("%s rebuilt a warm cache (%d pages)", sql, pages)
		}
	}
	execMust(t, e, "INSERT INTO t VALUES ('c',3,30)")
	check("SELECT pk FROM t WHERE v=3", [][]interface{}{{"c"}})
	check("SELECT COUNT(*) FROM t", [][]interface{}{{int64(3)}})
	execMust(t, e, "UPDATE t SET v=4,u=40 WHERE pk='c'")
	check("SELECT pk FROM t WHERE v=3", nil)
	check("SELECT pk FROM t WHERE v=4", [][]interface{}{{"c"}})
	check("SELECT pk FROM t WHERE u=30", nil)
	check("SELECT pk FROM t WHERE u=40", [][]interface{}{{"c"}})
	execMust(t, e, "UPDATE t SET pk='z' WHERE pk='c'")
	check("SELECT pk FROM t WHERE v=4", [][]interface{}{{"z"}})
	check("SELECT pk FROM t WHERE u=40", [][]interface{}{{"z"}})
	execMust(t, e, "BEGIN")
	execMust(t, e, "DELETE FROM t WHERE pk='a'")
	execMust(t, e, "INSERT INTO t VALUES ('a',9,90)")
	execMust(t, e, "COMMIT")
	check("SELECT pk FROM t WHERE v=1", nil)
	check("SELECT pk FROM t WHERE v=9", [][]interface{}{{"a"}})
	check("SELECT COUNT(*) FROM t", [][]interface{}{{int64(3)}})
	execMust(t, e, "UPDATE t SET u=110-u WHERE pk IN ('a','b')")
	check("SELECT pk FROM t WHERE u=20", [][]interface{}{{"a"}})
	check("SELECT pk FROM t WHERE u=90", [][]interface{}{{"b"}})
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO t VALUES ('c',3,30)")
	execMust(t, e, "ROLLBACK")
	check("SELECT pk FROM t WHERE v=3", nil)
	execMust(t, e, "DELETE FROM t WHERE pk='b'")
	check("SELECT pk FROM t WHERE u=90", nil)
	check("SELECT COUNT(*) FROM t", [][]interface{}{{int64(2)}})
	execMust(t, e, "DELETE FROM t")
	check("SELECT pk FROM t WHERE v=4", nil)
	check("SELECT COUNT(*) FROM t", [][]interface{}{{int64(0)}})
}
