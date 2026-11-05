package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// requireForeignKeyViolation asserts err is a foreign-key violation and returns
// its label so a test can check which constraint fired.
func requireForeignKeyViolation(t *testing.T, err error) *storage.ForeignKeyViolationError {
	t.Helper()
	var violation *storage.ForeignKeyViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("error = %v, want ForeignKeyViolationError", err)
	}
	if !strings.HasPrefix(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("message = %q, want the FOREIGN KEY failure prefix", err.Error())
	}
	return violation
}

func requireUnsupported(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", want)
	}
	if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
		t.Fatalf("error = %v, want ErrFeatureNotSupported (0A000)", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err, want)
	}
}

// Inline REFERENCES, table-level FOREIGN KEY, and the named forms all persist
// and enforce the same constraint.
func TestForeignKeyDeclarationForms(t *testing.T) {
	tests := []struct {
		name       string
		ddl        string
		columns    string
		refColumns string
	}{
		{
			name:       "inline",
			ddl:        "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))",
			columns:    "parent_id",
			refColumns: "id",
		},
		{
			name:       "inline without column list uses the parent primary key",
			ddl:        "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent)",
			columns:    "parent_id",
			refColumns: "id",
		},
		{
			name:       "table level",
			ddl:        "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, FOREIGN KEY (parent_id) REFERENCES parent(id))",
			columns:    "parent_id",
			refColumns: "id",
		},
		{
			name:       "named table level",
			ddl:        "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, CONSTRAINT child_parent FOREIGN KEY (parent_id) REFERENCES parent(id) ON DELETE RESTRICT ON UPDATE NO ACTION)",
			columns:    "parent_id",
			refColumns: "id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, schema, table := newTestDB(t)
			e := newExec(schema, table)
			execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
			execMust(t, e, tt.ddl)

			stored, err := schema.GetSchema("child")
			if err != nil {
				t.Fatalf("get schema: %v", err)
			}
			if len(stored.ForeignKeys) != 1 {
				t.Fatalf("foreign keys = %+v, want exactly one", stored.ForeignKeys)
			}
			fk := stored.ForeignKeys[0]
			if strings.Join(fk.Columns, ",") != tt.columns {
				t.Fatalf("columns = %v, want %s", fk.Columns, tt.columns)
			}
			if fk.RefTable != "parent" {
				t.Fatalf("ref table = %q, want parent", fk.RefTable)
			}
			if strings.Join(fk.RefColumns, ",") != tt.refColumns {
				t.Fatalf("ref columns = %v, want %s", fk.RefColumns, tt.refColumns)
			}

			execMust(t, e, "INSERT INTO parent (id) VALUES (1)")
			execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")
			execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (11, NULL)")
			_, err = execSQL(e, "INSERT INTO child (id, parent_id) VALUES (12, 2)")
			requireForeignKeyViolation(t, err)
		})
	}
}

// A composite foreign key matches the parent key component by component.
func TestForeignKeyComposite(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (a TEXT, b INTEGER, PRIMARY KEY (a, b))")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, pa TEXT, pb INTEGER, FOREIGN KEY (pa, pb) REFERENCES parent(a, b))")

	stored, err := schema.GetSchema("child")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if len(stored.ForeignKeys) != 1 || strings.Join(stored.ForeignKeys[0].RefColumns, ",") != "a,b" {
		t.Fatalf("foreign keys = %+v, want (pa,pb) -> parent(a,b)", stored.ForeignKeys)
	}

	execMust(t, e, "INSERT INTO parent (a, b) VALUES ('x', 1)")
	execMust(t, e, "INSERT INTO child (id, pa, pb) VALUES (1, 'x', 1)")

	_, err = execSQL(e, "INSERT INTO child (id, pa, pb) VALUES (2, 'x', 2)")
	requireForeignKeyViolation(t, err)
	_, err = execSQL(e, "INSERT INTO child (id, pa, pb) VALUES (3, 'x', 1.5)")
	requireForeignKeyViolation(t, err)

	// Any NULL component disables the constraint (MATCH SIMPLE).
	execMust(t, e, "INSERT INTO child (id, pa, pb) VALUES (4, NULL, 1)")
	execMust(t, e, "INSERT INTO child (id, pa, pb) VALUES (5, 'x', NULL)")

	if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(3) {
		t.Fatalf("child rows = %v, want 3", res.Rows)
	}
}

