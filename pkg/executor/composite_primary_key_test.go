package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

func TestCreateTablePrimaryKeyConstraints(t *testing.T) {
	tests := []struct {
		name            string
		sql             string
		wantErr         string
		wantErrSQLState bool
		wantPrimaryKey  string
		wantKeyColumns  []string
	}{
		{
			name:           "composite table-level primary key is accepted",
			sql:            "CREATE TABLE t (a TEXT, b TEXT, PRIMARY KEY (a, b))",
			wantPrimaryKey: "",
			wantKeyColumns: []string{"a", "b"},
		},
		{
			name:           "three-column table-level primary key is accepted in declaration order",
			sql:            "CREATE TABLE t (a TEXT, b TEXT, c TEXT, PRIMARY KEY (c, a, b))",
			wantPrimaryKey: "",
			wantKeyColumns: []string{"c", "a", "b"},
		},
		{
			name:           "single-column table-level primary key is accepted",
			sql:            "CREATE TABLE t (a TEXT, PRIMARY KEY (a))",
			wantPrimaryKey: "a",
			wantKeyColumns: []string{"a"},
		},
		{
			name:           "inline primary key is accepted",
			sql:            "CREATE TABLE t (a TEXT PRIMARY KEY)",
			wantPrimaryKey: "a",
			wantKeyColumns: []string{"a"},
		},
		{
			name:            "two inline primary keys are rejected",
			sql:             "CREATE TABLE t (a TEXT PRIMARY KEY, b TEXT PRIMARY KEY)",
			wantErr:         "more than one inline PRIMARY KEY",
			wantErrSQLState: true,
		},
		{
			name:            "inline and table-level primary keys are rejected together",
			sql:             "CREATE TABLE t (a TEXT PRIMARY KEY, b TEXT, PRIMARY KEY (b))",
			wantErr:         "mixes an inline PRIMARY KEY with a table-level PRIMARY KEY",
			wantErrSQLState: true,
		},
		{
			name:            "duplicate column in a composite primary key is rejected",
			sql:             "CREATE TABLE t (a TEXT, PRIMARY KEY (a, a))",
			wantErr:         "appears more than once in the PRIMARY KEY",
			wantErrSQLState: true,
		},
		{
			name:            "unknown primary key column is rejected",
			sql:             "CREATE TABLE t (a TEXT, PRIMARY KEY (a, missing))",
			wantErr:         `PRIMARY KEY column "missing" does not exist`,
			wantErrSQLState: true,
		},
		{
			name:            "two table-level primary keys are rejected",
			sql:             "CREATE TABLE t (a TEXT, b TEXT, PRIMARY KEY (a), PRIMARY KEY (b))",
			wantErr:         "declares more than one PRIMARY KEY",
			wantErrSQLState: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, schema, table := newTestDB(t)
			e := newExec(schema, table)

			_, err := execSQL(e, tt.sql)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected %q to fail", tt.sql)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				if tt.wantErrSQLState && !errors.Is(err, pgerror.ErrFeatureNotSupported) {
					t.Fatalf("error = %v, want SQLSTATE 0A000 (feature not supported)", err)
				}
				if schema.TableExists("t") {
					t.Fatal("rejected CREATE TABLE left a schema behind")
				}
				if _, err := execSQL(e, "SELECT * FROM t"); err == nil {
					t.Fatal("rejected CREATE TABLE left a usable table behind")
				}
				return
			}

			if err != nil {
				t.Fatalf("exec %q: %v", tt.sql, err)
			}
			got, err := schema.GetSchema("t")
			if err != nil {
				t.Fatalf("get schema: %v", err)
			}
			if got.PrimaryKey != tt.wantPrimaryKey {
				t.Fatalf("PrimaryKey = %q, want %q", got.PrimaryKey, tt.wantPrimaryKey)
			}
			if strings.Join(got.PrimaryKeyList(), ",") != strings.Join(tt.wantKeyColumns, ",") {
				t.Fatalf("PrimaryKeyList = %v, want %v", got.PrimaryKeyList(), tt.wantKeyColumns)
			}
			for _, col := range got.Columns {
				if got.IsPrimaryKeyColumn(col.Name) && col.Nullable {
					t.Fatalf("primary key column %s is nullable", col.Name)
				}
			}
		})
	}
}

