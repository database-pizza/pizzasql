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
