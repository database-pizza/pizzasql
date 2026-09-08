package executor

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

func newTestDB(t *testing.T) (*storage.KVPool, *storage.SchemaManager, *storage.TableManager) {
	t.Helper()
	kv := testkv.New(t)
	pool := kv.Pool(8)
	t.Cleanup(func() { pool.Close() })
	schema := storage.NewSchemaManager(pool, "testdb")
	table := storage.NewTableManager(pool, schema, "testdb")
	return pool, schema, table
}

func newExec(schema *storage.SchemaManager, table *storage.TableManager) *Executor {
	e := New(schema, table)
	e.SyncCatalog()
	return e
}

func execMust(t *testing.T, e *Executor, sql string) *Result {
	t.Helper()
	res, err := execSQL(e, sql)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return res
}

func TestSQLTransactionReadYourWrites(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO t VALUES (1, 'one')")
	if res := execMust(t, e, "SELECT * FROM t WHERE id = 1"); res.RowCount != 1 {
		t.Fatalf("expected 1 row before commit, got %d", res.RowCount)
	}
	execMust(t, e, "COMMIT")
	if res := execMust(t, e, "SELECT * FROM t WHERE id = 1"); res.RowCount != 1 {
		t.Fatalf("expected 1 row after commit, got %d", res.RowCount)
	}
}