func TestCompositePrimaryKeyDML(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE pairs (a TEXT, b INTEGER, v TEXT, PRIMARY KEY (a, b))")

	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('x', 1, 'one')")
	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('x', 2, 'two')")
	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('y', 1, 'other')")

	// A full-key predicate resolves exactly one row.
	res := execMust(t, e, "SELECT v FROM pairs WHERE a = 'x' AND b = 1")
	if res.RowCount != 1 || res.Rows[0][0] != "one" {
		t.Fatalf("full-key select = %#v, want one row 'one'", res.Rows)
	}

	// A single key column is only a prefix: it must not be treated as a point
	// lookup that returns whichever row was stored first.
	res = execMust(t, e, "SELECT b FROM pairs WHERE a = 'x' ORDER BY b")
	if res.RowCount != 2 {
		t.Fatalf("prefix select returned %d rows, want 2", res.RowCount)
	}

	// Duplicate full key.
	if _, err := execSQL(e, "INSERT INTO pairs (a, b, v) VALUES ('x', 1, 'dup')"); err == nil {
		t.Fatal("expected a duplicate composite key to fail")
	} else if !isConflictError(err) {
		t.Fatalf("duplicate error = %v, want a primary-key conflict", err)
	}

	// Missing component: NOT NULL violation, never a rowid auto-assign.
	_, err := execSQL(e, "INSERT INTO pairs (a, v) VALUES ('z', 'no b')")
	var notNull *storage.NotNullViolationError
	if !errors.As(err, &notNull) || notNull.Column != "b" {
		t.Fatalf("missing component error = %v, want a NOT NULL violation on b", err)
	}
	_, err = execSQL(e, "INSERT INTO pairs (a, b, v) VALUES ('z', NULL, 'null b')")
	if !errors.As(err, &notNull) || notNull.Column != "b" {
		t.Fatalf("NULL component error = %v, want a NOT NULL violation on b", err)
	}
	if res := execMust(t, e, "SELECT count(*) FROM pairs"); res.Rows[0][0] != int64(3) {
		t.Fatalf("rejected inserts stored rows: count = %v", res.Rows[0][0])
	}

	// Update by full key.
	res = execMust(t, e, "UPDATE pairs SET v = 'ONE' WHERE a = 'x' AND b = 1")
	if res.RowsAffected != 1 {
		t.Fatalf("update affected %d rows, want 1", res.RowsAffected)
	}
	if res = execMust(t, e, "SELECT v FROM pairs WHERE a = 'x' AND b = 1"); res.RowCount != 1 || res.Rows[0][0] != "ONE" {
		t.Fatalf("row after update = %#v, want 'ONE'", res.Rows)
	}

	// Moving one key component moves the durable row.
	execMust(t, e, "UPDATE pairs SET b = 3 WHERE a = 'x' AND b = 1")
	if res = execMust(t, e, "SELECT v FROM pairs WHERE a = 'x' AND b = 3"); res.RowCount != 1 || res.Rows[0][0] != "ONE" {
		t.Fatalf("row under the new key = %#v, want 'ONE'", res.Rows)
	}
	if res = execMust(t, e, "SELECT v FROM pairs WHERE a = 'x' AND b = 1"); res.RowCount != 0 {
		t.Fatalf("old key still resolves %d rows", res.RowCount)
	}

	// Moving onto an existing key is a duplicate.
	if _, err := execSQL(e, "UPDATE pairs SET b = 2 WHERE a = 'x' AND b = 3"); err == nil {
		t.Fatal("expected a key move onto an existing row to fail")
	} else if !isConflictError(err) {
		t.Fatalf("key move error = %v, want a primary-key conflict", err)
	}

	// Clearing a key component is a NOT NULL violation, not a key move.
	_, err = execSQL(e, "UPDATE pairs SET b = NULL WHERE a = 'x' AND b = 2")
	if !errors.As(err, &notNull) || notNull.Column != "b" {
		t.Fatalf("NULL key update error = %v, want a NOT NULL violation on b", err)
	}

	// Delete by full key removes exactly one row.
	res = execMust(t, e, "DELETE FROM pairs WHERE a = 'x' AND b = 2")
	if res.RowsAffected != 1 {
		t.Fatalf("delete affected %d rows, want 1", res.RowsAffected)
	}
	if res = execMust(t, e, "SELECT count(*) FROM pairs"); res.Rows[0][0] != int64(2) {
		t.Fatalf("row count after delete = %v, want 2", res.Rows[0][0])
	}
}

