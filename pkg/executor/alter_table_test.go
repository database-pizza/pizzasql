package executor

import (
	"errors"
	"strings"
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

// RENAME COLUMN rewrites the durable index definitions along with the schema
// and rows, so single- and multi-column indexes name the new column while
// keeping their original index names.
func TestAlterTableRenameColumnUpdatesIndexMetadata(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, b TEXT)")
	execMust(t, e, "INSERT INTO t (id, a, b) VALUES (1, 'x', 'y')")
	execMust(t, e, "CREATE INDEX idx_a ON t(a)")
	execMust(t, e, "CREATE INDEX idx_ab ON t(a, b)")

	execMust(t, e, "ALTER TABLE t RENAME COLUMN a TO aa")

	single, err := schema.GetIndex("idx_a")
	if err != nil {
		t.Fatalf("get idx_a: %v", err)
	}
	if len(single.Columns) != 1 || single.Columns[0].Name != "aa" {
		t.Fatalf("idx_a columns = %#v, want [aa]", single.Columns)
	}

	composite, err := schema.GetIndex("idx_ab")
	if err != nil {
		t.Fatalf("get idx_ab: %v", err)
	}
	if len(composite.Columns) != 2 || composite.Columns[0].Name != "aa" || composite.Columns[1].Name != "b" {
		t.Fatalf("idx_ab columns = %#v, want [aa b]", composite.Columns)
	}

	// index_info reports the renamed column and resolves its column id.
	res := execMust(t, e, "PRAGMA index_info(idx_a)")
	if len(res.Rows) != 1 || res.Rows[0][1] != int64(1) || res.Rows[0][2] != "aa" {
		t.Fatalf("index_info(idx_a) = %v, want [[0 1 aa]]", res.Rows)
	}

	// sqlite_master renders the recreated index over the new column name.
	res = execMust(t, e, "SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_ab'")
	if len(res.Rows) != 1 || !strings.Contains(res.Rows[0][0].(string), "(`aa`, `b`)") {
		t.Fatalf("recreated idx_ab SQL = %v, want a definition over aa, b", res.Rows)
	}

	// The definition is durable, not just cached on this SchemaManager.
	reloaded := storage.NewSchemaManager(pool, "testdb")
	composite, err = reloaded.GetIndex("idx_ab")
	if err != nil {
		t.Fatalf("reload idx_ab: %v", err)
	}
	if len(composite.Columns) != 2 || composite.Columns[0].Name != "aa" || composite.Columns[1].Name != "b" {
		t.Fatalf("reloaded idx_ab columns = %#v, want [aa b]", composite.Columns)
	}

	// Index-backed equality lookup still returns the row after the rename.
	res = execMust(t, e, "SELECT id, aa FROM t WHERE aa = 'x'")
	if res.RowCount != 1 || res.Rows[0][0] != int64(1) || res.Rows[0][1] != "x" {
		t.Fatalf("indexed lookup after rename = %v, want [[1 x]]", res.Rows)
	}
}

// A UNIQUE index keeps enforcing uniqueness after its column is renamed.
func TestAlterTableRenameColumnKeepsUniqueIndexEnforcement(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, email TEXT, tag TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX uq_email ON t(email)")
	execMust(t, e, "INSERT INTO t (id, email, tag) VALUES (1, 'a', 'first')")

	if _, err := execSQL(e, "INSERT INTO t (id, email, tag) VALUES (2, 'a', 'dup')"); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("pre-rename duplicate error = %v, want UNIQUE constraint failure", err)
	}

	execMust(t, e, "ALTER TABLE t RENAME COLUMN email TO mail")

	if _, err := execSQL(e, "INSERT INTO t (id, mail, tag) VALUES (3, 'a', 'dup')"); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("post-rename duplicate error = %v, want UNIQUE constraint failure", err)
	}
	execMust(t, e, "INSERT INTO t (id, mail, tag) VALUES (4, 'b', 'ok')")

	res := execMust(t, e, "SELECT id, mail, tag FROM t ORDER BY id")
	if res.RowCount != 2 || res.Rows[0][1] != "a" || res.Rows[1][1] != "b" {
		t.Fatalf("rows after rename = %v, want two distinct emails", res.Rows)
	}

	res = execMust(t, e, "PRAGMA index_list(t)")
	if len(res.Rows) != 1 || res.Rows[0][1] != "uq_email" || res.Rows[0][2] != int64(1) {
		t.Fatalf("index_list(t) = %v, want the unique uq_email index", res.Rows)
	}
}

// ON CONFLICT resolves against the renamed column and rejects the old name.
func TestAlterTableRenameColumnResolvesOnConflictTarget(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, email TEXT, tag TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX uq_email ON t(email)")
	execMust(t, e, "INSERT INTO t (id, email, tag) VALUES (1, 'a', 'old')")

	execMust(t, e, "ALTER TABLE t RENAME COLUMN email TO mail")

	execMust(t, e, "INSERT INTO t (id, mail, tag) VALUES (2, 'a', 'new') ON CONFLICT (mail) DO UPDATE SET tag = excluded.tag")
	res := execMust(t, e, "SELECT id, mail, tag FROM t")
	if res.RowCount != 1 || res.Rows[0][0] != int64(1) || res.Rows[0][2] != "new" {
		t.Fatalf("upsert on renamed column = %v, want the existing row updated", res.Rows)
	}

	if _, err := execSQL(e, "INSERT INTO t (id, mail, tag) VALUES (3, 'a', 'x') ON CONFLICT (email) DO UPDATE SET tag = excluded.tag"); err == nil || !strings.Contains(err.Error(), "column not found: email") {
		t.Fatalf("upsert on old column error = %v, want column not found: email", err)
	}
}

