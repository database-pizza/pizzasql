package executor

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// testClock lets a test move the clock forward deterministically between
// statements, so per-statement default evaluation is observable without
// depending on wall-clock resolution.
type testClock struct {
	now time.Time
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func TestCreateTableWithCurrentTimestampDefaultAppliesOnInsert(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	clock := newTestClock()
	e.clock = clock.Now

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1)")

	res := execMust(t, e, "SELECT created_at FROM t WHERE id = 1")
	if res.RowCount != 1 {
		t.Fatalf("row count = %d, want 1", res.RowCount)
	}
	want := "2026-01-02 03:04:05"
	if got := res.Rows[0][0]; got != want {
		t.Fatalf("created_at = %v, want %q", got, want)
	}
}

func TestCurrentDateAndTimeDefaults(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	e.clock = newTestClock().Now

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, d DATE DEFAULT CURRENT_DATE, tm TEXT DEFAULT CURRENT_TIME)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1)")

	res := execMust(t, e, "SELECT d, tm FROM t WHERE id = 1")
	if got, want := res.Rows[0][0], "2026-01-02"; got != want {
		t.Fatalf("date = %v, want %q", got, want)
	}
	if got, want := res.Rows[0][1], "03:04:05"; got != want {
		t.Fatalf("time = %v, want %q", got, want)
	}
}

func TestWhitelistedDatetimeNowDefaults(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	e.clock = newTestClock().Now

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, ts TIMESTAMP DEFAULT datetime('now'), d DATE DEFAULT date('now'), tm TEXT DEFAULT time('now'))")
	execMust(t, e, "INSERT INTO t (id) VALUES (1)")

	res := execMust(t, e, "SELECT ts, d, tm FROM t WHERE id = 1")
	if got, want := res.Rows[0][0], "2026-01-02 03:04:05"; got != want {
		t.Fatalf("ts = %v, want %q", got, want)
	}
	if got, want := res.Rows[0][1], "2026-01-02"; got != want {
		t.Fatalf("d = %v, want %q", got, want)
	}
	if got, want := res.Rows[0][2], "03:04:05"; got != want {
		t.Fatalf("tm = %v, want %q", got, want)
	}
}

func TestExplicitNullWinsOverNonConstantDefault(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	e.clock = newTestClock().Now

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)")
	execMust(t, e, "INSERT INTO t (id, created_at) VALUES (1, NULL)")

	res := execMust(t, e, "SELECT created_at FROM t WHERE id = 1")
	if got := res.Rows[0][0]; got != nil {
		t.Fatalf("created_at = %v, want NULL (an explicit NULL wins)", got)
	}
}

func TestUpdateOmittingColumnDoesNotReevaluateDefault(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	clock := newTestClock()
	e.clock = clock.Now

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, label TEXT, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)")
	execMust(t, e, "INSERT INTO t (id, label) VALUES (1, 'first')")

	clock.Advance(time.Hour)
	execMust(t, e, "UPDATE t SET label = 'second' WHERE id = 1")

	res := execMust(t, e, "SELECT label, created_at FROM t WHERE id = 1")
	if got := res.Rows[0][0]; got != "second" {
		t.Fatalf("label = %v, want second", got)
	}
	if got, want := res.Rows[0][1], "2026-01-02 03:04:05"; got != want {
		t.Fatalf("created_at = %v, want the INSERT-time %q (UPDATE must not re-evaluate)", got, want)
	}
}

func TestInsertDefaultValuesUsesNonConstantDefault(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	e.clock = newTestClock().Now

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, label TEXT DEFAULT 'new', created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)")
	execMust(t, e, "INSERT INTO t DEFAULT VALUES")

	res := execMust(t, e, "SELECT label, created_at FROM t")
	if res.RowCount != 1 {
		t.Fatalf("row count = %d, want 1", res.RowCount)
	}
	if got := res.Rows[0][0]; got != "new" {
		t.Fatalf("label = %v, want new", got)
	}
	if got, want := res.Rows[0][1], "2026-01-02 03:04:05"; got != want {
		t.Fatalf("created_at = %v, want %q", got, want)
	}
}

func TestNonConstantDefaultEvaluatedOncePerStatement(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	clock := newTestClock()
	e.clock = clock.Now

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1)")
	clock.Advance(time.Minute)
	execMust(t, e, "INSERT INTO t (id) VALUES (2)")

	res := execMust(t, e, "SELECT id, created_at FROM t ORDER BY id")
	first := res.Rows[0][1]
	second := res.Rows[1][1]
	if first == second {
		t.Fatalf("two INSERT statements produced the same timestamp %v; each statement must evaluate its own default", first)
	}
	if compare(first, second) >= 0 {
		t.Fatalf("timestamps %v then %v are not ordered", first, second)
	}
}

func TestNonConstantDefaultAppliesInsideTransaction(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	e.clock = newTestClock().Now

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)")
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO t (id) VALUES (1)")
	execMust(t, e, "COMMIT")

	// Read through a fresh executor so the value is the committed row, not the
	// transaction's staged overlay.
	e2 := newExec(schema, table)
	res := execMust(t, e2, "SELECT created_at FROM t WHERE id = 1")
	if got, want := res.Rows[0][0], "2026-01-02 03:04:05"; got != want {
		t.Fatalf("created_at = %v, want %q", got, want)
	}
}

func TestMultiRowInsertSharesStatementDefault(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	e.clock = newTestClock().Now

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1), (2)")

	res := execMust(t, e, "SELECT created_at FROM t")
	if res.Rows[0][0] != res.Rows[1][0] {
		t.Fatalf("multi-row INSERT timestamps = %v and %v, want one evaluation per statement", res.Rows[0][0], res.Rows[1][0])
	}
}

