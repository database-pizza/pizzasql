package executor

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResultLastInsertIDActualRowID(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)")

	res := execMust(t, e, "INSERT INTO t (name) VALUES ('a')")
	if res.LastInsertID != 1 {
		t.Fatalf("first LastInsertID = %d, want 1", res.LastInsertID)
	}
	res = execMust(t, e, "INSERT INTO t (name) VALUES ('b')")
	if res.LastInsertID != 2 {
		t.Fatalf("second LastInsertID = %d, want 2", res.LastInsertID)
	}

	// Never MAX: an explicit high id advances the counter, not a table scan MAX.
	execMust(t, e, "INSERT INTO t (id, name) VALUES (100, 'high')")
	res = execMust(t, e, "INSERT INTO t (name) VALUES ('c')")
	if res.LastInsertID != 101 {
		t.Fatalf("LastInsertID after explicit 100 = %d, want 101", res.LastInsertID)
	}
}

func TestLastInsertRowIDFunction(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)")

	if res := execMust(t, e, "SELECT last_insert_rowid()"); res.Rows[0][0] != int64(0) {
		t.Fatalf("initial last_insert_rowid = %v, want 0", res.Rows[0][0])
	}
	execMust(t, e, "INSERT INTO t (name) VALUES ('a')")
	if res := execMust(t, e, "SELECT last_insert_rowid()"); res.Rows[0][0] != int64(1) {
		t.Fatalf("last_insert_rowid after insert = %v, want 1", res.Rows[0][0])
	}
}

func TestChangesAndTotalChangesFunctions(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	execMust(t, e, "INSERT INTO t VALUES (1)")
	execMust(t, e, "INSERT INTO t VALUES (2)")
	if res := execMust(t, e, "SELECT changes()"); res.Rows[0][0] != int64(1) {
		t.Fatalf("changes() after single insert = %v, want 1", res.Rows[0][0])
	}
	if res := execMust(t, e, "SELECT total_changes()"); res.Rows[0][0] != int64(2) {
		t.Fatalf("total_changes() = %v, want 2", res.Rows[0][0])
	}

	execMust(t, e, "UPDATE t SET id = id WHERE id = 1")
	if res := execMust(t, e, "SELECT changes()"); res.Rows[0][0] != int64(1) {
		t.Fatalf("changes() after update = %v, want 1", res.Rows[0][0])
	}
	execMust(t, e, "DELETE FROM t")
	if res := execMust(t, e, "SELECT changes()"); res.Rows[0][0] != int64(2) {
		t.Fatalf("changes() after delete = %v, want 2", res.Rows[0][0])
	}
	if res := execMust(t, e, "SELECT total_changes()"); res.Rows[0][0] != int64(5) {
		t.Fatalf("total_changes() = %v, want 5", res.Rows[0][0])
	}
}

func TestLastInsertRowIDHoldsAcrossRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO t (name) VALUES ('x')")
	execMust(t, e, "ROLLBACK")

	if res := execMust(t, e, "SELECT last_insert_rowid()"); res.Rows[0][0] != int64(1) {
		t.Fatalf("last_insert_rowid() after rollback = %v, want 1 (SQLite holds it)", res.Rows[0][0])
	}
	// total_changes() is monotonic: the rolled-back insert still counts.
	if res := execMust(t, e, "SELECT total_changes()"); res.Rows[0][0] != int64(1) {
		t.Fatalf("total_changes() after rollback = %v, want 1 (not decremented)", res.Rows[0][0])
	}
	if res := execMust(t, e, "SELECT changes()"); res.Rows[0][0] != int64(1) {
		t.Fatalf("changes() after rollback = %v, want 1 (reflects last DML)", res.Rows[0][0])
	}
}

func TestTotalChangesMonotonicAcrossSavepointRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO t (name) VALUES ('a')") // total=1
	execMust(t, e, "SAVEPOINT sp1")
	execMust(t, e, "INSERT INTO t (name) VALUES ('b')") // total=2
	execMust(t, e, "INSERT INTO t (name) VALUES ('c')") // total=3
	execMust(t, e, "ROLLBACK TO sp1")
	if res := execMust(t, e, "SELECT total_changes()"); res.Rows[0][0] != int64(3) {
		t.Fatalf("total_changes() after ROLLBACK TO = %v, want 3 (monotonic)", res.Rows[0][0])
	}
	execMust(t, e, "INSERT INTO t (name) VALUES ('d')") // total=4
	execMust(t, e, "ROLLBACK")
	if res := execMust(t, e, "SELECT total_changes()"); res.Rows[0][0] != int64(4) {
		t.Fatalf("total_changes() after full rollback = %v, want 4 (monotonic)", res.Rows[0][0])
	}
	// changes() reflects the most recent DML statement (INSERT 'd' = 1 row).
	if res := execMust(t, e, "SELECT changes()"); res.Rows[0][0] != int64(1) {
		t.Fatalf("changes() after rollback = %v, want 1", res.Rows[0][0])
	}
	// The entire transaction was rolled back, so no rows survive; total_changes
	// nevertheless still counts every completed DML statement.
	if res := execMust(t, e, "SELECT count(*) FROM t"); res.Rows[0][0] != int64(0) {
		t.Fatalf("surviving rows = %v, want 0 (whole transaction rolled back)", res.Rows[0][0])
	}
}

