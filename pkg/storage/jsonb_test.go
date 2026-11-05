package storage

import (
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/analyzer"
)

// A JSONB column is persisted as the type name the catalog already stores for
// any column, with no row-format change: the canonical document is an ordinary
// text value and survives a manager reload byte-for-byte.
func TestJSONBColumnTypeSurvivesReload(t *testing.T) {
	_, pool, schemas, tables := newTestSession(t)
	if err := schemas.CreateTable(&Schema{
		Name: "docs",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", PrimaryKey: true, Nullable: false},
			{Name: "doc", Type: "JSONB", Nullable: true},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	canonical := `{"a":1,"b":2}`
	if err := tables.Insert("docs", Row{"id": int64(1), "doc": canonical}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	schema, err := schemas.GetSchema("docs")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if len(schema.Columns) != 2 || schema.Columns[1].Type != "JSONB" {
		t.Fatalf("declared columns = %+v, want doc JSONB", schema.Columns)
	}
	info := schema.ToAnalyzerTableInfo()
	if len(info.Columns) != 2 || info.Columns[1].Type != analyzer.TypeJSONB {
		t.Fatalf("analyzer column types = %+v, want doc JSONB", info.Columns)
	}
	if info.Columns[1].Array {
		t.Fatal("a JSONB column must not be marked as an array")
	}

	// The durable catalog stores the type name and the row value unchanged.
	var catalog string
	if err := pool.WithClient(func(c *KVClient) error {
		var err error
		catalog, err = c.Read("testdb:_schema:docs")
		return err
	}); err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	if !strings.Contains(catalog, `"type":"JSONB"`) {
		t.Fatalf("catalog = %s, want a JSONB column type", catalog)
	}

	reloadedSchemas := NewSchemaManager(pool, "testdb")
	reloadedTables := NewTableManager(pool, reloadedSchemas, "testdb")
	reloadedSchema, err := reloadedSchemas.GetSchema("docs")
	if err != nil {
		t.Fatalf("reload schema: %v", err)
	}
	if reloadedSchema.Columns[1].Type != "JSONB" {
		t.Fatalf("reloaded type = %q, want JSONB", reloadedSchema.Columns[1].Type)
	}
	rows, err := reloadedTables.Select("docs", nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("reloaded rows = %v, err %v", rows, err)
	}
	if rows[0]["doc"] != canonical {
		t.Fatalf("reloaded doc = %v, want %q", rows[0]["doc"], canonical)
	}
}