// A child INSERT with a missing parent is rejected before anything is written.
func TestForeignKeyInsertViolation(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")

	_, err := execSQL(e, "INSERT INTO child (id, parent_id) VALUES (1, 42)")
	requireForeignKeyViolation(t, err)
	if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(0) {
		t.Fatalf("child rows after rejected insert = %v, want 0", res.Rows)
	}

	execMust(t, e, "INSERT INTO parent (id) VALUES (42)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (1, 42)")
}

// A multi-row INSERT is atomic: one dangling row rejects the whole statement.
func TestForeignKeyMultiRowInsertIsAtomic(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1)")

	_, err := execSQL(e, "INSERT INTO child (id, parent_id) VALUES (10, 1), (11, 2), (12, 1)")
	requireForeignKeyViolation(t, err)
	if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(0) {
		t.Fatalf("child rows after rejected multi-row insert = %v, want 0", res.Rows)
	}

	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1), (12, 1)")
	if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(2) {
		t.Fatalf("child rows = %v, want 2", res.Rows)
	}
}

// Updating the child foreign-key column to a missing parent is rejected, and a
// valid update is accepted.
func TestForeignKeyChildUpdate(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1), (2)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")

	_, err := execSQL(e, "UPDATE child SET parent_id = 99 WHERE id = 10")
	requireForeignKeyViolation(t, err)
	res := execMust(t, e, "SELECT parent_id FROM child WHERE id = 10")
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("parent_id after rejected update = %v, want 1", res.Rows[0][0])
	}

	execMust(t, e, "UPDATE child SET parent_id = 2 WHERE id = 10")
	res = execMust(t, e, "SELECT parent_id FROM child WHERE id = 10")
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("parent_id after valid update = %v, want 2", res.Rows[0][0])
	}

	// The same check applies to the update half of an upsert.
	_, err = execSQL(e, "INSERT INTO child (id, parent_id) VALUES (10, 99) ON CONFLICT (id) DO UPDATE SET parent_id = excluded.parent_id")
	requireForeignKeyViolation(t, err)
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1) ON CONFLICT (id) DO UPDATE SET parent_id = excluded.parent_id")
	res = execMust(t, e, "SELECT parent_id FROM child WHERE id = 10")
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("parent_id after upsert = %v, want 1", res.Rows[0][0])
	}
}

// Deleting a referenced parent is rejected; deleting the child first makes it
// possible, and a parent with no children can always be deleted.
func TestForeignKeyParentDelete(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1), (2)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")

	_, err := execSQL(e, "DELETE FROM parent WHERE id = 1")
	requireForeignKeyViolation(t, err)
	if res := execMust(t, e, "SELECT count(*) FROM parent"); res.Rows[0][0] != int64(2) {
		t.Fatalf("parent rows after rejected delete = %v, want 2", res.Rows)
	}

	// A scan-based delete takes the same path through the transaction commit.
	_, err = execSQL(e, "DELETE FROM parent")
	requireForeignKeyViolation(t, err)

	execMust(t, e, "DELETE FROM child WHERE id = 10")
	execMust(t, e, "DELETE FROM parent WHERE id = 1")
	execMust(t, e, "DELETE FROM parent WHERE id = 2")
}

// Updating a referenced parent key is rejected while a child points at it and
// accepted once nothing references the old value.
func TestForeignKeyParentKeyUpdate(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id, v) VALUES (1, 'one'), (2, 'two')")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")

	// A non-referenced column update is always allowed.
	execMust(t, e, "UPDATE parent SET v = 'uno' WHERE id = 1")

	_, err := execSQL(e, "UPDATE parent SET id = 5 WHERE id = 1")
	requireForeignKeyViolation(t, err)
	res := execMust(t, e, "SELECT id FROM parent WHERE v = 'uno'")
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("parent id after rejected update = %v, want 1", res.Rows[0][0])
	}

	execMust(t, e, "DELETE FROM child WHERE id = 10")
	execMust(t, e, "UPDATE parent SET id = 5 WHERE id = 1")
	if res := execMust(t, e, "SELECT id FROM parent WHERE v = 'uno'"); res.Rows[0][0] != int64(5) {
		t.Fatalf("parent id after valid update = %v, want 5", res.Rows[0][0])
	}
}