func TestCompositePrimaryKeyOnConflict(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE counters (a TEXT, b TEXT, n INTEGER, PRIMARY KEY (a, b))")
	execMust(t, e, "INSERT INTO counters (a, b, n) VALUES ('x', 'y', 1)")

	execMust(t, e, "INSERT INTO counters (a, b, n) VALUES ('x', 'y', 2) ON CONFLICT (a, b) DO UPDATE SET n = excluded.n")
	if res := execMust(t, e, "SELECT n FROM counters WHERE a = 'x' AND b = 'y'"); res.RowCount != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("upsert result = %#v, want one row with n = 2", res.Rows)
	}

	execMust(t, e, "INSERT INTO counters (a, b, n) VALUES ('x', 'y', 9) ON CONFLICT (a, b) DO NOTHING")
	if res := execMust(t, e, "SELECT n FROM counters WHERE a = 'x' AND b = 'y'"); res.RowCount != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("DO NOTHING result = %#v, want the existing row with n = 2", res.Rows)
	}

	// A targetless conflict clause uses the full primary-key column list.
	execMust(t, e, "INSERT INTO counters (a, b, n) VALUES ('x', 'y', 9) ON CONFLICT DO NOTHING")
	if res := execMust(t, e, "SELECT count(*) FROM counters"); res.Rows[0][0] != int64(1) {
		t.Fatalf("targetless DO NOTHING inserted a row: count = %v", res.Rows[0][0])
	}

	// REPLACE resolves the conflict on the full key.
	execMust(t, e, "INSERT OR REPLACE INTO counters (a, b, n) VALUES ('x', 'y', 5)")
	if res := execMust(t, e, "SELECT n FROM counters WHERE a = 'x' AND b = 'y'"); res.RowCount != 1 || res.Rows[0][0] != int64(5) {
		t.Fatalf("REPLACE result = %#v, want one row with n = 5", res.Rows)
	}

	// A different key is not a conflict.
	execMust(t, e, "INSERT INTO counters (a, b, n) VALUES ('x', 'z', 7) ON CONFLICT (a, b) DO NOTHING")
	if res := execMust(t, e, "SELECT n FROM counters WHERE a = 'x' AND b = 'z'"); res.RowCount != 1 || res.Rows[0][0] != int64(7) {
		t.Fatalf("new key result = %#v, want one row with n = 7", res.Rows)
	}

	// ON CONFLICT UPDATE on a full key with the columns in the other order is a
	// distinct target and must not match the declared key.
	execMust(t, e, "INSERT INTO counters (a, b, n) VALUES ('q', 'r', 1) ON CONFLICT (b, a) DO UPDATE SET n = excluded.n")
	if res := execMust(t, e, "SELECT count(*) FROM counters WHERE a = 'q' AND b = 'r'"); res.Rows[0][0] != int64(1) {
		t.Fatalf("reordered conflict target rows = %v, want 1", res.Rows[0][0])
	}
}

