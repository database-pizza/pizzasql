package executor

import (
	"errors"
	"sync"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// ADD COLUMN with a constant DEFAULT materializes the value into rows that
// already exist, so the column reads the default everywhere.
func TestAlterTableAddColumnBackfillsPopulatedTable(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1), (2)")

	execMust(t, e, "ALTER TABLE t ADD COLUMN note TEXT DEFAULT 'later'")

	res := execMust(t, e, "SELECT id, note FROM t ORDER BY id")
	if res.RowCount != 2 {
		t.Fatalf("row count = %d, want 2", res.RowCount)
	}
	for _, row := range res.Rows {
		if row[1] != "later" {
			t.Fatalf("existing row %v note = %v, want later", row[0], row[1])
		}
	}

	execMust(t, e, "INSERT INTO t (id) VALUES (3)")
	res = execMust(t, e, "SELECT note FROM t WHERE id = 3")
	if got := res.Rows[0][0]; got != "later" {
		t.Fatalf("new row note = %v, want later", got)
	}
}

// A DEFAULT declared on an empty table has no rows to rewrite; later INSERTs
// that omit the column take it, and an explicit NULL still wins.
func TestAlterTableAddColumnDefaultOnEmptyTable(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	execMust(t, e, "ALTER TABLE t ADD COLUMN note TEXT DEFAULT 'later'")

	execMust(t, e, "INSERT INTO t (id) VALUES (1)")
	execMust(t, e, "INSERT INTO t (id, note) VALUES (2, NULL)")
	res := execMust(t, e, "SELECT id, note FROM t ORDER BY id")
	if len(res.Rows) != 2 || res.Rows[0][1] != "later" || res.Rows[1][1] != nil {
		t.Fatalf("rows = %v, want [[1 later] [2 NULL]]", res.Rows)
	}
}

// ADD COLUMN without a DEFAULT leaves existing rows NULL.
func TestAlterTableAddColumnWithoutDefaultLeavesExistingRowsNull(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1), (2)")

	execMust(t, e, "ALTER TABLE t ADD COLUMN note TEXT")

	res := execMust(t, e, "SELECT id, note FROM t ORDER BY id")
	if res.RowCount != 2 {
		t.Fatalf("row count = %d, want 2", res.RowCount)
	}
	for _, row := range res.Rows {
		if row[1] != nil {
			t.Fatalf("existing row %v note = %v, want NULL", row[0], row[1])
		}
	}
}

// A NOT NULL column with a constant DEFAULT backfills existing rows, so the
// constraint holds for rows that predate the statement.
func TestAlterTableAddColumnNotNullWithDefaultBackfills(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1), (2)")

	execMust(t, e, "ALTER TABLE t ADD COLUMN status TEXT NOT NULL DEFAULT 'pending'")

	fresh := newExec(schema, table)
	res := execMust(t, fresh, "SELECT status FROM t ORDER BY id")
	if res.RowCount != 2 {
		t.Fatalf("row count = %d, want 2", res.RowCount)
	}
	for _, row := range res.Rows {
		if row[0] != "pending" {
			t.Fatalf("existing status = %v, want pending", row[0])
		}
	}

	execMust(t, e, "INSERT INTO t (id) VALUES (3)")
	res = execMust(t, e, "SELECT status FROM t WHERE id = 3")
	if got := res.Rows[0][0]; got != "pending" {
		t.Fatalf("new row status = %v, want pending", got)
	}
}

// A NOT NULL column without a DEFAULT has nothing to write into existing rows
// and must not report the table as migrated.
func TestAlterTableAddColumnNotNullWithoutDefaultRejectsPopulatedTable(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1)")

	_, err := execSQL(e, "ALTER TABLE t ADD COLUMN status TEXT NOT NULL")
	var violation *storage.NotNullViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("error = %v, want NotNullViolationError", err)
	}

	s, err := schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if _, exists := s.GetColumn("status"); exists {
		t.Fatal("rejected ADD COLUMN left the column in the schema")
	}
	if rows := execMust(t, e, "SELECT id FROM t").Rows; len(rows) != 1 {
		t.Fatalf("rows = %v, want the existing row untouched", rows)
	}
}