// Renaming a column back restores the original index metadata, and renaming to
// an existing column name still fails without touching that metadata.
func TestAlterTableRenameColumnCycleRestoresIndexMetadata(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX uq_a ON t(a)")
	execMust(t, e, "INSERT INTO t (id, a) VALUES (1, 'x')")

	execMust(t, e, "ALTER TABLE t RENAME COLUMN a TO b")
	execMust(t, e, "ALTER TABLE t RENAME COLUMN b TO a")

	idx, err := schema.GetIndex("uq_a")
	if err != nil {
		t.Fatalf("get uq_a: %v", err)
	}
	if len(idx.Columns) != 1 || idx.Columns[0].Name != "a" {
		t.Fatalf("uq_a columns after cycle = %#v, want [a]", idx.Columns)
	}
	if _, err := execSQL(e, "INSERT INTO t (id, a) VALUES (2, 'x')"); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("duplicate error after rename cycle = %v, want UNIQUE constraint failure", err)
	}

	if _, err := execSQL(e, "ALTER TABLE t RENAME COLUMN a TO id"); err == nil || !strings.Contains(err.Error(), "column already exists: id") {
		t.Fatalf("rename to existing column error = %v, want column already exists: id", err)
	}
	idx, err = schema.GetIndex("uq_a")
	if err != nil {
		t.Fatalf("get uq_a after rejected rename: %v", err)
	}
	if len(idx.Columns) != 1 || idx.Columns[0].Name != "a" {
		t.Fatalf("uq_a columns after rejected rename = %#v, want [a]", idx.Columns)
	}
}

// A column referenced by an expression index cannot be renamed: the engine
// stores expression text verbatim, so the rename is rejected instead of leaving
// a UNIQUE expression index silently unbound from the column it enforces.
func TestAlterTableRenameColumnRejectsExpressionIndexDependency(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, email TEXT, tag TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX uq_lower_email ON t(lower(email))")
	execMust(t, e, "INSERT INTO t (id, email, tag) VALUES (1, 'a@x', 'keep')")

	_, err := execSQL(e, "ALTER TABLE t RENAME COLUMN email TO mail")
	if err == nil {
		t.Fatal("rename of a column used by an expression index must be rejected")
	}
	for _, want := range []string{"uq_lower_email", "lower(email)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("rename error = %v, want it to name %q", err, want)
		}
	}

	// Nothing was renamed, and the UNIQUE expression index still enforces.
	sch, err := schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if _, ok := sch.GetColumn("email"); !ok {
		t.Fatal("rejected rename removed the email column")
	}
	if _, ok := sch.GetColumn("mail"); ok {
		t.Fatal("rejected rename added the mail column")
	}
	if _, err := execSQL(e, "INSERT INTO t (id, email, tag) VALUES (2, 'a@x', 'dup')"); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("duplicate on expression index after rejected rename = %v, want UNIQUE constraint failure", err)
	}

	// A column the expression does not reference can still be renamed.
	execMust(t, e, "ALTER TABLE t RENAME COLUMN tag TO label")
	idx, err := schema.GetIndex("uq_lower_email")
	if err != nil {
		t.Fatalf("get uq_lower_email: %v", err)
	}
	if len(idx.Columns) != 1 || idx.Columns[0].Expression != "lower(email)" {
		t.Fatalf("uq_lower_email columns = %#v, want the untouched lower(email) expression", idx.Columns)
	}
	if res := execMust(t, e, "SELECT label FROM t WHERE id = 1"); res.RowCount != 1 || res.Rows[0][0] != "keep" {
		t.Fatalf("renamed label = %v, want keep", res.Rows)
	}
}

// Conflict targets that name a hidden rowid alias are not table columns but are
// still valid targets, including after a rename.
func TestAlterTableRenameColumnKeepsRowIDConflictTarget(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT)")
	execMust(t, e, "INSERT INTO t (id, a) VALUES (1, 'x')")

	execMust(t, e, "ALTER TABLE t RENAME COLUMN a TO b")

	execMust(t, e, "INSERT INTO t (id, b) VALUES (2, 'y') ON CONFLICT (rowid) DO NOTHING")
	execMust(t, e, "INSERT INTO t (id, b) VALUES (3, 'z') ON CONFLICT (oid) DO NOTHING")
	execMust(t, e, "INSERT INTO t (id, b) VALUES (4, 'w') ON CONFLICT (_rowid_) DO NOTHING")

	if res := execMust(t, e, "SELECT count(*) FROM t"); res.Rows[0][0] != int64(4) {
		t.Fatalf("row count = %v, want 4", res.Rows[0][0])
	}
}
