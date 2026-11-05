package pgserver

import (
	"context"
	"testing"
)

// A stock pgx connection introspects the catalog with its normal
// Parse/Bind/Execute path. This covers the real driver reading oid/name/regclass
// columns and issuing SET during a session.
func TestPGXCatalogIntrospection(t *testing.T) {
	conn := connectPGX(t)
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE TABLE catalog_parent (id INTEGER PRIMARY KEY, label TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE catalog_child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL, note TEXT DEFAULT 'n', CONSTRAINT fk_catalog_child FOREIGN KEY (parent_id) REFERENCES catalog_parent(id))`,
		`CREATE INDEX idx_catalog_child_note ON catalog_child (note)`,
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	var (
		oid     uint32
		relname string
		relkind string
	)
	err := conn.QueryRow(ctx, `
		SELECT c.oid, c.relname, c.relkind
		FROM pg_catalog.pg_class c
		WHERE c.relname = 'catalog_child'`).Scan(&oid, &relname, &relkind)
	if err != nil {
		t.Fatalf("pg_class scan: %v", err)
	}
	if oid == 0 || relname != "catalog_child" || relkind != "r" {
		t.Fatalf("pg_class = %d %q %q", oid, relname, relkind)
	}

	rows, err := conn.Query(ctx, `
		SELECT n.nspname, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
		WHERE c.relname = 'catalog_child' AND a.attnum > 0
		ORDER BY a.attnum`)
	if err != nil {
		t.Fatalf("catalog join: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var schema, column, typ string
		if err := rows.Scan(&schema, &column, &typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if schema != "public" {
			t.Fatalf("schema = %q", schema)
		}
		columns = append(columns, column+":"+typ)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []string{"id:integer", "parent_id:integer", "note:text"}
	if len(columns) != len(want) {
		t.Fatalf("columns = %v, want %v", columns, want)
	}
	for i := range want {
		if columns[i] != want[i] {
			t.Fatalf("columns = %v, want %v", columns, want)
		}
	}

	var (
		tableName  string
		columnName string
		dataType   string
		isNullable string
	)
	err = conn.QueryRow(ctx, `
		SELECT table_name, column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_name = 'catalog_child' AND column_name = 'note'`).
		Scan(&tableName, &columnName, &dataType, &isNullable)
	if err != nil {
		t.Fatalf("information_schema scan: %v", err)
	}
	if tableName != "catalog_child" || columnName != "note" || dataType != "text" || isNullable != "YES" {
		t.Fatalf("information_schema row = %q %q %q %q", tableName, columnName, dataType, isNullable)
	}

	if _, err := conn.Exec(ctx, "SET search_path TO public"); err != nil {
		t.Fatalf("SET search_path: %v", err)
	}
	var searchPath string
	if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
		t.Fatalf("SHOW search_path: %v", err)
	}
	if searchPath != "public" {
		t.Fatalf("search_path = %q", searchPath)
	}

	var regclassRows int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_catalog.pg_constraint
		WHERE conrelid = 'catalog_child'::regclass AND contype = 'f'`).Scan(&regclassRows); err != nil {
		t.Fatalf("regclass constraint query: %v", err)
	}
	if regclassRows != 1 {
		t.Fatalf("foreign key constraint count = %d, want 1", regclassRows)
	}
}