func TestCompositePrimaryKeyCatalog(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE pairs (a TEXT, b INTEGER, v TEXT, PRIMARY KEY (a, b))")

	res := execMust(t, e, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'pairs'")
	if res.RowCount != 1 {
		t.Fatalf("sqlite_master rows = %d, want 1", res.RowCount)
	}
	createSQL, _ := res.Rows[0][0].(string)
	if !strings.Contains(createSQL, "PRIMARY KEY (`a`, `b`)") {
		t.Fatalf("sqlite_master SQL = %q, want a table-level PRIMARY KEY (a, b)", createSQL)
	}
	if strings.Count(createSQL, "PRIMARY KEY") != 1 {
		t.Fatalf("sqlite_master SQL = %q, want exactly one PRIMARY KEY clause", createSQL)
	}

	res = execMust(t, e, "PRAGMA table_info(pairs)")
	if res.RowCount != 3 {
		t.Fatalf("table_info rows = %d, want 3", res.RowCount)
	}
	wantPK := []int64{1, 2, 0}
	for i, row := range res.Rows {
		if row[1] != []string{"a", "b", "v"}[i] {
			t.Fatalf("table_info row %d column = %v", i, row[1])
		}
		if row[5] != wantPK[i] {
			t.Fatalf("table_info pk ordinal for %v = %v, want %d", row[1], row[5], wantPK[i])
		}
	}
	// Both key columns are implicitly NOT NULL.
	if res.Rows[0][3] != int64(1) || res.Rows[1][3] != int64(1) {
		t.Fatalf("table_info notnull = %v/%v, want 1/1", res.Rows[0][3], res.Rows[1][3])
	}

	res = execMust(t, e, "PRAGMA table_xinfo(pairs)")
	if res.RowCount != 3 || res.Rows[1][5] != int64(2) {
		t.Fatalf("table_xinfo = %#v, want pk ordinal 2 on the second key column", res.Rows)
	}
}

func TestCompositePrimaryKeyDurableReload(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE pairs (a TEXT, b TEXT, v TEXT, PRIMARY KEY (a, b))")
	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('x', 'y', 'one')")
	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('x', 'z', 'two')")

	// Simulate a process restart: new managers share only the durable KV state.
	reloadedSchema := storage.NewSchemaManager(pool, "testdb")
	reloadedTable := storage.NewTableManager(pool, reloadedSchema, "testdb")
	reloaded := newExec(reloadedSchema, reloadedTable)

	got, err := reloadedSchema.GetSchema("pairs")
	if err != nil {
		t.Fatalf("get reloaded schema: %v", err)
	}
	if got.PrimaryKey != "" || strings.Join(got.PrimaryKeyList(), ",") != "a,b" {
		t.Fatalf("reloaded key = %q/%v, want composite a,b", got.PrimaryKey, got.PrimaryKeyList())
	}

	res := execMust(t, reloaded, "SELECT v FROM pairs WHERE a = 'x' AND b = 'y'")
	if res.RowCount != 1 || res.Rows[0][0] != "one" {
		t.Fatalf("reloaded full-key read = %#v, want one row 'one'", res.Rows)
	}
	if res = execMust(t, reloaded, "SELECT count(*) FROM pairs"); res.Rows[0][0] != int64(2) {
		t.Fatalf("reloaded count = %v, want 2", res.Rows[0][0])
	}

	execMust(t, reloaded, "UPDATE pairs SET v = 'updated' WHERE a = 'x' AND b = 'z'")
	if res = execMust(t, reloaded, "SELECT v FROM pairs WHERE a = 'x' AND b = 'z'"); res.RowCount != 1 || res.Rows[0][0] != "updated" {
		t.Fatalf("reloaded update = %#v, want one row 'updated'", res.Rows)
	}
}

func TestCompositePrimaryKeyRejectsDuplicateInsert(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE pairs (a TEXT, b TEXT, PRIMARY KEY (a, b))")
	execMust(t, e, "INSERT INTO pairs (a, b) VALUES ('x', 'y')")
	if _, err := execSQL(e, "INSERT INTO pairs (a, b) VALUES ('x', 'y')"); err == nil {
		t.Fatal("expected a duplicate composite key to fail")
	} else if !isConflictError(err) {
		t.Fatalf("duplicate error = %v, want a primary-key conflict", err)
	}
	execMust(t, e, "INSERT INTO pairs (a, b) VALUES ('x', 'z')")
	execMust(t, e, "INSERT INTO pairs (a, b) VALUES ('y', 'y')")
	if res := execMust(t, e, "SELECT count(*) FROM pairs"); res.Rows[0][0] != int64(3) {
		t.Fatalf("row count = %v, want 3", res.Rows[0][0])
	}
}

func TestIntegerPrimaryKeyRemainsRowIDAlias(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO t (v) VALUES ('auto')")
	res := execMust(t, e, "SELECT id, rowid FROM t")
	if res.RowCount != 1 || res.Rows[0][0] != int64(1) || res.Rows[0][1] != int64(1) {
		t.Fatalf("INTEGER PRIMARY KEY row = %#v, want id and rowid 1", res.Rows)
	}

	// A composite key is never a rowid alias: the key columns keep the supplied
	// values and the hidden rowid is engine-assigned.
	execMust(t, e, "CREATE TABLE pairs (a INTEGER, b TEXT, PRIMARY KEY (a, b))")
	execMust(t, e, "INSERT INTO pairs (a, b) VALUES (7, 'x')")
	res = execMust(t, e, "SELECT a, b, rowid FROM pairs")
	if res.RowCount != 1 || res.Rows[0][0] != int64(7) || res.Rows[0][2] != int64(1) {
		t.Fatalf("composite key row = %#v, want a=7 b='x' rowid=1", res.Rows)
	}
}

func TestCompositePrimaryKeyInsertSelect(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE src (a TEXT, b TEXT)")
	execMust(t, e, "INSERT INTO src (a, b) VALUES ('x', 'y'), ('x', 'z'), ('q', 'r')")
	execMust(t, e, "CREATE TABLE pairs (a TEXT, b TEXT, v TEXT, PRIMARY KEY (a, b))")

	execMust(t, e, "INSERT INTO pairs (a, b, v) SELECT a, b, 'copied' FROM src")
	if res := execMust(t, e, "SELECT count(*) FROM pairs"); res.Rows[0][0] != int64(3) {
		t.Fatalf("bulk insert stored %v rows, want 3", res.Rows[0][0])
	}
	if res := execMust(t, e, "SELECT v FROM pairs WHERE a = 'x' AND b = 'z'"); res.RowCount != 1 || res.Rows[0][0] != "copied" {
		t.Fatalf("bulk row = %#v, want one row 'copied'", res.Rows)
	}

	// A bulk insert that repeats a key fails atomically.
	if _, err := execSQL(e, "INSERT INTO pairs (a, b, v) SELECT a, b, 'again' FROM src"); err == nil {
		t.Fatal("expected a duplicate-key bulk insert to fail")
	} else if !isConflictError(err) {
		t.Fatalf("bulk duplicate error = %v, want a key conflict", err)
	}
	if res := execMust(t, e, "SELECT count(*) FROM pairs"); res.Rows[0][0] != int64(3) {
		t.Fatalf("failed bulk insert changed the row count: %v", res.Rows[0][0])
	}
}

func TestCompositePrimaryKeyAlterTable(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE pairs (a TEXT, b TEXT, v TEXT, PRIMARY KEY (a, b))")
	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('x', 'y', 'one')")

	// Renaming a key column keeps the key list and the durable row keys.
	execMust(t, e, "ALTER TABLE pairs RENAME COLUMN a TO k")
	got, err := schema.GetSchema("pairs")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if strings.Join(got.PrimaryKeyList(), ",") != "k,b" {
		t.Fatalf("key list after rename = %v, want [k b]", got.PrimaryKeyList())
	}
	if res := execMust(t, e, "SELECT v FROM pairs WHERE k = 'x' AND b = 'y'"); res.RowCount != 1 || res.Rows[0][0] != "one" {
		t.Fatalf("read after rename = %#v, want one row 'one'", res.Rows)
	}

	// A key column cannot be dropped, so the key arity is immutable.
	if _, err := execSQL(e, "ALTER TABLE pairs DROP COLUMN b"); err == nil {
		t.Fatal("expected DROP COLUMN of a key column to fail")
	} else if !strings.Contains(err.Error(), "cannot drop primary key column") {
		t.Fatalf("DROP COLUMN error = %v, want a primary-key rejection", err)
	}
}

// An upsert updates the conflicting row in place, so it cannot move that row to
// a different key. Accepting the assignment would leave the durable row under
// the old key while its body carries the new key values, and a later full-key
// update would create a second row.
func TestUpsertRejectsPrimaryKeyAssignment(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE pairs (a TEXT, b TEXT, v TEXT, PRIMARY KEY (a, b))")
	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('x', 'y', 'one')")

	for _, sql := range []string{
		"INSERT INTO pairs (a, b, v) VALUES ('x', 'y', 'two') ON CONFLICT (a, b) DO UPDATE SET v = excluded.v, a = 'moved'",
		"INSERT INTO pairs (a, b, v) VALUES ('new', 'key', 'v') ON CONFLICT (a, b) DO UPDATE SET b = excluded.b",
	} {
		_, err := execSQL(e, sql)
		if err == nil {
			t.Fatalf("%q succeeded, want a primary-key rejection", sql)
		}
		if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Fatalf("%q error = %v, want SQLSTATE 0A000", sql, err)
		}
		if !strings.Contains(err.Error(), "cannot assign primary key column") {
			t.Fatalf("%q error = %v, want a primary-key assignment message", sql, err)
		}
	}

	// No ghost row: the original row kept both its key and its value.
	if res := execMust(t, e, "SELECT a, b, v FROM pairs"); res.RowCount != 1 || res.Rows[0][0] != "x" || res.Rows[0][1] != "y" || res.Rows[0][2] != "one" {
		t.Fatalf("rows after rejected upserts = %#v, want the original ('x','y','one')", res.Rows)
	}
	if res := execMust(t, e, "SELECT v FROM pairs WHERE a = 'x' AND b = 'y'"); res.RowCount != 1 || res.Rows[0][0] != "one" {
		t.Fatalf("full-key read after rejected upserts = %#v", res.Rows)
	}
	execMust(t, e, "UPDATE pairs SET v = 'updated' WHERE a = 'x' AND b = 'y'")
	if res := execMust(t, e, "SELECT count(*) FROM pairs"); res.Rows[0][0] != int64(1) {
		t.Fatalf("row count after full-key update = %v, want 1 (no split row)", res.Rows[0][0])
	}

	// A single-column key gets the same rejection.
	execMust(t, e, "CREATE TABLE single (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO single (v) VALUES ('one')")
	if _, err := execSQL(e, "INSERT INTO single (id, v) VALUES (1, 'two') ON CONFLICT (id) DO UPDATE SET id = excluded.id"); err == nil {
		t.Fatal("single-column upsert key assignment succeeded, want a rejection")
	} else if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
		t.Fatalf("single-column upsert error = %v, want SQLSTATE 0A000", err)
	}
}