// The same statement is legal on an empty table; the constraint is then
// enforced on the INSERTs that follow.
func TestAlterTableAddColumnNotNullWithoutDefaultOnEmptyTable(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	execMust(t, e, "ALTER TABLE t ADD COLUMN status TEXT NOT NULL")

	_, err := execSQL(e, "INSERT INTO t (id) VALUES (1)")
	var violation *storage.NotNullViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("insert without status error = %v, want NotNullViolationError", err)
	}

	execMust(t, e, "INSERT INTO t (id, status) VALUES (1, 'active')")
	res := execMust(t, e, "SELECT status FROM t WHERE id = 1")
	if got := res.Rows[0][0]; got != "active" {
		t.Fatalf("status = %v, want active", got)
	}
}

// Backfilling one constant into several rows would duplicate the value, so the
// UNIQUE constraint must reject the ALTER instead of creating a silently
// non-unique column.
func TestAlterTableAddColumnUniqueWithDefaultRejectsPopulatedTable(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1), (2)")

	_, err := execSQL(e, "ALTER TABLE t ADD COLUMN code TEXT DEFAULT 'same' UNIQUE")
	var violation *storage.UniqueViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("error = %v, want UniqueViolationError", err)
	}

	s, err := schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if _, exists := s.GetColumn("code"); exists {
		t.Fatal("rejected ADD COLUMN left the column in the schema")
	}
	indexes, err := schema.ListTableIndexes("t")
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	if len(indexes) != 0 {
		t.Fatalf("indexes = %v, want none", indexes)
	}
}

// A single-row table can take a UNIQUE column with a default, and the
// constraint is enforced for every later INSERT that omits the column. The
// column and the index that validates it must be visible together.
func TestAlterTableAddColumnUniqueWithDefaultEnforcesLaterInserts(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1)")

	execMust(t, e, "ALTER TABLE t ADD COLUMN code TEXT DEFAULT 'same' UNIQUE")

	s, err := schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if _, exists := s.GetColumn("code"); !exists {
		t.Fatal("ADD COLUMN ... UNIQUE published no column")
	}
	indexes, err := schema.ListTableIndexes("t")
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	if len(indexes) != 1 {
		t.Fatalf("indexes = %v, want the one unique index for column code", indexes)
	}
	if !indexes[0].Unique || len(indexes[0].Columns) != 1 || indexes[0].Columns[0].Name != "code" {
		t.Fatalf("index = %+v, want a unique index over code", indexes[0])
	}

	_, err = execSQL(e, "INSERT INTO t (id) VALUES (2)")
	var violation *storage.UniqueViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("insert with duplicated default error = %v, want UniqueViolationError", err)
	}

	execMust(t, e, "INSERT INTO t (id, code) VALUES (2, 'other')")
	res := execMust(t, e, "SELECT code FROM t ORDER BY id")
	if len(res.Rows) != 2 || res.Rows[0][0] != "same" || res.Rows[1][0] != "other" {
		t.Fatalf("codes = %v, want [same other]", res.Rows)
	}
}

// A transaction can stage a row image before a concurrent ADD COLUMN publishes
// a required column. The commit must re-validate that row against the schema now
// in effect instead of persisting a durable NOT NULL violation.
func TestAlterTableAddColumnRejectsRowStagedBeforeDDL(t *testing.T) {
	_, schema, table := newTestDB(t)
	writer := newExec(schema, table)
	ddl := newExec(schema, table)
	execMust(t, writer, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	execMust(t, writer, "BEGIN")
	execMust(t, writer, "INSERT INTO t (id) VALUES (1)")

	// The table is durably empty, so the ALTER accepts the required column.
	execMust(t, ddl, "ALTER TABLE t ADD COLUMN status TEXT NOT NULL")

	_, err := execSQL(writer, "COMMIT")
	var violation *storage.NotNullViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("commit error = %v, want NotNullViolationError", err)
	}
	if got := execMust(t, ddl, "SELECT id FROM t").RowCount; got != 0 {
		t.Fatalf("rows after rejected commit = %d, want 0", got)
	}
}

