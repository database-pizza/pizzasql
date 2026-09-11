package executor

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// runWithin fails the test if fn does not return before the deadline. It is used
// to turn a would-be deadlock into a test failure instead of a hung suite.
func runWithin(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("operation did not complete within %s (deadlock?)", d)
	}
}

func TestFinishDMLPreservesLastInsertRowID(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)")
	execMust(t, e, "INSERT INTO t (v) VALUES ('a')")
	if got := execMust(t, e, "SELECT last_insert_rowid()").Rows[0][0]; got != int64(1) {
		t.Fatalf("last_insert_rowid after insert = %v, want 1", got)
	}

	execMust(t, e, "UPDATE t SET v = 'b'")
	if got := execMust(t, e, "SELECT last_insert_rowid()").Rows[0][0]; got != int64(1) {
		t.Fatalf("last_insert_rowid after UPDATE = %v, want 1", got)
	}
	if res := execMust(t, e, "UPDATE t SET v = 'c' RETURNING id"); res.Rows[0][0] != int64(1) {
		t.Fatalf("UPDATE RETURNING id = %v", res.Rows[0][0])
	}
	if got := execMust(t, e, "SELECT last_insert_rowid()").Rows[0][0]; got != int64(1) {
		t.Fatalf("last_insert_rowid after UPDATE RETURNING = %v, want 1", got)
	}

	execMust(t, e, "DELETE FROM t")
	if got := execMust(t, e, "SELECT last_insert_rowid()").Rows[0][0]; got != int64(1) {
		t.Fatalf("last_insert_rowid after DELETE = %v, want 1", got)
	}

	execMust(t, e, "INSERT INTO t (v) VALUES ('d')")
	if got := execMust(t, e, "SELECT last_insert_rowid()").Rows[0][0]; got != int64(2) {
		t.Fatalf("last_insert_rowid after second insert = %v, want 2", got)
	}
}

func TestUpsertPreservesLastInsertRowID(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT UNIQUE, tag TEXT)")
	execMust(t, e, "INSERT INTO t (email, tag) VALUES ('a', 'old')")
	if got := execMust(t, e, "SELECT last_insert_rowid()").Rows[0][0]; got != int64(1) {
		t.Fatalf("insert last_insert_rowid = %v, want 1", got)
	}
	execMust(t, e, "INSERT INTO t (email, tag) VALUES ('a', 'new') ON CONFLICT (email) DO UPDATE SET tag = excluded.tag")
	if got := execMust(t, e, "SELECT last_insert_rowid()").Rows[0][0]; got != int64(1) {
		t.Fatalf("upsert-update last_insert_rowid = %v, want 1", got)
	}
}

func TestUpsertNonPKUniqueReturningReturnsStoredRow(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT UNIQUE, tag TEXT)")

	res := execMust(t, e, "INSERT INTO t (email, tag) VALUES ('a', 'old') RETURNING id, tag")
	id := res.Rows[0][0]

	res = execMust(t, e, "INSERT INTO t (email, tag) VALUES ('a', 'new') ON CONFLICT (email) DO UPDATE SET tag = excluded.tag RETURNING id, tag")
	if res.RowCount != 1 {
		t.Fatalf("upsert RETURNING rows = %d, want 1", res.RowCount)
	}
	if res.Rows[0][0] != id || res.Rows[0][1] != "new" {
		t.Fatalf("upsert RETURNING = %v, want [%v new]", res.Rows[0], id)
	}
	// The candidate's auto-increment id must not have been consumed/returned.
	if res.LastInsertID != id {
		t.Fatalf("upsert LastInsertID = %v, want preserved %v", res.LastInsertID, id)
	}
}

func TestUpdatePrimaryKeyReturningReturnsChangedRow(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (1, 'a')")

	res := execMust(t, e, "UPDATE t SET id = 2 WHERE id = 1 RETURNING id, v")
	if res.RowCount != 1 || res.Rows[0][0] != int64(2) || res.Rows[0][1] != "a" {
		t.Fatalf("UPDATE pk RETURNING = %v", res.Rows)
	}
	if rows := execMust(t, e, "SELECT id, v FROM t"); rows.RowCount != 1 || rows.Rows[0][0] != int64(2) {
		t.Fatalf("after pk update table = %v (orphan old key?)", rows.Rows)
	}
}