// A row may reference itself and another row of the same table; a dangling
// self-reference is rejected.
func TestForeignKeySelfReference(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE nodes (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES nodes(id))")

	execMust(t, e, "INSERT INTO nodes (id, parent_id) VALUES (1, 1)")
	execMust(t, e, "INSERT INTO nodes (id, parent_id) VALUES (2, 1)")
	execMust(t, e, "INSERT INTO nodes (id, parent_id) VALUES (3, NULL)")

	_, err := execSQL(e, "INSERT INTO nodes (id, parent_id) VALUES (4, 99)")
	requireForeignKeyViolation(t, err)

	// Row 1 exists and references itself, but row 2 also references it, so the
	// delete is rejected because another row still points at it.
	_, err = execSQL(e, "DELETE FROM nodes WHERE id = 1")
	requireForeignKeyViolation(t, err)

	// A row that only references itself can be deleted: the row and the
	// reference leave together.
	execMust(t, e, "INSERT INTO nodes (id, parent_id) VALUES (5, 5)")
	execMust(t, e, "DELETE FROM nodes WHERE id = 5")

	// Once the other child is gone, node 1's self-reference no longer blocks.
	execMust(t, e, "DELETE FROM nodes WHERE id = 2")
	execMust(t, e, "DELETE FROM nodes WHERE id = 1")
	execMust(t, e, "INSERT INTO nodes (id, parent_id) VALUES (1, 1)")

	// A composite self-reference resolves against this table's table-level key.
	execMust(t, e, `CREATE TABLE tree (
		a TEXT NOT NULL,
		b TEXT NOT NULL,
		pa TEXT,
		pb TEXT,
		PRIMARY KEY (a, b),
		FOREIGN KEY (pa, pb) REFERENCES tree(a, b)
	)`)
	execMust(t, e, "INSERT INTO tree (a, b, pa, pb) VALUES ('r', '1', 'r', '1')")
	execMust(t, e, "INSERT INTO tree (a, b, pa, pb) VALUES ('c', '1', 'r', '1')")
	_, err = execSQL(e, "INSERT INTO tree (a, b, pa, pb) VALUES ('x', '1', 'missing', '1')")
	requireForeignKeyViolation(t, err)
}

// A transaction is validated at COMMIT against its final state, so a staged
// child insert and a staged parent delete cannot commit into a dangling
// reference in either order.
func TestForeignKeyTransactionCommitEnforcement(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1)")

	t.Run("staged child insert then parent delete", func(t *testing.T) {
		execMust(t, e, "BEGIN")
		execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")
		execMust(t, e, "DELETE FROM parent WHERE id = 1")
		_, err := execSQL(e, "COMMIT")
		requireForeignKeyViolation(t, err)

		if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(0) {
			t.Fatalf("child rows after rejected commit = %v, want 0", res.Rows)
		}
		if res := execMust(t, e, "SELECT count(*) FROM parent"); res.Rows[0][0] != int64(1) {
			t.Fatalf("parent rows after rejected commit = %v, want 1", res.Rows)
		}
	})

	t.Run("parent delete then staged child insert", func(t *testing.T) {
		execMust(t, e, "BEGIN")
		execMust(t, e, "DELETE FROM parent WHERE id = 1")
		execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")
		_, err := execSQL(e, "COMMIT")
		requireForeignKeyViolation(t, err)
		execMust(t, e, "SELECT count(*) FROM parent")
	})

	t.Run("staged child insert with missing parent", func(t *testing.T) {
		execMust(t, e, "BEGIN")
		execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (11, 77)")
		_, err := execSQL(e, "COMMIT")
		requireForeignKeyViolation(t, err)
		if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(0) {
			t.Fatalf("child rows after rejected commit = %v, want 0", res.Rows)
		}
	})
}

