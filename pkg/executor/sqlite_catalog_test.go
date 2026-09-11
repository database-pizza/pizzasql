package executor

import (
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

func setupCatalogFixture(t *testing.T) *Executor {
	t.Helper()
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	execMust(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, email TEXT, age INTEGER DEFAULT 0)")
	execMust(t, e, "CREATE TABLE posts (id INTEGER PRIMARY KEY, title TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX uq_users_email ON users (email)")
	execMust(t, e, "CREATE INDEX idx_users_name ON users (name)")
	return e
}

func TestSQLiteCatalogListTables(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 table rows, got %d: %v", len(res.Rows), res.Rows)
	}
	if res.Rows[0][0] != "posts" || res.Rows[1][0] != "users" {
		t.Fatalf("unexpected table order: %v", res.Rows)
	}
}

func TestSQLiteCatalogCountStar(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='users'")
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("expected count 1, got %v", res.Rows)
	}
	res = execMust(t, e, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='missing'")
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("expected count 0, got %v", res.Rows)
	}
}

func TestSQLiteCatalogTableSQL(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "SELECT sql FROM sqlite_master WHERE type='table' AND name='users'")
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
	sql, _ := res.Rows[0][0].(string)
	for _, want := range []string{
		"CREATE TABLE",
		"`users`",
		"`id` INTEGER PRIMARY KEY AUTOINCREMENT",
		"`name` TEXT NOT NULL",
		"`email` TEXT",
		"`age` INTEGER DEFAULT 0",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("CREATE TABLE sql %q missing %q", sql, want)
		}
	}
}

func TestSQLiteCatalogIndexSQL(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name='users' ORDER BY sql")
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 index rows, got %d: %v", len(res.Rows), res.Rows)
	}
	var hasUnique, hasPlain bool
	for _, r := range res.Rows {
		sql, _ := r[0].(string)
		if strings.Contains(sql, "CREATE UNIQUE INDEX `uq_users_email` ON `users` (`email`)") {
			hasUnique = true
		}
		if strings.Contains(sql, "CREATE INDEX `idx_users_name` ON `users` (`name`)") {
			hasPlain = true
		}
	}
	if !hasUnique || !hasPlain {
		t.Fatalf("missing expected index SQL: %v", res.Rows)
	}
}

func TestSQLiteCatalogSchemaAlias(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "SELECT count(*) FROM sqlite_schema WHERE type='table'")
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("sqlite_schema expected 2 tables, got %v", res.Rows)
	}
}

func TestSQLiteCatalogSelectStar(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "SELECT * FROM sqlite_master WHERE name='users' AND type='table'")
	if len(res.Columns) != 5 {
		t.Fatalf("expected 5 columns, got %v", res.Columns)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != "table" || res.Rows[0][1] != "users" || res.Rows[0][2] != "users" {
		t.Fatalf("unexpected row: %v", res.Rows)
	}
}

func TestSQLiteCatalogInAndIsNotNull(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "SELECT sql FROM sqlite_master WHERE type IN ('table','index') AND tbl_name='users' AND sql IS NOT NULL")
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(res.Rows))
	}
}

func TestSQLiteCatalogLikeColumnExist(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "SELECT name FROM sqlite_master WHERE type='table' AND name='users' AND sql LIKE '%`email`%'")
	if len(res.Rows) != 1 || res.Rows[0][0] != "users" {
		t.Fatalf("expected users row, got %v", res.Rows)
	}
	res = execMust(t, e, "SELECT name FROM sqlite_master WHERE type='table' AND name='users' AND sql LIKE '%`nope`%'")
	if len(res.Rows) != 0 {
		t.Fatalf("expected no rows, got %v", res.Rows)
	}
}

func TestSQLiteCatalogPragmaIndexList(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "PRAGMA index_list('users')")
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 indexes, got %d", len(res.Rows))
	}
	for _, row := range res.Rows {
		if row[3] != "c" {
			t.Fatalf("expected origin 'c', got %v", row[3])
		}
	}
}

func TestSQLiteCatalogPragmaIndexInfo(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "PRAGMA index_info('uq_users_email')")
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 column, got %d", len(res.Rows))
	}
	if res.Rows[0][2] != "email" {
		t.Fatalf("expected email, got %v", res.Rows[0][2])
	}
}