// A key column that takes its value from a constant DEFAULT must be resolved
// before conflict matching, otherwise the candidate key is encoded from a
// missing component and the upsert misses its own row.
func TestUpsertWithConstantDefaultKeyColumns(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE konst (a TEXT DEFAULT 'd', b TEXT DEFAULT 'e', v TEXT, PRIMARY KEY (a, b))")

	execMust(t, e, "INSERT INTO konst (b, v) VALUES ('y', 'two')")
	if res := execMust(t, e, "SELECT a, b, v FROM konst"); res.RowCount != 1 || res.Rows[0][0] != "d" || res.Rows[0][1] != "y" {
		t.Fatalf("defaulted key row = %#v, want ('d','y')", res.Rows)
	}

	// DO UPDATE hits the row whose key came from the DEFAULT.
	execMust(t, e, "INSERT INTO konst (b, v) VALUES ('y', 'updated') ON CONFLICT (a, b) DO UPDATE SET v = excluded.v")
	if res := execMust(t, e, "SELECT v FROM konst WHERE a = 'd' AND b = 'y'"); res.RowCount != 1 || res.Rows[0][0] != "updated" {
		t.Fatalf("DO UPDATE result = %#v, want one row 'updated'", res.Rows)
	}
	// DO NOTHING suppresses the duplicate.
	execMust(t, e, "INSERT INTO konst (b, v) VALUES ('y', 'ignored') ON CONFLICT (a, b) DO NOTHING")
	if res := execMust(t, e, "SELECT v FROM konst WHERE a = 'd' AND b = 'y'"); res.RowCount != 1 || res.Rows[0][0] != "updated" {
		t.Fatalf("DO NOTHING result = %#v, want the existing 'updated' row", res.Rows)
	}
	// Targetless ON CONFLICT uses the defaulted key too.
	execMust(t, e, "INSERT INTO konst (b, v) VALUES ('y', 'ignored') ON CONFLICT DO NOTHING")
	if res := execMust(t, e, "SELECT count(*) FROM konst"); res.Rows[0][0] != int64(1) {
		t.Fatalf("targetless DO NOTHING inserted a row: count = %v", res.Rows[0][0])
	}
	// INSERT OR REPLACE replaces the defaulted-key row instead of duplicating it.
	execMust(t, e, "INSERT OR REPLACE INTO konst (b, v) VALUES ('y', 'replaced')")
	if res := execMust(t, e, "SELECT count(*) FROM konst"); res.Rows[0][0] != int64(1) {
		t.Fatalf("OR REPLACE left %v rows, want 1", res.Rows[0][0])
	}
	if res := execMust(t, e, "SELECT v FROM konst WHERE a = 'd' AND b = 'y'"); res.RowCount != 1 || res.Rows[0][0] != "replaced" {
		t.Fatalf("OR REPLACE result = %#v, want one row 'replaced'", res.Rows)
	}

	// A single-column TEXT key with a DEFAULT follows the same path and must not
	// fall back to an engine-assigned rowid key.
	execMust(t, e, "CREATE TABLE single (id TEXT DEFAULT 'k', v TEXT, PRIMARY KEY (id))")
	execMust(t, e, "INSERT INTO single (v) VALUES ('a')")
	if res := execMust(t, e, "SELECT id FROM single"); res.RowCount != 1 || res.Rows[0][0] != "k" {
		t.Fatalf("single defaulted key = %#v, want one row with id 'k'", res.Rows)
	}
	execMust(t, e, "INSERT INTO single (v) VALUES ('b') ON CONFLICT (id) DO UPDATE SET v = excluded.v")
	if res := execMust(t, e, "SELECT v FROM single"); res.RowCount != 1 || res.Rows[0][0] != "b" {
		t.Fatalf("single DO UPDATE = %#v, want one row 'b'", res.Rows)
	}
	execMust(t, e, "INSERT INTO single (v) VALUES ('c') ON CONFLICT DO NOTHING")
	if res := execMust(t, e, "SELECT count(*) FROM single"); res.Rows[0][0] != int64(1) {
		t.Fatalf("single targetless DO NOTHING count = %v, want 1", res.Rows[0][0])
	}
	execMust(t, e, "INSERT OR REPLACE INTO single (v) VALUES ('d')")
	if res := execMust(t, e, "SELECT v FROM single"); res.RowCount != 1 || res.Rows[0][0] != "d" {
		t.Fatalf("single OR REPLACE = %#v, want one row 'd'", res.Rows)
	}
}

