package executor

import (
	"sync"
	"testing"
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
			if _, err := execSQL(e, "ALTER TABLE t ADD COLUMN IF NOT EXISTS c TEXT"); err != nil {
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