// The same race is harmless when the new column has a default: the commit
// materializes it into the staged row, exactly as the backfill did for rows that
// were already durable.
func TestAlterTableAddColumnDefaultsRowStagedBeforeDDL(t *testing.T) {
	_, schema, table := newTestDB(t)
	writer := newExec(schema, table)
	ddl := newExec(schema, table)
	execMust(t, writer, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	execMust(t, writer, "BEGIN")
	execMust(t, writer, "INSERT INTO t (id) VALUES (1)")

	execMust(t, ddl, "ALTER TABLE t ADD COLUMN status TEXT NOT NULL DEFAULT 'pending'")
	execMust(t, writer, "COMMIT")

	res := execMust(t, ddl, "SELECT id, status FROM t")
	if len(res.Rows) != 1 || res.Rows[0][1] != "pending" {
		t.Fatalf("rows = %v, want [[1 pending]]", res.Rows)
	}
}

// IF NOT EXISTS stays idempotent on a populated table: the second statement is
// a no-op even when it declares a different default.
func TestAlterTableAddColumnIfNotExistsKeepsBackfilledValues(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1)")

	execMust(t, e, "ALTER TABLE t ADD COLUMN IF NOT EXISTS note TEXT DEFAULT 'first'")
	execMust(t, e, "ALTER TABLE t ADD COLUMN IF NOT EXISTS note TEXT DEFAULT 'second'")

	res := execMust(t, e, "SELECT note FROM t WHERE id = 1")
	if got := res.Rows[0][0]; got != "first" {
		t.Fatalf("note = %v, want the first default to stand", got)
	}

	s, err := schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	count := 0
	for _, col := range s.Columns {
		if col.Name == "note" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("column note appears %d times, want 1", count)
	}
}

func TestAlterTableAddColumnIfNotExistsIsIdempotent(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	execMust(t, e, "ALTER TABLE t ADD COLUMN IF NOT EXISTS c TEXT")
	execMust(t, e, "ALTER TABLE t ADD COLUMN IF NOT EXISTS c TEXT")

	s, err := schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	count := 0
	for _, col := range s.Columns {
		if col.Name == "c" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("column c appears %d times, want 1", count)
	}
}

// Concurrent IF NOT EXISTS ADD COLUMN on one engine instance must not duplicate
// metadata; the schema manager serializes the check-and-append.
func TestAlterTableAddColumnIfNotExistsConcurrent(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := newExec(schema, table)
			if _, err := execSQL(session, "ALTER TABLE t ADD COLUMN IF NOT EXISTS c TEXT"); err != nil {
				t.Errorf("add column: %v", err)
			}
		}()
	}
	wg.Wait()

	s, err := schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	count := 0
	for _, col := range s.Columns {
		if col.Name == "c" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("column c appears %d times, want 1", count)
	}
}

// DROP COLUMN rewrites every remaining row so the removed value does not stay
// in durable storage.
func TestAlterTableDropColumnRewritesPopulatedRows(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, keep TEXT, dropme TEXT)")
	execMust(t, e, "INSERT INTO t (id, keep, dropme) VALUES (1, 'a', 'secret'), (2, 'b', 'secret')")

	execMust(t, e, "ALTER TABLE t DROP COLUMN dropme")

	res := execMust(t, e, "SELECT * FROM t ORDER BY id")
	if len(res.Columns) != 2 || res.Columns[0] != "id" || res.Columns[1] != "keep" {
		t.Fatalf("columns after drop = %v, want [id keep]", res.Columns)
	}
	if res.RowCount != 2 || res.Rows[0][1] != "a" || res.Rows[1][1] != "b" {
		t.Fatalf("rows after drop = %v", res.Rows)
	}
	if _, err := execSQL(e, "SELECT dropme FROM t"); err == nil {
		t.Fatal("expected dropped column to be unknown")
	}
}

// RENAME COLUMN rewrites row keys but deliberately leaves index metadata
// unchanged. Documented guidance: drop and recreate indexes after a rename.
func TestAlterTableRenameColumnLeavesIndexMetadataStale(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT)")
	execMust(t, e, "INSERT INTO t (id, a) VALUES (1, 'x')")
	execMust(t, e, "CREATE INDEX idx_a ON t(a)")

	execMust(t, e, "ALTER TABLE t RENAME COLUMN a TO aa")

	res := execMust(t, e, "SELECT aa FROM t WHERE id = 1")
	if got := res.Rows[0][0]; got != "x" {
		t.Fatalf("renamed value = %v, want x", got)
	}

	names, err := schema.ListIndexes()
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	found := false
	for _, name := range names {
		if name == "idx_a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("indexes = %v, want idx_a to still exist (rename does not rewrite index metadata)", names)
	}
}