func TestAlterTableAddColumnPrimaryKeyRejected(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (a TEXT)")
	execMust(t, e, "INSERT INTO t (a) VALUES ('x')")

	_, err := execSQL(e, "ALTER TABLE t ADD COLUMN id INTEGER PRIMARY KEY")
	if err == nil {
		t.Fatal("ALTER TABLE ADD COLUMN PRIMARY KEY succeeded, want a rejection")
	}
	if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
		t.Fatalf("error = %v, want SQLSTATE 0A000", err)
	}
	if !strings.Contains(err.Error(), "cannot declare PRIMARY KEY") {
		t.Fatalf("error = %v, want an ADD COLUMN PRIMARY KEY message", err)
	}
	got, err := schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if _, exists := got.GetColumn("id"); exists {
		t.Fatal("rejected ADD COLUMN published the column")
	}
}

func TestGeneratedColumnInPrimaryKeyRejected(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE g (a TEXT, b TEXT GENERATED ALWAYS AS (a) STORED, PRIMARY KEY (b))",
		"CREATE TABLE g (a TEXT, b TEXT GENERATED ALWAYS AS (a) STORED PRIMARY KEY)",
	} {
		_, schema, table := newTestDB(t)
		e := newExec(schema, table)
		_, err := execSQL(e, sql)
		if err == nil {
			t.Fatalf("%q succeeded, want a generated-column rejection", sql)
		}
		if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Fatalf("%q error = %v, want SQLSTATE 0A000", sql, err)
		}
		if !strings.Contains(err.Error(), "cannot be part of a PRIMARY KEY") {
			t.Fatalf("%q error = %v, want a PRIMARY KEY message", sql, err)
		}
		if schema.TableExists("g") {
			t.Fatalf("%q left a schema behind", sql)
		}
	}
}