// Deleting a parent and its children in one transaction succeeds in either
// order because the commit sees the final, consistent state.
func TestForeignKeyTransactionDeleteParentAndChild(t *testing.T) {
	for _, childrenFirst := range []bool{false, true} {
		name := "parent first"
		if childrenFirst {
			name = "children first"
		}
		t.Run(name, func(t *testing.T) {
			_, schema, table := newTestDB(t)
			e := newExec(schema, table)
			execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
			execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
			execMust(t, e, "INSERT INTO parent (id) VALUES (1), (2)")
			execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1), (11, 1)")

			execMust(t, e, "BEGIN")
			if childrenFirst {
				execMust(t, e, "DELETE FROM child WHERE parent_id = 1")
				execMust(t, e, "DELETE FROM parent WHERE id = 1")
			} else {
				execMust(t, e, "DELETE FROM parent WHERE id = 1")
				execMust(t, e, "DELETE FROM child WHERE parent_id = 1")
			}
			execMust(t, e, "COMMIT")

			if res := execMust(t, e, "SELECT count(*) FROM parent"); res.Rows[0][0] != int64(1) {
				t.Fatalf("parent rows = %v, want 1", res.Rows)
			}
			if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(0) {
				t.Fatalf("child rows = %v, want 0", res.Rows)
			}
		})
	}
}

// A parent column is only a valid target when it is the primary key or covered
// by a UNIQUE index, and forward references fail at CREATE time.
func TestForeignKeyDDLRejections(t *testing.T) {
	tests := []struct {
		name  string
		setup []string
		ddl   string
		want  string
	}{
		{
			name: "parent does not exist",
			ddl:  "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))",
			want: "does not exist",
		},
		{
			name:  "parent columns are not unique",
			setup: []string{"CREATE TABLE parent (id INTEGER PRIMARY KEY, code TEXT)"},
			ddl:   "CREATE TABLE child (id INTEGER PRIMARY KEY, code TEXT REFERENCES parent(code))",
			want:  "foreign key mismatch",
		},
		{
			name:  "composite parent columns are not a unique set",
			setup: []string{"CREATE TABLE parent (a TEXT UNIQUE, b INTEGER)"},
			ddl:   "CREATE TABLE child (id INTEGER PRIMARY KEY, a TEXT, b INTEGER, FOREIGN KEY (a, b) REFERENCES parent(a, b))",
			want:  "foreign key mismatch",
		},
		{
			name:  "unknown child column",
			setup: []string{"CREATE TABLE parent (id INTEGER PRIMARY KEY)"},
			ddl:   "CREATE TABLE child (id INTEGER PRIMARY KEY, FOREIGN KEY (missing) REFERENCES parent(id))",
			want:  `column "missing"`,
		},
		{
			name:  "unknown parent column",
			setup: []string{"CREATE TABLE parent (id INTEGER PRIMARY KEY)"},
			ddl:   "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(missing))",
			want:  "does not exist in table parent",
		},
		{
			name:  "column count mismatch",
			setup: []string{"CREATE TABLE parent (a TEXT, b TEXT, PRIMARY KEY (a, b))"},
			ddl:   "CREATE TABLE child (id INTEGER PRIMARY KEY, a TEXT, FOREIGN KEY (a) REFERENCES parent(a, b))",
			want:  "same number of columns",
		},
		{
			name:  "on delete cascade",
			setup: []string{"CREATE TABLE parent (id INTEGER PRIMARY KEY)"},
			ddl:   "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE)",
			want:  "cascades are not implemented",
		},
		{
			name:  "on update cascade",
			setup: []string{"CREATE TABLE parent (id INTEGER PRIMARY KEY)"},
			ddl:   "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, FOREIGN KEY (parent_id) REFERENCES parent(id) ON UPDATE CASCADE)",
			want:  "cascades are not implemented",
		},
		{
			name:  "on delete set null",
			setup: []string{"CREATE TABLE parent (id INTEGER PRIMARY KEY)"},
			ddl:   "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, FOREIGN KEY (parent_id) REFERENCES parent(id) ON DELETE SET NULL)",
			want:  "cascades are not implemented",
		},
		{
			name:  "on delete set default",
			setup: []string{"CREATE TABLE parent (id INTEGER PRIMARY KEY)"},
			ddl:   "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, FOREIGN KEY (parent_id) REFERENCES parent(id) ON DELETE SET DEFAULT)",
			want:  "cascades are not implemented",
		},
		{
			name:  "deferrable",
			setup: []string{"CREATE TABLE parent (id INTEGER PRIMARY KEY)"},
			ddl:   "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)",
			want:  "DEFERRABLE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, schema, table := newTestDB(t)
			e := newExec(schema, table)
			for _, sql := range tt.setup {
				execMust(t, e, sql)
			}
			_, err := execSQL(e, tt.ddl)
			requireUnsupported(t, err, tt.want)
			if schema.TableExists("child") {
				t.Fatal("rejected CREATE TABLE left a schema behind")
			}
		})
	}

	// ALTER TABLE ADD COLUMN still cannot introduce a foreign key.
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	_, err := execSQL(e, "ALTER TABLE t ADD COLUMN parent_id INTEGER REFERENCES parent(id)")
	requireUnsupported(t, err, "CREATE TABLE")
}