func TestUpdatePrimaryKeyInTransaction(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (1, 'a')")

	execMust(t, e, "BEGIN")
	execMust(t, e, "UPDATE t SET id = 2 WHERE id = 1")
	// The old key must be gone within the transaction overlay too.
	if res := execMust(t, e, "SELECT id FROM t"); res.RowCount != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("in-tx pk update = %v", res.Rows)
	}
	execMust(t, e, "COMMIT")

	res := execMust(t, e, "SELECT id, v FROM t")
	if res.RowCount != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("after commit = %v", res.Rows)
	}
}

func TestUpdateDeleteReturningWithSubqueryNoDeadlock(t *testing.T) {
	runWithin(t, 10*time.Second, func() {
		_, schema, table := newTestDB(t)
		e := newExec(schema, table)
		execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
		execMust(t, e, "INSERT INTO t VALUES (1, 'a'), (2, 'b')")

		res := execMust(t, e, "UPDATE t SET v = 'x' WHERE id IN (SELECT id FROM t WHERE id = 1) RETURNING id, v")
		if res.RowCount != 1 || res.Rows[0][0] != int64(1) || res.Rows[0][1] != "x" {
			t.Fatalf("UPDATE ... IN (subquery) RETURNING = %v", res.Rows)
		}
		res = execMust(t, e, "DELETE FROM t WHERE id IN (SELECT id FROM t WHERE id = 1) RETURNING id")
		if res.RowCount != 1 || res.Rows[0][0] != int64(1) {
			t.Fatalf("DELETE ... IN (subquery) RETURNING = %v", res.Rows)
		}
	})
}

func TestUpdateCorrelatedSubqueryInSet(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE sizes (size_id INTEGER PRIMARY KEY, width INTEGER)")
	execMust(t, e, "CREATE TABLE hits (id INTEGER PRIMARY KEY, size_id INTEGER, width INTEGER)")
	execMust(t, e, "INSERT INTO sizes VALUES (1, 480), (2, 720)")
	execMust(t, e, "INSERT INTO hits (id, size_id) VALUES (1, 1), (2, 2)")

	execMust(t, e, "UPDATE hits SET width = (SELECT width FROM sizes WHERE size_id = hits.size_id)")
	res := execMust(t, e, "SELECT id, width FROM hits ORDER BY id")
	if res.Rows[0][1] != int64(480) || res.Rows[1][1] != int64(720) {
		t.Fatalf("correlated SET update = %v", res.Rows)
	}
}

func TestUpdateFromWithCTE(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE users (user_id INTEGER PRIMARY KEY AUTOINCREMENT, site_id INTEGER, access TEXT DEFAULT 'x')")
	execMust(t, e, "INSERT INTO users (site_id) VALUES (1), (1), (2)")

	// The exact GoatCounter 2021-12-13-2-superuser.sql shape.
	execMust(t, e, `WITH x AS (
		SELECT count(*) AS count, site_id FROM users GROUP BY site_id
	)
	UPDATE users SET access = '{"all": "*"}' FROM x
	WHERE x.count = 1 AND users.site_id = x.site_id`)

	res := execMust(t, e, "SELECT site_id, access FROM users ORDER BY user_id")
	if res.Rows[0][1] != "x" || res.Rows[1][1] != "x" {
		t.Fatalf("site 1 users should be unchanged, got %v", res.Rows)
	}
	if res.Rows[2][1] != `{"all": "*"}` {
		t.Fatalf("site 2 user should be updated, got %v", res.Rows[2])
	}
}

