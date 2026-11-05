package sqliteimport

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

// CHECK constraints are supported by the engine now, so the SQLite import
// sanitizer must not strip them. Unsupported expressions are rejected by the
// executor when the table is created instead of being silently dropped.
func TestSanitizeDDLPreservesCheckConstraints(t *testing.T) {
	ddl := `CREATE TABLE "items" ("id" INTEGER PRIMARY KEY, "qty" INTEGER NOT NULL, CHECK (qty > 0), CONSTRAINT "named_check" CHECK (LENGTH("name") > 0))`
	got := sanitizeDDL(ddl)
	for _, want := range []string{"CHECK (qty > 0)", `CONSTRAINT "named_check" CHECK (LENGTH("name") > 0)`} {
		if !strings.Contains(got, want) {
			t.Fatalf("sanitizeDDL removed %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AUTOINCREMENT") {
		t.Fatalf("sanitizeDDL kept AUTOINCREMENT:\n%s", got)
	}
}

func TestImportSQLitePreservesCheckConstraint(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/source.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, qty INTEGER CHECK (qty > 0))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO items VALUES (1, 5)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	kv := testkv.New(t)
	pool := kv.Pool(4)
	defer pool.Close()
	schema := storage.NewSchemaManager(pool, "testdb")
	table := storage.NewTableManager(pool, schema, "testdb")
	exec := executor.New(schema, table)
	exec.SyncCatalog()

	if _, err := ImportSQLiteFile(path, exec, DefaultImportOptions()); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := exec.Execute(mustParse(t, `INSERT INTO items VALUES (2, -1)`)); err == nil {
		t.Fatal("imported CHECK constraint was not enforced")
	}
}

func mustParse(t *testing.T, sql string) parser.Statement {
	t.Helper()
	stmt, err := parser.New(lexer.New(sql)).Parse()
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return stmt
}