// The persisted constraint keeps being enforced after the schema is re-read
// from storage.
func TestForeignKeyDurableReload(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, CONSTRAINT child_parent FOREIGN KEY (parent_id) REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")

	reloaded := storage.NewSchemaManager(pool, "testdb")
	reopened := newExec(reloaded, storage.NewTableManager(pool, reloaded, "testdb"))

	stored, err := reloaded.GetSchema("child")
	if err != nil {
		t.Fatalf("reload schema: %v", err)
	}
	if len(stored.ForeignKeys) != 1 || stored.ForeignKeys[0].Name != "child_parent" {
		t.Fatalf("reloaded foreign keys = %+v, want child_parent", stored.ForeignKeys)
	}

	_, err = execSQL(reopened, "INSERT INTO child (id, parent_id) VALUES (11, 2)")
	requireForeignKeyViolation(t, err)
	_, err = execSQL(reopened, "DELETE FROM parent WHERE id = 1")
	requireForeignKeyViolation(t, err)
	execMust(t, reopened, "INSERT INTO child (id, parent_id) VALUES (12, 1)")
}

// The emulated SQLite catalog reports the foreign key in the table SQL.
func TestForeignKeyInSQLiteCatalog(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, CONSTRAINT child_parent FOREIGN KEY (parent_id) REFERENCES parent(id))")

	res := execMust(t, e, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'child'")
	if res.RowCount != 1 {
		t.Fatalf("sqlite_master rows = %d, want 1", res.RowCount)
	}
	sql, _ := res.Rows[0][0].(string)
	for _, want := range []string{"CONSTRAINT `child_parent` FOREIGN KEY (`parent_id`) REFERENCES `parent` (`id`)"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("catalog SQL %q does not contain %q", sql, want)
		}
	}

	res = execMust(t, e, "SELECT sql FROM sqlite_schema WHERE name = 'child'")
	if res.RowCount != 1 || !strings.Contains(res.Rows[0][0].(string), "FOREIGN KEY") {
		t.Fatalf("sqlite_schema SQL = %v, want the foreign key", res.Rows)
	}

	// Declared referential actions round-trip through the catalog text.
	execMust(t, e, `CREATE TABLE child_actions (
		id INTEGER PRIMARY KEY,
		parent_id INTEGER,
		CONSTRAINT child_parent_actions FOREIGN KEY (parent_id) REFERENCES parent(id) ON DELETE RESTRICT ON UPDATE NO ACTION
	)`)
	res = execMust(t, e, "SELECT sql FROM sqlite_master WHERE name = 'child_actions'")
	sql, _ = res.Rows[0][0].(string)
	want := "CONSTRAINT `child_parent_actions` FOREIGN KEY (`parent_id`) REFERENCES `parent` (`id`) ON DELETE RESTRICT ON UPDATE NO ACTION"
	if !strings.Contains(sql, want) {
		t.Fatalf("catalog SQL %q does not contain %q", sql, want)
	}
}