func TestAnalyzeIsSafeNoOp(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	if res := execMust(t, e, "ANALYZE"); res.CommandTag != "ANALYZE" {
		t.Fatalf("ANALYZE tag = %q", res.CommandTag)
	}
	if res := execMust(t, e, "ANALYZE t"); res.CommandTag != "ANALYZE" {
		t.Fatalf("ANALYZE t tag = %q", res.CommandTag)
	}
}

func TestForeignKeysPragmaNoOp(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	if res := execMust(t, e, "PRAGMA foreign_keys = OFF"); res.CommandTag != "PRAGMA" {
		t.Fatalf("PRAGMA tag = %q", res.CommandTag)
	}
	res := execMust(t, e, "PRAGMA foreign_keys")
	if res.RowCount != 1 || res.Rows[0][0] != int64(0) {
		t.Fatalf("foreign_keys = %v, want 0", res.Rows)
	}
	if _, err := execSQL(e, "PRAGMA foreign_keys = ON"); err == nil {
		t.Fatal("expected enabling unsupported foreign keys to fail")
	}
}

func TestIsDistinctFrom(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	cases := []struct {
		sql  string
		want int64
	}{
		{"SELECT 1 IS DISTINCT FROM 2", 1},
		{"SELECT 1 IS DISTINCT FROM 1", 0},
		{"SELECT NULL IS DISTINCT FROM NULL", 0},
		{"SELECT NULL IS DISTINCT FROM 1", 1},
		{"SELECT 1 IS NOT DISTINCT FROM 1", 1},
		{"SELECT NULL IS NOT DISTINCT FROM NULL", 1},
		{"SELECT NULL IS NOT DISTINCT FROM 1", 0},
	}
	for _, tc := range cases {
		got := execMust(t, e, tc.sql).Rows[0][0]
		var b int64
		if v, ok := got.(bool); ok {
			if v {
				b = 1
			}
		} else {
			b = got.(int64)
		}
		if b != tc.want {
			t.Errorf("%s = %v, want %d", tc.sql, got, tc.want)
		}
	}
}

func TestRejectVirtualGeneratedColumn(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	if _, err := execSQL(e, "CREATE TABLE a (x INTEGER, y INTEGER GENERATED ALWAYS AS (x + 1))"); err == nil {
		t.Fatal("expected implicit VIRTUAL generated column to be rejected")
	}
	if _, err := execSQL(e, "CREATE TABLE b (x INTEGER, y INTEGER AS (x + 1) VIRTUAL)"); err == nil {
		t.Fatal("expected VIRTUAL generated column to be rejected")
	}
	if _, err := execSQL(e, "CREATE TABLE c (x INTEGER, y INTEGER GENERATED ALWAYS AS (x + 1) STORED)"); err != nil {
		t.Fatalf("STORED generated column should be accepted: %v", err)
	}
}

func TestRejectUnsupportedIndexExpressions(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (email TEXT)")
	for _, sql := range []string{
		"CREATE INDEX i1 ON t (random())",
		"CREATE INDEX i2 ON t (randomblob(4))",
		"CREATE INDEX i3 ON t ((SELECT 1))",
		"CREATE INDEX i4 ON t (email || (SELECT 1))",
		"CREATE INDEX i5 ON t (no_such_function(email))",
		"CREATE INDEX i6 ON t (datetime('now'))",
		"CREATE INDEX i7 ON t (lower(missing))",
	} {
		if _, err := execSQL(e, sql); err == nil {
			t.Errorf("%s: expected rejection", sql)
		}
	}
	// Deterministic expressions remain accepted.
	if _, err := execSQL(e, "CREATE INDEX iok ON t (lower(email))"); err != nil {
		t.Fatalf("lower(email) index should be accepted: %v", err)
	}
}

func TestRejectGeneratedColumnWithNonDeterministicExpr(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	if _, err := execSQL(e, "CREATE TABLE t (a INTEGER, b INTEGER GENERATED ALWAYS AS (random()) STORED)"); err == nil {
		t.Fatal("expected non-deterministic generated column to be rejected")
	}
	if _, err := execSQL(e, "CREATE TABLE t2 (a INTEGER, b INTEGER GENERATED ALWAYS AS ((SELECT 1)) STORED)"); err == nil {
		t.Fatal("expected subquery generated column to be rejected")
	}
}

