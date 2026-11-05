package sqlexport

import (
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

// A schema dump must keep a non-constant default as SQL text; emitting the
// value evaluated at export time would freeze it.
func TestExportPreservesDefaultExpression(t *testing.T) {
	kv := testkv.New(t)
	pool := kv.Pool(4)
	t.Cleanup(func() { pool.Close() })

	schemaManager := storage.NewSchemaManager(pool, "testdb")
	tableManager := storage.NewTableManager(pool, schemaManager, "testdb")

	if err := schemaManager.CreateTable(&storage.Schema{
		Name: "events",
		Columns: []storage.Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// The table is empty, so the schema manager needs no row evaluator for the
	// rewrite and the column can be declared with an expression default.
	if err := schemaManager.AddColumn("events", storage.Column{
		Name:        "created_at",
		Type:        "TIMESTAMP",
		Nullable:    true,
		DefaultExpr: "CURRENT_TIMESTAMP",
	}, nil, nil); err != nil {
		t.Fatalf("add column: %v", err)
	}

	dump, err := ExportDatabase(schemaManager, tableManager, ExportOptions{Tables: []string{"events"}})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(dump, "DEFAULT CURRENT_TIMESTAMP") {
		t.Fatalf("dump lost the default expression:\n%s", dump)
	}
}

// A composite key must be dumped as one table-level constraint; emitting an
// inline PRIMARY KEY on each key column would describe two keys.
func TestExportCompositePrimaryKey(t *testing.T) {
	kv := testkv.New(t)
	pool := kv.Pool(4)
	t.Cleanup(func() { pool.Close() })

	schemaManager := storage.NewSchemaManager(pool, "testdb")
	tableManager := storage.NewTableManager(pool, schemaManager, "testdb")

	if err := schemaManager.CreateTable(&storage.Schema{
		Name:              "pairs",
		PrimaryKeyColumns: []string{"a", "b"},
		Columns: []storage.Column{
			{Name: "a", Type: "TEXT", Nullable: false},
			{Name: "b", Type: "TEXT", Nullable: false},
			{Name: "v", Type: "TEXT", Nullable: true},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	dump, err := ExportDatabase(schemaManager, tableManager, ExportOptions{Tables: []string{"pairs"}})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if got := strings.Count(dump, "PRIMARY KEY"); got != 1 {
		t.Fatalf("dump has %d PRIMARY KEY clauses, want 1:\n%s", got, dump)
	}
	if !strings.Contains(dump, "PRIMARY KEY (a, b)") {
		t.Fatalf("dump lost the composite key:\n%s", dump)
	}
}

// CHECK constraints are part of the schema and must survive a logical dump.
func TestExportPreservesCheckConstraints(t *testing.T) {
	kv := testkv.New(t)
	pool := kv.Pool(4)
	t.Cleanup(func() { pool.Close() })

	schemaManager := storage.NewSchemaManager(pool, "testdb")
	tableManager := storage.NewTableManager(pool, schemaManager, "testdb")

	if err := schemaManager.CreateTable(&storage.Schema{
		Name: "items",
		Checks: []storage.CheckConstraint{
			{Name: "positive_qty", Expression: "qty > 0"},
			{Expression: "LENGTH(name) > 0"},
		},
		Columns: []storage.Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "qty", Type: "INTEGER", Nullable: false},
			{Name: "name", Type: "TEXT", Nullable: false},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	dump, err := ExportDatabase(schemaManager, tableManager, ExportOptions{Tables: []string{"items"}})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(dump, `CONSTRAINT positive_qty CHECK (qty > 0)`) {
		t.Fatalf("dump lost the named CHECK:\n%s", dump)
	}
	if !strings.Contains(dump, `CHECK (LENGTH(name) > 0)`) {
		t.Fatalf("dump lost the unnamed CHECK:\n%s", dump)
	}
}