// RETURNING must observe key components that the storage layer resolved from a
// catalog DEFAULT; the candidate row does not carry them until then.
func TestCompositePrimaryKeyReturningWithDefaultedKey(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE pairs (a TEXT DEFAULT 'd', b TEXT DEFAULT 'e', v TEXT, PRIMARY KEY (a, b))")

	res := execMust(t, e, "INSERT INTO pairs (v) VALUES ('one') RETURNING a, b, v")
	if res.RowCount != 1 || res.Rows[0][0] != "d" || res.Rows[0][1] != "e" || res.Rows[0][2] != "one" {
		t.Fatalf("RETURNING = %#v, want ('d','e','one')", res.Rows)
	}

	// A partially defaulted key also returns the stored values.
	res = execMust(t, e, "INSERT INTO pairs (b, v) VALUES ('y', 'two') RETURNING *")
	if res.RowCount != 1 {
		t.Fatalf("RETURNING * rows = %d, want 1", res.RowCount)
	}
	values := map[string]interface{}{}
	for i, name := range res.Columns {
		values[name] = res.Rows[0][i]
	}
	if values["a"] != "d" || values["b"] != "y" || values["v"] != "two" {
		t.Fatalf("RETURNING * row = %#v, want a='d' b='y' v='two'", values)
	}
}

// ON CONFLICT targets address the key column set, not its declaration order:
// (b, a) and (a, b) name the same PRIMARY KEY (a, b). SQLite requires the
// constraint's order; this engine already accepts arbitrary target lists and
// documents the set match here.
func TestOnConflictReorderedPrimaryKeyTarget(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE pairs (a TEXT, b TEXT, v TEXT, PRIMARY KEY (a, b))")
	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('x', 'y', 'one')")

	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('x', 'y', 'two') ON CONFLICT (b, a) DO UPDATE SET v = excluded.v")
	res := execMust(t, e, "SELECT count(*) FROM pairs")
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("reordered target left %v rows, want 1", res.Rows[0][0])
	}
	res = execMust(t, e, "SELECT v FROM pairs WHERE a = 'x' AND b = 'y'")
	if res.RowCount != 1 || res.Rows[0][0] != "two" {
		t.Fatalf("reordered target result = %#v, want the updated row 'two'", res.Rows)
	}

	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('x', 'y', 'three') ON CONFLICT (b, a) DO NOTHING")
	res = execMust(t, e, "SELECT v FROM pairs WHERE a = 'x' AND b = 'y'")
	if res.RowCount != 1 || res.Rows[0][0] != "two" {
		t.Fatalf("reordered DO NOTHING result = %#v, want the existing 'two' row", res.Rows)
	}
}