func TestAlterTableAddColumnBackfillsNonConstantDefaultPerRow(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1), (2)")

	// The storage-level evaluator is the per-row injection point for the
	// rewrite; a counting evaluator proves each rewritten row is stamped again.
	calls := 0
	schema.SetDefaultEvaluator(func(expression string) (interface{}, error) {
		calls++
		return fmt.Sprintf("stamp-%d", calls), nil
	})

	execMust(t, e, "ALTER TABLE t ADD COLUMN created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP")

	if calls != 2 {
		t.Fatalf("evaluator calls = %d, want one per rewritten row (2)", calls)
	}
	res := execMust(t, e, "SELECT id, created_at FROM t ORDER BY id")
	if got, want := res.Rows[0][1], "stamp-1"; got != want {
		t.Fatalf("row 1 created_at = %v, want %q", got, want)
	}
	if got, want := res.Rows[1][1], "stamp-2"; got != want {
		t.Fatalf("row 2 created_at = %v, want %q", got, want)
	}

	col, ok := e.schemaColumn(t, "t", "created_at")
	if !ok {
		t.Fatal("created_at column missing from schema")
	}
	if col.DefaultExpr == "" {
		t.Fatal("ADD COLUMN did not persist the default expression")
	}
	if col.Default != nil {
		t.Fatalf("ADD COLUMN stored an evaluated default %v instead of an expression", col.Default)
	}
}

// schemaColumn reads a column from the durable schema through a fresh manager,
// which also proves the expression survives a reload.
func (e *Executor) schemaColumn(t *testing.T, table, column string) (*storage.Column, bool) {
	t.Helper()
	reloaded := storage.NewSchemaManager(e.schema.GetPool(), e.schema.GetDatabaseName())
	schema, err := reloaded.GetSchema(table)
	if err != nil {
		t.Fatalf("reload schema: %v", err)
	}
	return schema.GetColumn(column)
}

func TestNonConstantDefaultSurvivesReload(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)")

	reloaded := storage.NewSchemaManager(pool, "testdb")
	reloadedTable := storage.NewTableManager(pool, reloaded, "testdb")
	e2 := newExec(reloaded, reloadedTable)
	clock := newTestClock()
	e2.clock = clock.Now

	col, ok := e2.schemaColumn(t, "t", "created_at")
	if !ok {
		t.Fatal("created_at column missing after reload")
	}
	if col.DefaultExpr != "CURRENT_TIMESTAMP" {
		t.Fatalf("reloaded DefaultExpr = %q, want CURRENT_TIMESTAMP", col.DefaultExpr)
	}
	if col.Default != nil {
		t.Fatalf("reloaded Default = %v, want nil", col.Default)
	}

	execMust(t, e2, "INSERT INTO t (id) VALUES (1)")
	res := execMust(t, e2, "SELECT created_at FROM t WHERE id = 1")
	if got, want := res.Rows[0][0], "2026-01-02 03:04:05"; got != want {
		t.Fatalf("created_at after reload = %v, want %q", got, want)
	}
}

func TestUnsupportedNonConstantDefaultsAreRejected(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	for _, sql := range []string{
		"CREATE TABLE t1 (id INTEGER PRIMARY KEY, token TEXT DEFAULT lower('X'))",
		"CREATE TABLE t2 (id INTEGER PRIMARY KEY, token TEXT DEFAULT upper('x'))",
		"CREATE TABLE t3 (id INTEGER PRIMARY KEY, n INTEGER DEFAULT (2 + 3))",
	} {
		if _, err := execSQL(e, sql); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Fatalf("%s: error = %v, want ErrFeatureNotSupported", sql, err)
		}
	}
}

func TestAlterTableRejectsUnsupportedNonConstantDefault(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	if _, err := execSQL(e, "ALTER TABLE t ADD COLUMN token TEXT DEFAULT lower('X')"); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
		t.Fatalf("ALTER error = %v, want ErrFeatureNotSupported", err)
	}
}

func TestConstantDefaultsStillApplyOnInsert(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, label TEXT DEFAULT 'new', score INTEGER DEFAULT -1, flag BOOLEAN DEFAULT false, note TEXT DEFAULT NULL)")

	execMust(t, e, "INSERT INTO t (id) VALUES (1)")
	res := execMust(t, e, "SELECT label, score, flag, note FROM t WHERE id = 1")
	if res.RowCount != 1 {
		t.Fatalf("row count = %d, want 1", res.RowCount)
	}
	if got := res.Rows[0][0]; got != "new" {
		t.Fatalf("label = %v, want new", got)
	}
	if got := res.Rows[0][1]; got != int64(-1) {
		t.Fatalf("score = %v (%T), want int64(-1)", got, got)
	}
	if got := res.Rows[0][2]; got != false {
		t.Fatalf("flag = %v, want false", got)
	}
	if got := res.Rows[0][3]; got != nil {
		t.Fatalf("note = %v, want NULL", got)
	}
}

func TestAlterTableAddColumnBackfillsExistingRows(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1)")

	execMust(t, e, "ALTER TABLE t ADD COLUMN status TEXT DEFAULT 'pending'")

	res := execMust(t, e, "SELECT id, status FROM t WHERE id = 1")
	if res.RowCount != 1 {
		t.Fatalf("row count = %d, want 1", res.RowCount)
	}
	if got := res.Rows[0][1]; got != "pending" {
		t.Fatalf("existing row status = %v, want pending (ADD COLUMN backfills)", got)
	}

	execMust(t, e, "INSERT INTO t (id) VALUES (2)")
	res = execMust(t, e, "SELECT status FROM t WHERE id = 2")
	if got := res.Rows[0][0]; got != "pending" {
		t.Fatalf("new row status = %v, want pending", got)
	}
}