func TestSQLiteCatalogPragmaTableXInfo(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "PRAGMA table_xinfo('users')")
	if len(res.Columns) != 7 {
		t.Fatalf("expected 7 columns, got %v", res.Columns)
	}
	if len(res.Rows) != 4 {
		t.Fatalf("expected 4 columns, got %d", len(res.Rows))
	}
	if res.Rows[0][1] != "id" || res.Rows[0][5] != int64(1) {
		t.Fatalf("unexpected id row: %v", res.Rows[0])
	}
	if res.Rows[1][3] != int64(1) {
		t.Fatalf("expected name notnull=1, got %v", res.Rows[1])
	}
}

func TestSQLiteCatalogPragmaTableInfoStillDelegated(t *testing.T) {
	e := setupCatalogFixture(t)
	res := execMust(t, e, "PRAGMA table_info('users')")
	if len(res.Columns) != 6 {
		t.Fatalf("expected built-in table_info with 6 columns, got %v", res.Columns)
	}
}

func TestRecreateCreateTableSQLQuotesIdentifiers(t *testing.T) {
	s := &storage.Schema{
		Name: "we`ird table",
		Columns: []storage.Column{
			{Name: "col`umn", Type: "TEXT", Nullable: true},
			{Name: "has space", Type: "INTEGER", Nullable: false, PrimaryKey: true},
		},
	}
	got := recreateCreateTableSQL(s)
	want := "CREATE TABLE `we``ird table` (`col``umn` TEXT, `has space` INTEGER PRIMARY KEY NOT NULL)"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestRecreateCreateTableSQLStripsRowID(t *testing.T) {
	s := &storage.Schema{
		Name:       "t",
		PrimaryKey: "_rowid_",
		Columns: []storage.Column{
			{Name: "_rowid_", Type: "INTEGER", PrimaryKey: true},
			{Name: "v", Type: "TEXT", Nullable: true},
		},
	}
	got := recreateCreateTableSQL(s)
	if strings.Contains(got, "_rowid_") {
		t.Fatalf("implicit _rowid_ must be stripped: %q", got)
	}
	if got != "CREATE TABLE `t` (`v` TEXT)" {
		t.Fatalf("unexpected SQL: %q", got)
	}
}

func TestRecreateCreateTableSQLKeepsUserOidRowid(t *testing.T) {
	// A real Gogs LFS table has an oid TEXT column; a user table may also have
	// an explicit rowid column. Neither may be dropped from the catalog.
	s := &storage.Schema{
		Name:       "lfs_object",
		PrimaryKey: "_rowid_",
		Columns: []storage.Column{
			{Name: "oid", Type: "TEXT", Nullable: false},
			{Name: "rowid", Type: "INTEGER", Nullable: true},
		},
	}
	got := recreateCreateTableSQL(s)
	want := "CREATE TABLE `lfs_object` (`oid` TEXT NOT NULL, `rowid` INTEGER)"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestRecreateCreateTableSQLDefaults(t *testing.T) {
	s := &storage.Schema{
		Name: "t",
		Columns: []storage.Column{
			{Name: "a", Type: "TEXT", Default: "it's", Nullable: true},
			{Name: "b", Type: "INTEGER", Default: int64(7), Nullable: true},
			{Name: "c", Type: "INTEGER", Default: true, Nullable: true},
			{Name: "d", Type: "REAL", Default: 3.5, Nullable: true},
			{Name: "e", Type: "TEXT", Nullable: true},
		},
	}
	got := recreateCreateTableSQL(s)
	for _, want := range []string{
		"`a` TEXT DEFAULT 'it''s'",
		"`b` INTEGER DEFAULT 7",
		"`c` INTEGER DEFAULT 1",
		"`d` REAL DEFAULT 3.5",
		"`e` TEXT",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestRecreateCreateIndexSQL(t *testing.T) {
	idx := &storage.Index{
		Name:   "ix`a",
		Table:  "ta`ble",
		Unique: true,
		Columns: []storage.IndexColumn{
			{Name: "co`l1"},
			{Name: "col2", Desc: true},
		},
	}
	got := recreateCreateIndexSQL(idx)
	want := "CREATE UNIQUE INDEX `ix``a` ON `ta``ble` (`co``l1`, `col2` DESC)"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"a":     "`a`",
		"a`b":   "`a``b`",
		"plain": "`plain`",
		"x y z": "`x y z`",
	}
	for in, want := range cases {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}
