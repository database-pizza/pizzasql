package executor

import (
	"strings"
	"sync"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// ADD COLUMN only appends schema metadata. Existing rows are not rewritten, so
// a declared DEFAULT applies to later INSERTs only.
func TestAlterTableAddColumnDoesNotBackfillPopulatedTable(t *testing.T) {
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
		if row[1] != nil {
			t.Fatalf("existing row %v note = %v, want NULL (no backfill)", row[0], row[1])
		}
	}

	execMust(t, e, "INSERT INTO t (id) VALUES (3)")
	res = execMust(t, e, "SELECT note FROM t WHERE id = 3")
	if got := res.Rows[0][0]; got != "later" {
		t.Fatalf("new row note = %v, want later", got)
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
