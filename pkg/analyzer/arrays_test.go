package analyzer

import (
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
)

// arrayCatalog describes an array-typed table so expression analysis sees the
// same declared types the executor resolves from storage.
func arrayCatalog() *Catalog {
	catalog := NewCatalog()
	catalog.CreateTable(&TableInfo{
		Name: "documents",
		Columns: []ColumnInfo{
			{Name: "id", Type: TypeInteger, PrimaryKey: true},
			{Name: "tags", Type: TypeFromName("TEXT[]"), Nullable: true},
			{Name: "nums", Type: TypeFromName("INTEGER[]"), Nullable: true},
		},
	})
	return catalog
}

func TestArrayTypeNameKeepsElementAffinity(t *testing.T) {
	cases := map[string]Type{
		"TEXT[]":             TypeText,
		"VARCHAR(10)[]":      TypeText,
		"INTEGER[]":          TypeInteger,
		"BIGINT[]":           TypeInteger,
		"BOOLEAN[]":          TypeBoolean,
		"DOUBLE PRECISION[]": TypeReal,
		"NUMERIC[]":          TypeNumeric,
	}
	for name, want := range cases {
		if got := TypeFromName(name); got != want {
			t.Errorf("TypeFromName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestArrayColumnDDLAnalyzes(t *testing.T) {
	a := New(setupCatalog())
	if err := a.Analyze(parse(t, `CREATE TABLE tagged (id INTEGER PRIMARY KEY, tags TEXT[], nums INTEGER[])`)); err != nil {
		t.Fatalf("array column DDL: %v", err)
	}
	if err := a.Analyze(parse(t, `ALTER TABLE tagged ADD COLUMN flags BOOLEAN[]`)); err != nil {
		t.Fatalf("ALTER TABLE ADD array column: %v", err)
	}
}

func TestArrayExpressionsAnalyze(t *testing.T) {
	a := New(arrayCatalog())
	for _, sql := range []string{
		`SELECT tags FROM documents WHERE 'go' = ANY(tags)`,
		`SELECT tags FROM documents WHERE 'go' <> ALL(tags)`,
		`SELECT nums FROM documents WHERE id = ANY(ARRAY[1,2])`,
		`SELECT tags[1] FROM documents WHERE tags[1] = 'go'`,
		`SELECT array_length(tags, 1) FROM documents`,
		`SELECT ARRAY['a','b'] FROM documents`,
		`SELECT '{1,2}'::INTEGER[] FROM documents`,
	} {
		if err := a.Analyze(parse(t, sql)); err != nil {
			t.Errorf("analyze %q: %v", sql, err)
		}
	}
}

func TestArrayExpressionRejections(t *testing.T) {
	a := New(arrayCatalog())
	cases := []struct {
		sql  string
		want string
	}{
		{`SELECT missing[1] FROM documents`, "column not found"},
		{`SELECT tags['x'] FROM documents`, "array subscript must be an integer expression"},
	}
	for _, tc := range cases {
		err := a.Analyze(parse(t, tc.sql))
		if err == nil {
			t.Errorf("%q analyzed, want an error", tc.sql)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q error = %v, want it to mention %q", tc.sql, err, tc.want)
		}
	}
}

func TestUnnestFromDefinesRelation(t *testing.T) {
	a := New(arrayCatalog())
	stmt := parse(t, `SELECT u.unnest FROM unnest('{1,2}'::INTEGER[]) AS u`).(*parser.SelectStmt)
	if err := a.Analyze(stmt); err != nil {
		t.Fatalf("unnest FROM analysis: %v", err)
	}
}
