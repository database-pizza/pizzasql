package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

func TestWarmUniqueValidationDoesNotScan(t *testing.T) {
	kv := testkv.New(t)
	pool := kv.Pool(8)
	defer pool.Close()
	schemas := storage.NewSchemaManager(pool, "unique_cache")
	tables := storage.NewTableManager(pool, schemas, "unique_cache")
	e := newExec(schemas, tables)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER UNIQUE)")
	var values []string
	for i := 1; i <= 100; i++ {
		values = append(values, fmt.Sprintf("(%d,%d)", i, i))
	}
	execMust(t, e, "INSERT INTO t VALUES "+strings.Join(values, ","))
	for _, sql := range []string{
		"INSERT INTO t VALUES (101,101)",
		"UPDATE t SET v=102 WHERE id=101",
		"DELETE FROM t WHERE id=101",
		"INSERT INTO t VALUES (102,102)",
		"BEGIN", "INSERT INTO t VALUES (103,103)", "ROLLBACK",
		"INSERT INTO t VALUES (103,103)",
		"BEGIN", "UPDATE t SET v=501 WHERE id=1", "UPDATE t SET v=1 WHERE id=2", "UPDATE t SET v=2 WHERE id=1", "COMMIT",
		"INSERT INTO t VALUES (104,NULL),(105,NULL)",
	} {
		before := kv.RequestCount(10)
		execMust(t, e, sql)
		if n := kv.RequestCount(10) - before; n != 0 {
			t.Fatalf("%s: unique validation scanned %d pages", sql, n)
		}
	}
	for _, sql := range []string{"INSERT INTO t VALUES (200,2)", "UPDATE t SET v=2 WHERE id=103"} {
		if _, err := execSQL(e, sql); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	execMust(t, e, "UPDATE t SET id=1000 WHERE id=1")
	if _, err := execSQL(e, "INSERT INTO t VALUES (200,2)"); err == nil {
		t.Fatal("PK move released an owned unique value")
	}
	execMust(t, e, "DELETE FROM t WHERE id=1000")
	execMust(t, e, "INSERT INTO t VALUES (200,2)")

	// A new manager models restart: durable rows rebuild the cache once, not on
	// every mutation. Managers are shared per database in normal operation.
	schemas = storage.NewSchemaManager(pool, "unique_cache")
	tables = storage.NewTableManager(pool, schemas, "unique_cache")
	e = newExec(schemas, tables)
	before := kv.RequestCount(10)
	execMust(t, e, "INSERT INTO t VALUES (201,201)")
	if kv.RequestCount(10) == before {
		t.Fatal("cold UNIQUE cache did not read durable state")
	}
	before = kv.RequestCount(10)
	execMust(t, e, "INSERT INTO t VALUES (202,202)")
	if kv.RequestCount(10) != before {
		t.Fatal("warm UNIQUE cache rebuilt after restart")
	}
	if _, err := execSQL(e, "INSERT INTO t VALUES (203,201)"); err == nil {
		t.Fatal("restarted cache lost an existing value")
	}
}

func TestUniqueCacheDropRecreateAndFailedTransaction(t *testing.T) {
	_, schemas, tables := newTestDB(t)
	e := newExec(schemas, tables)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)")
	execMust(t, e, "CREATE UNIQUE INDEX MixedCase ON t(v)")
	execMust(t, e, "INSERT INTO t VALUES (1,1)")
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO t VALUES (2,2),(3,2)")
	if _, err := execSQL(e, "COMMIT"); err == nil {
		t.Fatal("duplicate pending values committed")
	}
	execMust(t, e, "INSERT INTO t VALUES (2,2)")
	execMust(t, e, "DROP INDEX MixedCase")
	execMust(t, e, "INSERT INTO t VALUES (3,2)")
	if _, err := execSQL(e, "CREATE UNIQUE INDEX MixedCase ON t(v)"); err == nil {
		t.Fatal("recreated unique index ignored durable duplicate")
	}
	execMust(t, e, "DELETE FROM t WHERE id=3")
	execMust(t, e, "CREATE UNIQUE INDEX MixedCase ON t(v)")
	if _, err := execSQL(e, "INSERT INTO t VALUES (3,2)"); err == nil {
		t.Fatal("recreated cache lost owner")
	}
}

func TestUniqueCacheAmbiguousWriteOutcome(t *testing.T) {
	for _, operation := range []string{"transaction", "insert", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			kv := testkv.New(t)
			pool := kv.Pool(8)
			defer pool.Close()
			schemas := storage.NewSchemaManager(pool, "ambiguous")
			tables := storage.NewTableManager(pool, schemas, "ambiguous")
			e := newExec(schemas, tables)
			execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER UNIQUE)")
			execMust(t, e, "INSERT INTO t VALUES (1,1)")
			kv.DropNextCompareReply()
			var err error
			switch operation {
			case "transaction":
				_, err = execSQL(e, "INSERT INTO t VALUES (2,2)")
			case "insert":
				err = tables.Insert("t", storage.Row{"id": int64(2), "v": int64(2)})
			case "update":
				_, _, err = tables.UpdateByPK("t", "1", func(storage.Row) (storage.Row, error) { return storage.Row{"v": int64(2)}, nil })
			case "delete":
				_, _, err = tables.DeleteByPK("t", "1")
			}
			if err == nil {
				t.Fatal("dropped reply unexpectedly succeeded")
			}
			if operation == "delete" {
				execMust(t, e, "INSERT INTO t VALUES (3,1)")
			} else {
				if _, err := execSQL(e, "INSERT INTO t VALUES (3,2)"); err == nil {
					t.Fatal("unknown but durable unique owner was ignored")
				}
				if operation == "update" {
					execMust(t, e, "INSERT INTO t VALUES (4,1)")
				}
			}
		})
	}
}