// A composite primary key must coexist with scan-validated UNIQUE indexes:
// key moves keep index lookups correct, ON CONFLICT (unique_col) upserts the
// row, and INSERT OR REPLACE resolves both the unique index and the primary key.
func TestCompositePrimaryKeyWithUniqueIndex(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE members (org TEXT, user_id INTEGER, email TEXT UNIQUE, v TEXT, PRIMARY KEY (org, user_id))")
	execMust(t, e, "INSERT INTO members (org, user_id, email, v) VALUES ('o', 1, 'a@x', 'one')")

	// ON CONFLICT (unique_col) updates the row under its composite key.
	execMust(t, e, "INSERT INTO members (org, user_id, email, v) VALUES ('o', 1, 'a@x', 'two') ON CONFLICT (email) DO UPDATE SET v = excluded.v")
	if res := execMust(t, e, "SELECT v FROM members WHERE org = 'o' AND user_id = 1"); res.RowCount != 1 || res.Rows[0][0] != "two" {
		t.Fatalf("unique-target upsert = %#v, want one row 'two'", res.Rows)
	}

	// A unique conflict on a different key is suppressed by DO NOTHING.
	execMust(t, e, "INSERT INTO members (org, user_id, email, v) VALUES ('o', 2, 'a@x', 'duplicate email') ON CONFLICT (email) DO NOTHING")
	if res := execMust(t, e, "SELECT count(*) FROM members"); res.Rows[0][0] != int64(1) {
		t.Fatalf("unique DO NOTHING count = %v, want 1", res.Rows[0][0])
	}

	// Moving a key component keeps the unique index entry attached to the row.
	execMust(t, e, "UPDATE members SET user_id = 3 WHERE org = 'o' AND user_id = 1")
	if res := execMust(t, e, "SELECT v FROM members WHERE org = 'o' AND user_id = 3"); res.RowCount != 1 || res.Rows[0][0] != "two" {
		t.Fatalf("row after key move = %#v, want one row 'two'", res.Rows)
	}
	if res := execMust(t, e, "SELECT v FROM members WHERE email = 'a@x'"); res.RowCount != 1 || res.Rows[0][0] != "two" {
		t.Fatalf("unique index after key move = %#v, want one row 'two'", res.Rows)
	}

	// OR REPLACE resolves the unique index by removing the old row and keeping
	// exactly the new key.
	execMust(t, e, "INSERT OR REPLACE INTO members (org, user_id, email, v) VALUES ('o', 9, 'a@x', 'replaced')")
	if res := execMust(t, e, "SELECT count(*) FROM members"); res.Rows[0][0] != int64(1) {
		t.Fatalf("unique OR REPLACE count = %v, want 1", res.Rows[0][0])
	}
	if res := execMust(t, e, "SELECT v FROM members WHERE org = 'o' AND user_id = 9"); res.RowCount != 1 || res.Rows[0][0] != "replaced" {
		t.Fatalf("unique OR REPLACE row = %#v, want one row 'replaced'", res.Rows)
	}
	if res := execMust(t, e, "SELECT v FROM members WHERE org = 'o' AND user_id = 3"); res.RowCount != 0 {
		t.Fatalf("old composite key still resolves %d rows", res.RowCount)
	}

	// OR REPLACE resolving the primary key works with a unique index present.
	execMust(t, e, "INSERT OR REPLACE INTO members (org, user_id, email, v) VALUES ('o', 9, 'b@x', 'key replaced')")
	if res := execMust(t, e, "SELECT email, v FROM members WHERE org = 'o' AND user_id = 9"); res.RowCount != 1 || res.Rows[0][0] != "b@x" || res.Rows[0][1] != "key replaced" {
		t.Fatalf("PK OR REPLACE row = %#v, want ('b@x','key replaced')", res.Rows)
	}
	if res := execMust(t, e, "SELECT v FROM members WHERE email = 'a@x'"); res.RowCount != 0 {
		t.Fatalf("stale unique index entry still resolves %d rows", res.RowCount)
	}
}

// A composite table has no INTEGER PRIMARY KEY alias, so last_insert_rowid()
// reports the engine-assigned hidden rowid and advances per inserted row.
func TestLastInsertRowIDOnCompositeTable(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE pairs (a TEXT, b TEXT, PRIMARY KEY (a, b))")

	res := execMust(t, e, "INSERT INTO pairs (a, b) VALUES ('x', 'y')")
	if res.LastInsertID != 1 {
		t.Fatalf("first LastInsertID = %d, want 1", res.LastInsertID)
	}
	if res := execMust(t, e, "SELECT last_insert_rowid()"); res.RowCount != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("last_insert_rowid() = %#v, want 1", res.Rows)
	}

	res = execMust(t, e, "INSERT INTO pairs (a, b) VALUES ('x', 'z')")
	if res.LastInsertID != 2 {
		t.Fatalf("second LastInsertID = %d, want 2", res.LastInsertID)
	}
	if res := execMust(t, e, "SELECT last_insert_rowid()"); res.RowCount != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("last_insert_rowid() after second insert = %#v, want 2", res.Rows)
	}

	// A suppressed conflict does not advance last_insert_rowid().
	execMust(t, e, "INSERT INTO pairs (a, b) VALUES ('x', 'z') ON CONFLICT (a, b) DO NOTHING")
	if res := execMust(t, e, "SELECT last_insert_rowid()"); res.Rows[0][0] != int64(2) {
		t.Fatalf("last_insert_rowid() after upsert = %v, want 2", res.Rows[0][0])
	}
}