// Foreign keys coexist with the composite primary key, CHECK, and default
// support already in the engine.
func TestForeignKeyRegressionCompositeKeyCheckDefaults(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE parent (
		a TEXT,
		b INTEGER,
		PRIMARY KEY (a, b)
	)`)
	execMust(t, e, `CREATE TABLE child (
		id INTEGER PRIMARY KEY,
		pa TEXT NOT NULL,
		pb INTEGER NOT NULL,
		state TEXT NOT NULL DEFAULT 'new' CHECK (state IN ('new', 'done')),
		FOREIGN KEY (pa, pb) REFERENCES parent(a, b)
	)`)

	execMust(t, e, "INSERT INTO parent (a, b) VALUES ('x', 1)")
	execMust(t, e, "INSERT INTO child (id, pa, pb) VALUES (1, 'x', 1)")
	if res := execMust(t, e, "SELECT state FROM child WHERE id = 1"); res.Rows[0][0] != "new" {
		t.Fatalf("default state = %v, want new", res.Rows[0][0])
	}
	execMust(t, e, "UPDATE child SET state = 'done' WHERE id = 1")

	_, err := execSQL(e, "INSERT INTO child (id, pa, pb) VALUES (2, 'x', 2)")
	requireForeignKeyViolation(t, err)
	_, err = execSQL(e, "INSERT INTO child (id, pa, pb, state) VALUES (2, 'x', 1, 'bogus')")
	requireCheckViolation(t, err, "")
	_, err = execSQL(e, "INSERT INTO child (id, pa, pb) VALUES (1, 'x', 1)")
	if err == nil {
		t.Fatal("duplicate composite parent key or primary key was accepted")
	}
}

// A parent column covered by a UNIQUE index (not the primary key) is a valid
// foreign-key target, whether the uniqueness comes from a constraint or an
// explicit CREATE UNIQUE INDEX.
func TestForeignKeyReferencesUniqueIndex(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE by_constraint (id INTEGER PRIMARY KEY, code TEXT UNIQUE)")
	execMust(t, e, "CREATE TABLE by_index (id INTEGER PRIMARY KEY, code TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX idx_by_index_code ON by_index (code)")
	execMust(t, e, "CREATE TABLE child_constraint (id INTEGER PRIMARY KEY, code TEXT REFERENCES by_constraint(code))")
	execMust(t, e, "CREATE TABLE child_index (id INTEGER PRIMARY KEY, code TEXT REFERENCES by_index(code))")

	execMust(t, e, "INSERT INTO by_constraint (id, code) VALUES (1, 'a')")
	execMust(t, e, "INSERT INTO by_index (id, code) VALUES (1, 'b')")
	execMust(t, e, "INSERT INTO child_constraint (id, code) VALUES (10, 'a')")
	execMust(t, e, "INSERT INTO child_index (id, code) VALUES (10, 'b')")

	_, err := execSQL(e, "INSERT INTO child_constraint (id, code) VALUES (11, 'missing')")
	requireForeignKeyViolation(t, err)
	_, err = execSQL(e, "INSERT INTO child_index (id, code) VALUES (11, 'missing')")
	requireForeignKeyViolation(t, err)

	_, err = execSQL(e, "DELETE FROM by_constraint WHERE code = 'a'")
	requireForeignKeyViolation(t, err)
	_, err = execSQL(e, "DELETE FROM by_index WHERE code = 'b'")
	requireForeignKeyViolation(t, err)
}

// A scan-based parent UPDATE of a referenced column is validated at the
// statement boundary just like a point update.
func TestForeignKeyParentScanUpdate(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY, v INTEGER)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id, v) VALUES (1, 1), (2, 1)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")

	_, err := execSQL(e, "UPDATE parent SET id = id + 10 WHERE v = 1")
	requireForeignKeyViolation(t, err)
	if res := execMust(t, e, "SELECT count(*) FROM parent WHERE id > 10"); res.Rows[0][0] != int64(0) {
		t.Fatalf("rejected scan update moved rows: %v", res.Rows)
	}

	execMust(t, e, "DELETE FROM child WHERE id = 10")
	execMust(t, e, "UPDATE parent SET id = id + 10 WHERE v = 1")
	if res := execMust(t, e, "SELECT count(*) FROM parent WHERE id > 10"); res.Rows[0][0] != int64(2) {
		t.Fatalf("valid scan update rows = %v, want 2", res.Rows)
	}
}

// INSERT ... SELECT goes through the same enforcement as a VALUES insert.
func TestForeignKeyInsertSelect(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE staging (id INTEGER PRIMARY KEY, parent_id INTEGER)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO staging (id, parent_id) VALUES (10, 1), (11, 2)")

	_, err := execSQL(e, "INSERT INTO child (id, parent_id) SELECT id, parent_id FROM staging")
	requireForeignKeyViolation(t, err)
	if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(0) {
		t.Fatalf("child rows after rejected INSERT ... SELECT = %v, want 0", res.Rows)
	}

	execMust(t, e, "DELETE FROM staging WHERE id = 11")
	execMust(t, e, "INSERT INTO child (id, parent_id) SELECT id, parent_id FROM staging")
	if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(1) {
		t.Fatalf("child rows after valid INSERT ... SELECT = %v, want 1", res.Rows)
	}
}

// Referential integrity is a final-state property: a parent and its child can be
// inserted in one transaction, and the commit validates both.
func TestForeignKeyTransactionInsertParentAndChild(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")

	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")
	execMust(t, e, "COMMIT")

	if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(1) {
		t.Fatalf("child rows = %v, want 1", res.Rows)
	}
}

// Rows of one multi-row self-referential INSERT may reference each other: the
// statement is validated against its own final row set.
func TestForeignKeySiblingReferencesInOneInsert(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE nodes (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES nodes(id))")

	execMust(t, e, "INSERT INTO nodes (id, parent_id) VALUES (1, 2), (2, 1)")
	if res := execMust(t, e, "SELECT count(*) FROM nodes"); res.Rows[0][0] != int64(2) {
		t.Fatalf("nodes = %v, want 2", res.Rows)
	}

	_, err := execSQL(e, "INSERT INTO nodes (id, parent_id) VALUES (3, 4), (4, 99)")
	requireForeignKeyViolation(t, err)
	if res := execMust(t, e, "SELECT count(*) FROM nodes"); res.Rows[0][0] != int64(2) {
		t.Fatalf("rejected sibling insert left rows behind: %v", res.Rows)
	}
}

// INSERT OR REPLACE rebuilds the conflicting row inside one implicit
// transaction, so replacing a referenced parent row with the same key is valid
// while a replacement that moves the key orphans children and fails at commit.
func TestForeignKeyInsertOrReplaceOnReferencedParent(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY, code TEXT UNIQUE)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id, code) VALUES (1, 'a')")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")

	// Same key: the child keeps a valid parent, so the replacement commits.
	execMust(t, e, "INSERT OR REPLACE INTO parent (id, code) VALUES (1, 'b')")
	if res := execMust(t, e, "SELECT count(*) FROM child WHERE parent_id = 1"); res.Rows[0][0] != int64(1) {
		t.Fatalf("child rows after in-place replace = %v, want 1", res.Rows)
	}

	// A unique-code conflict replaces row 1 with row 2, orphaning the child.
	_, err := execSQL(e, "INSERT OR REPLACE INTO parent (id, code) VALUES (2, 'b')")
	requireForeignKeyViolation(t, err)
	if res := execMust(t, e, "SELECT id, code FROM parent"); res.RowCount != 1 || res.Rows[0][0] != int64(1) || res.Rows[0][1] != "b" {
		t.Fatalf("parent after rejected replace = %v, want the original row", res.Rows)
	}
	if res := execMust(t, e, "SELECT count(*) FROM child"); res.Rows[0][0] != int64(1) {
		t.Fatalf("child rows after rejected replace = %v, want 1", res.Rows)
	}
}

// A UNIQUE parent index is matched as a column set, and the values are matched in
// declaration order: FOREIGN KEY (a, b) REFERENCES parent(y, x) compares a to y
// and b to x.
func TestForeignKeyReorderedUniqueTarget(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (x INTEGER, y INTEGER, UNIQUE (x, y))")
	execMust(t, e, "CREATE TABLE child (a INTEGER, b INTEGER, FOREIGN KEY (a, b) REFERENCES parent (y, x))")
	execMust(t, e, "INSERT INTO parent (x, y) VALUES (1, 2)")

	execMust(t, e, "INSERT INTO child (a, b) VALUES (2, 1)")
	_, err := execSQL(e, "INSERT INTO child (a, b) VALUES (1, 2)")
	requireForeignKeyViolation(t, err)
	_, err = execSQL(e, "DELETE FROM parent")
	requireForeignKeyViolation(t, err)
}

// ALTER TABLE rejects changes that would strand a persisted foreign key. The
// metadata stores table and column names verbatim, so it cannot be rewritten.
func TestForeignKeyAlterTableRejections(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE u (code TEXT UNIQUE, note TEXT)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, code TEXT REFERENCES u(code), memo TEXT)")

	rejections := []struct {
		name string
		sql  string
		want string
	}{
		{"drop child column", "ALTER TABLE child DROP COLUMN code", "cannot drop column code"},
		{"rename child column", "ALTER TABLE child RENAME COLUMN code TO parent_code", "cannot rename column code"},
		{"drop parent column", "ALTER TABLE u DROP COLUMN code", "cannot drop column code"},
		{"rename parent column", "ALTER TABLE u RENAME COLUMN code TO code2", "cannot rename column code"},
		{"rename referenced table", "ALTER TABLE u RENAME TO u2", "cannot rename table u"},
		{"drop referenced table", "DROP TABLE u", "cannot drop table u"},
	}
	for _, tt := range rejections {
		t.Run(tt.name, func(t *testing.T) {
			requireUnsupported(t, execErr(t, e, tt.sql), tt.want)
		})
	}

	// Unrelated columns and the child table itself are not constrained.
	execMust(t, e, "ALTER TABLE child DROP COLUMN memo")
	execMust(t, e, "ALTER TABLE u RENAME COLUMN note TO comment")
	execMust(t, e, "ALTER TABLE u ADD COLUMN extra TEXT")
	execMust(t, e, "ALTER TABLE child RENAME TO child2")

	execMust(t, e, "INSERT INTO u (code, comment) VALUES ('a', 'x')")
	execMust(t, e, "INSERT INTO child2 (id, code) VALUES (1, 'a')")
	_, err := execSQL(e, "INSERT INTO child2 (id, code) VALUES (2, 'missing')")
	requireForeignKeyViolation(t, err)
}

// A self-referential table is both sides of its own constraint, so renaming or
// dropping it is rejected for the same reason.
func TestForeignKeySelfReferenceAlterRejections(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE nodes (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES nodes(id), memo TEXT)")

	requireUnsupported(t, execErr(t, e, "ALTER TABLE nodes DROP COLUMN parent_id"), "cannot drop column parent_id")
	requireUnsupported(t, execErr(t, e, "ALTER TABLE nodes RENAME COLUMN parent_id TO parent"), "cannot rename column parent_id")
	requireUnsupported(t, execErr(t, e, "ALTER TABLE nodes RENAME TO n2"), "cannot rename table nodes")
	requireUnsupported(t, execErr(t, e, "DROP TABLE nodes"), "cannot drop table nodes")

	execMust(t, e, "ALTER TABLE nodes DROP COLUMN memo")
	execMust(t, e, "INSERT INTO nodes (id, parent_id) VALUES (1, 1)")
	_, err := execSQL(e, "INSERT INTO nodes (id, parent_id) VALUES (2, 9)")
	requireForeignKeyViolation(t, err)
}

// execErr parses and executes SQL, returning the error (nil on success).
func execErr(t *testing.T, e *Executor, sql string) error {
	t.Helper()
	_, err := execSQL(e, sql)
	return err
}
