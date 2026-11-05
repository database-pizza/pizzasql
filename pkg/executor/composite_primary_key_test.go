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
