package executor

import (
	"errors"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

// Non-constant defaults cannot be stored in the schema catalog without freezing
// their value at DDL time, so they must be rejected instead of silently
// producing a constant.
func TestCreateTableRejectsNonConstantDefaults(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	for _, sql := range []string{
		"CREATE TABLE t1 (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)",
		"CREATE TABLE t2 (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT datetime('now'))",
		"CREATE TABLE t3 (id INTEGER PRIMARY KEY, token TEXT DEFAULT lower('X'))",
	} {
		if _, err := execSQL(e, sql); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Fatalf("%s: error = %v, want ErrFeatureNotSupported", sql, err)
		}
	}
}

func TestAlterTableRejectsNonConstantDefault(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	if _, err := execSQL(e, "ALTER TABLE t ADD COLUMN created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP"); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
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

func TestAlterTableAddColumnDoesNotBackfill(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1)")

	execMust(t, e, "ALTER TABLE t ADD COLUMN status TEXT DEFAULT 'pending'")

	res := execMust(t, e, "SELECT id, status FROM t WHERE id = 1")
	if res.RowCount != 1 {
		t.Fatalf("row count = %d, want 1", res.RowCount)
	}
	if got := res.Rows[0][1]; got != nil {
		t.Fatalf("existing row status = %v, want NULL (ADD COLUMN does not backfill)", got)
	}

	execMust(t, e, "INSERT INTO t (id) VALUES (2)")
	res = execMust(t, e, "SELECT status FROM t WHERE id = 2")
	if got := res.Rows[0][0]; got != "pending" {
		t.Fatalf("new row status = %v, want pending", got)
	}
}