func TestNumericConcatFormatting(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	cases := map[string]string{
		"SELECT 5 || 'px'":                 "5px",
		"SELECT 1.5 - 0.5 || 'px'":         "1.0px",
		"SELECT 2.5 || ''":                 "2.5",
		"SELECT '↔ ' || 480 || 'px'":       "↔ 480px",
		"SELECT (SELECT 3.5 - 0.5) || 'x'": "3.0x",
		"SELECT CAST(7 AS REAL) || 'x'":    "7.0x",
	}
	for sql, want := range cases {
		got := execMust(t, e, sql).Rows[0][0]
		if got != want {
			t.Errorf("%s = %v, want %q", sql, got, want)
		}
	}
}

func TestInsertOrReplaceWithExpressionUniqueIndexIsIndexed(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, tag TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX users_email_lower ON users (lower(email))")

	for i := 1; i <= 50; i++ {
		execMust(t, e, fmt.Sprintf("INSERT INTO users (id, email, tag) VALUES (%d, 'User%d@example.com', 'seed')", i, i))
	}
	execMust(t, e, "INSERT OR REPLACE INTO users (id, email, tag) VALUES (999, 'user7@example.com', 'replaced')")

	res := execMust(t, e, "SELECT id, tag FROM users WHERE lower(email) = 'user7@example.com'")
	if res.RowCount != 1 || res.Rows[0][0] != int64(999) || res.Rows[0][1] != "replaced" {
		t.Fatalf("replace result = %v", res.Rows)
	}
}

func TestCompositeExpressionUniqueReplace(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE users (
		user_id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL,
		email TEXT NOT NULL
	)`)
	execMust(t, e, "CREATE UNIQUE INDEX users_site_email ON users(site_id, lower(email))")
	execMust(t, e, "INSERT INTO users (site_id, email) VALUES (1, 'A@x.com')")
	execMust(t, e, "INSERT OR REPLACE INTO users (site_id, email) VALUES (1, 'a@x.com')")

	res := execMust(t, e, "SELECT count(*) FROM users WHERE site_id = 1")
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("composite expression replace left %v rows", res.Rows[0][0])
	}
}

// TestConcurrentExpressionIndex exercises the stateless evaluator under -race.
func TestConcurrentExpressionIndex(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE)")
	execMust(t, e, "CREATE UNIQUE INDEX users_email_lower ON users (lower(email))")

	const workers = 8
	const perWorker = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			exec := New(schema, table)
			exec.SyncCatalog()
			for i := 0; i < perWorker; i++ {
				email := fmt.Sprintf("user-%d-%d@example.com", w, i)
				sql := fmt.Sprintf("INSERT INTO users (id, email) VALUES (%d, '%s')", w*1000+i+1, email)
				stmt, err := parser.New(lexer.New(sql)).Parse()
				if err != nil {
					errs <- err
					return
				}
				if _, err := exec.Execute(stmt); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent insert failed: %v", err)
	}

	res := execMust(t, e, "SELECT count(*) FROM users")
	if res.Rows[0][0] != int64(workers*perWorker) {
		t.Fatalf("row count = %v, want %d", res.Rows[0][0], workers*perWorker)
	}
}

func TestExpressionIndexEvaluatorErrorIsNotSwallowed(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (email TEXT)")
	execMust(t, e, "CREATE INDEX i ON t (lower(email))")

	// Break the evaluator after the index cache is built; a subsequent write
	// must surface the evaluator error instead of silently skipping index
	// maintenance.
	table.SetExpressionEvaluator(func(expression string, row storage.Row) (interface{}, error) {
		return nil, fmt.Errorf("boom: %s", expression)
	})
	if _, err := execSQL(e, "INSERT INTO t (email) VALUES ('a')"); err == nil {
		t.Fatal("expected evaluator error to surface on insert")
	}
}
