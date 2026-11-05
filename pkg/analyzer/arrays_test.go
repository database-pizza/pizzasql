package analyzer

import (
	"errors"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

// arrayCatalog describes an array-typed table so expression analysis sees the
// same declared types the executor resolves from storage.
func arrayCatalog() *Catalog {
	catalog := NewCatalog()
	catalog.CreateTable(&TableInfo{
		Name: "documents",
		Columns: []ColumnInfo{
			{Name: "id", Type: TypeInteger, PrimaryKey: true},
			{Name: "tags", Type: TypeFromName("TEXT[]"), Nullable: true, Array: true},
			{Name: "nums", Type: TypeFromName("INTEGER[]"), Nullable: true, Array: true},
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
		`SELECT nums FROM documents WHERE nums = '{3,4}'`,
		`SELECT nums FROM documents WHERE '{3,4}' = nums`,
		`SELECT nums FROM documents WHERE nums <> '{3,4}'`,
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
		// A text literal is only coercible to an array side, not to a scalar
		// integer column.
		{`SELECT id FROM documents WHERE id = '{3,4}'`, "cannot compare INTEGER with TEXT"},
		// An array column still does not compare with a scalar number.
		{`SELECT nums FROM documents WHERE nums = 5`, "cannot compare INTEGER with INTEGER"},
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

func TestTableFunctionColumnAliasesAnalyze(t *testing.T) {
	a := New(arrayCatalog())
	for _, sql := range []string{
		`SELECT u.x FROM unnest('{1,2}'::INTEGER[]) AS u(x)`,
		`SELECT x FROM unnest('{1,2}'::INTEGER[]) AS u(x) WHERE x > 1`,
		`SELECT e.k, e.v FROM jsonb_each('{"a":1}') AS e(k, v)`,
	} {
		if err := a.Analyze(parse(t, sql)); err != nil {
			t.Errorf("analyze %q: %v", sql, err)
		}
	}

	// A column alias list must match the function's output arity.
	err := a.Analyze(parse(t, `SELECT * FROM jsonb_each('{"a":1}') AS e(k)`))
	if !errors.Is(err, pgerror.ErrInvalidColumnReference) {
		t.Fatalf("arity error = %v, want ErrInvalidColumnReference", err)
	}
}

func TestArrayContainmentAnalyzes(t *testing.T) {
	a := New(arrayCatalog())
	for _, sql := range []string{
		`SELECT nums FROM documents WHERE nums @> ARRAY[1]`,
		`SELECT nums FROM documents WHERE ARRAY[1] <@ nums`,
		`SELECT nums FROM documents WHERE nums @> '{1}'`,
		`SELECT tags FROM documents WHERE tags @> ARRAY['go']`,
		`SELECT nums FROM documents WHERE nums @> NULL`,
	} {
		if err := a.Analyze(parse(t, sql)); err != nil {
			t.Errorf("analyze %q: %v", sql, err)
		}
	}
	for _, sql := range []string{
		`SELECT nums FROM documents WHERE nums @> 5`,
		`SELECT nums FROM documents WHERE 5 <@ nums`,
	} {
		if err := a.Analyze(parse(t, sql)); err == nil {
			t.Errorf("analyze %q succeeded, want an array containment type error", sql)
		}
	}
}