func TestLastInsertRowIDMultiRowAndInsertSelect(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)")
	execMust(t, e, "CREATE TABLE src (name TEXT)")

	res := execMust(t, e, "INSERT INTO t (name) VALUES ('a'), ('b')")
	if res.LastInsertID != 2 {
		t.Fatalf("multi-row LastInsertID = %d, want 2 (last row)", res.LastInsertID)
	}

	execMust(t, e, "INSERT INTO src VALUES ('c'), ('d')")
	res = execMust(t, e, "INSERT INTO t (name) SELECT name FROM src")
	if res.LastInsertID != 4 {
		t.Fatalf("insert-select LastInsertID = %d, want 4 (last row)", res.LastInsertID)
	}
}

func TestSessionLocalStateIsolation(t *testing.T) {
	_, schema, table := newTestDB(t)
	e1 := newExec(schema, table)
	e2 := newExec(schema, table)

	execMust(t, e1, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)")

	execMust(t, e1, "INSERT INTO t (name) VALUES ('a')")
	execMust(t, e2, "INSERT INTO t (name) VALUES ('b')")

	if res := execMust(t, e1, "SELECT last_insert_rowid()"); res.Rows[0][0] != int64(1) {
		t.Fatalf("e1 last_insert_rowid = %v, want 1", res.Rows[0][0])
	}
	if res := execMust(t, e2, "SELECT last_insert_rowid()"); res.Rows[0][0] != int64(2) {
		t.Fatalf("e2 last_insert_rowid = %v, want 2", res.Rows[0][0])
	}
	if res := execMust(t, e2, "SELECT total_changes()"); res.Rows[0][0] != int64(1) {
		t.Fatalf("e2 total_changes = %v, want 1 (session-local)", res.Rows[0][0])
	}
	if res := execMust(t, e1, "SELECT total_changes()"); res.Rows[0][0] != int64(1) {
		t.Fatalf("e1 total_changes = %v, want 1 (INSERT only; DDL does not count)", res.Rows[0][0])
	}
}

func TestConcurrentSessionRowIDs(t *testing.T) {
	_, schema, table := newTestDB(t)
	execMust(t, newExec(schema, table), "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)")

	const n = 32
	rowIDs := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := newExec(schema, table)
			res := execMust(t, e, "INSERT INTO t (name) VALUES ('x')")
			rowIDs[i] = res.LastInsertID
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]bool, n)
	for _, id := range rowIDs {
		if id == 0 {
			t.Fatal("an insert returned LastInsertID 0")
		}
		if seen[id] {
			t.Fatalf("duplicate generated rowid %d across concurrent sessions", id)
		}
		seen[id] = true
	}
}

func TestUniqueIndexMultiRowUpdateAtomic(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX uq_v ON t (v)")
	execMust(t, e, "INSERT INTO t VALUES (1, 'a'), (2, 'b'), (3, 'c')")

	_, err := execSQL(e, "UPDATE t SET v = 'x'")
	if err == nil {
		t.Fatal("expected unique violation")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	// No partial writes: the first row must not have been changed to 'x'.
	if res := execMust(t, e, "SELECT v FROM t WHERE id = 1"); res.Rows[0][0] != "a" {
		t.Fatalf("partial update applied: row 1 v = %v, want 'a'", res.Rows[0][0])
	}
}

func TestUniqueIndexMultiRowInsertAtomic(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX uq_v ON t (v)")

	_, err := execSQL(e, "INSERT INTO t VALUES (1, 'a'), (2, 'b'), (3, 'a')")
	if err == nil {
		t.Fatal("expected unique violation on the third row")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if res := execMust(t, e, "SELECT count(*) FROM t"); res.Rows[0][0] != int64(0) {
		t.Fatalf("partial insert applied: %v rows present, want 0", res.Rows[0][0])
	}
}

func TestPrimaryKeyMultiRowInsertAtomic(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")

	_, err := execSQL(e, "INSERT INTO t VALUES (1, 'a'), (1, 'b')")
	if err == nil {
		t.Fatal("expected primary-key violation on the second row")
	}
	if res := execMust(t, e, "SELECT count(*) FROM t"); res.Rows[0][0] != int64(0) {
		t.Fatalf("partial insert applied: %v rows present, want 0", res.Rows[0][0])
	}
}

func TestUpdateWithScalarSubqueryInTransaction(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE watch (user_id INTEGER, repo_id INTEGER, PRIMARY KEY(user_id, repo_id))")
	execMust(t, e, "CREATE TABLE repository (id INTEGER PRIMARY KEY, num_watches INTEGER)")
	execMust(t, e, "INSERT INTO repository VALUES (1, 0)")
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO watch VALUES (1, 1)")

	// A scalar subquery in SET must not deadlock on the session lock while the
	// update callback runs inside an explicit transaction.
	done := make(chan error, 1)
	go func() {
		_, err := execSQL(e, "UPDATE repository SET num_watches = (SELECT COUNT(*) FROM watch WHERE repo_id = 1) WHERE id = 1")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("update with scalar subquery: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("UPDATE with scalar subquery deadlocked inside a transaction")
	}

	execMust(t, e, "COMMIT")
	if res := execMust(t, e, "SELECT num_watches FROM repository WHERE id = 1"); res.Rows[0][0] != int64(1) {
		t.Fatalf("num_watches = %v, want 1", res.Rows[0][0])
	}
}