func TestSQLAutocommitDeleteWithSubquery(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE organizations (id INTEGER PRIMARY KEY, active INTEGER)")
	execMust(t, e, "CREATE TABLE metrics (id INTEGER PRIMARY KEY, organization_id INTEGER)")
	execMust(t, e, "INSERT INTO organizations VALUES (1, 0)")
	execMust(t, e, "INSERT INTO organizations VALUES (2, 1)")
	execMust(t, e, "INSERT INTO metrics VALUES (10, 1)")
	execMust(t, e, "INSERT INTO metrics VALUES (20, 2)")

	type outcome struct {
		result *Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := execSQL(e, "DELETE FROM metrics WHERE organization_id IN (SELECT id FROM organizations WHERE active = 0)")
		done <- outcome{result: result, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.result.RowsAffected != 1 {
			t.Fatalf("deleted %d rows, want 1", got.result.RowsAffected)
		}
	case <-time.After(time.Second):
		t.Fatal("DELETE with subquery deadlocked")
	}

	if result := execMust(t, e, "SELECT id FROM metrics"); result.RowCount != 1 || result.Rows[0][0] != int64(20) {
		t.Fatalf("remaining rows = %v", result.Rows)
	}
}

func TestSQLTransactionRollbackZeroDurableWrites(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO t VALUES (1)")
	execMust(t, e, "INSERT INTO t VALUES (2)")
	execMust(t, e, "ROLLBACK")
	res := execMust(t, e, "SELECT COUNT(*) FROM t")
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(0) {
		t.Fatalf("expected 0 rows after rollback, got %v", res.Rows)
	}
}

func TestSQLTransactionSavepoints(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO t VALUES (1)")
	execMust(t, e, "SAVEPOINT sp1")
	execMust(t, e, "INSERT INTO t VALUES (2)")
	execMust(t, e, "ROLLBACK TO sp1")
	execMust(t, e, "COMMIT")
	if res := execMust(t, e, "SELECT COUNT(*) FROM t"); res.Rows[0][0] != int64(1) {
		t.Fatalf("expected 1 row after savepoint rollback, got %v", res.Rows[0][0])
	}
	if res := execMust(t, e, "SELECT * FROM t WHERE id = 2"); res.RowCount != 0 {
		t.Fatalf("row 2 should be discarded")
	}
}

func TestSQLTransactionConcurrentUpdate(t *testing.T) {
	_, schema, table := newTestDB(t)
	e1 := newExec(schema, table)
	e2 := newExec(schema, table)

	execMust(t, e1, "CREATE TABLE acct (id INTEGER PRIMARY KEY, bal INTEGER)")
	execMust(t, e1, "INSERT INTO acct VALUES (1, 100)")

	execMust(t, e1, "BEGIN")
	execMust(t, e2, "BEGIN")
	execMust(t, e1, "UPDATE acct SET bal = bal + 10 WHERE id = 1")
	execMust(t, e2, "UPDATE acct SET bal = bal + 20 WHERE id = 1")

	results := make(chan error, 2)
	go func() { _, err := execSQL(e1, "COMMIT"); results <- err }()
	go func() { _, err := execSQL(e2, "COMMIT"); results <- err }()

	errs := [2]error{<-results, <-results}
	ok, conflict := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case err == storage.ErrSerialization:
			conflict++
		default:
			t.Fatalf("unexpected commit error: %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("expected exactly one commit and one conflict, got ok=%d conflict=%d", ok, conflict)
	}
	if res := execMust(t, e1, "SELECT bal FROM acct WHERE id = 1"); res.Rows[0][0] != int64(110) && res.Rows[0][0] != int64(120) {
		t.Fatalf("balance should be 110 or 120 (the winning update), got %v", res.Rows[0][0])
	}
}

func TestSQLAtomicMultiTableCommit(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE a (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE b (id INTEGER PRIMARY KEY)")
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO a VALUES (1)")
	execMust(t, e, "INSERT INTO b VALUES (1)")
	execMust(t, e, "COMMIT")
	if res := execMust(t, e, "SELECT COUNT(*) FROM a"); res.Rows[0][0] != int64(1) {
		t.Fatalf("a count = %v", res.Rows[0][0])
	}
	if res := execMust(t, e, "SELECT COUNT(*) FROM b"); res.Rows[0][0] != int64(1) {
		t.Fatalf("b count = %v", res.Rows[0][0])
	}
}

func TestSQLTransactionJoinIgnoresUnrelatedRows(t *testing.T) {
	_, schema, table := newTestDB(t)
	e1 := newExec(schema, table)
	e2 := newExec(schema, table)

	execMust(t, e1, "CREATE TABLE products (id TEXT PRIMARY KEY, inventory INTEGER)")
	execMust(t, e1, "CREATE TABLE cart_items (id TEXT PRIMARY KEY, cart_id TEXT, product_id TEXT)")
	execMust(t, e1, "CREATE INDEX idx_cart_items_cart ON cart_items (cart_id)")
	execMust(t, e1, "CREATE TABLE orders (id TEXT PRIMARY KEY)")
	execMust(t, e1, "INSERT INTO products VALUES ('p1', 10)")
	execMust(t, e1, "INSERT INTO products VALUES ('p2', 20)")
	execMust(t, e1, "INSERT INTO cart_items VALUES ('i1', 'cart-a', 'p1')")

	execMust(t, e1, "BEGIN")
	if result := execMust(t, e1, "SELECT ci.id, p.inventory FROM cart_items ci JOIN products p ON ci.product_id = p.id WHERE ci.cart_id = 'cart-a'"); result.RowCount != 1 {
		t.Fatalf("join returned %d rows", result.RowCount)
	}
	execMust(t, e2, "UPDATE products SET inventory = 21 WHERE id = 'p2'")
	execMust(t, e2, "INSERT INTO cart_items VALUES ('i2', 'cart-b', 'p2')")
	execMust(t, e1, "INSERT INTO orders VALUES ('o1')")
	if _, err := execSQL(e1, "COMMIT"); err != nil {
		t.Fatalf("unrelated product/cart item caused conflict: %v", err)
	}
}

func TestSQLDuplicateInsert(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t VALUES (1)")
	if _, err := execSQL(e, "INSERT INTO t VALUES (1)"); err == nil {
		t.Fatal("expected duplicate insert to fail")
	}
}

func TestSQLPerTableConcurrentRowIDs(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (name TEXT)")

	const n = 50
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			exec := newExec(schema, table)
			if _, err := execSQL(exec, fmt.Sprintf("INSERT INTO t VALUES ('n%d')", i)); err != nil {
				errCh <- fmt.Errorf("insert %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	check := newExec(schema, table)
	res, err := execSQL(check, "SELECT COUNT(*) FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0] != int64(n) {
		t.Fatalf("expected %d rows, got %v", n, res.Rows[0][0])
	}
}
