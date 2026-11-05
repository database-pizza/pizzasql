package parser

import (
	"errors"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

// The jsonb path/containment/existence operators parse at their intended
// precedence and keep the JSON1 arrows working.
func TestParseJSONBOperators(t *testing.T) {
	cases := map[string]lexer.TokenType{
		`SELECT doc -> 'a' FROM docs`:    lexer.TokenArrow,
		`SELECT doc ->> 'a' FROM docs`:   lexer.TokenArrowText,
		`SELECT doc #> '{a}' FROM docs`:  lexer.TokenHashArrow,
		`SELECT doc #>> '{a}' FROM docs`: lexer.TokenHashArrowText,
		`SELECT doc #- '{a}' FROM docs`:  lexer.TokenHashMinus,
		`SELECT doc @> '{}' FROM docs`:   lexer.TokenAtArrow,
		`SELECT doc <@ '{}' FROM docs`:   lexer.TokenLtAt,
		`SELECT doc ? 'a' FROM docs`:     lexer.TokenQuestion,
		`SELECT doc ?| '{a}' FROM docs`:  lexer.TokenQuestionPipe,
		`SELECT doc ?& '{a}' FROM docs`:  lexer.TokenQuestionAmp,
	}
	for sql, wantOp := range cases {
		stmt := parse(t, sql)
		sel := stmt.(*SelectStmt)
		binary, ok := sel.Columns[0].Expr.(*BinaryExpr)
		if !ok || binary.Op != wantOp {
			t.Fatalf("%s: expression = %#v, want operator %v", sql, sel.Columns[0].Expr, wantOp)
		}
	}

	// Containment binds looser than extraction: doc -> 'a' @> '1'.
	sel := parse(t, `SELECT doc -> 'a' @> '1' FROM docs`).(*SelectStmt)
	binary := sel.Columns[0].Expr.(*BinaryExpr)
	if binary.Op != lexer.TokenAtArrow {
		t.Fatalf("containment operator = %v", binary.Op)
	}
	if left, ok := binary.Left.(*BinaryExpr); !ok || left.Op != lexer.TokenArrow {
		t.Fatalf("left operand = %#v, want ->", binary.Left)
	}

	// A jsonb cast is a normal cast target and keeps the extract/compare chain.
	sel = parse(t, `SELECT CAST(doc AS JSONB) -> 'a' = '1'::jsonb FROM docs`).(*SelectStmt)
	if eq, ok := sel.Columns[0].Expr.(*BinaryExpr); !ok || eq.Op != lexer.TokenEq {
		t.Fatalf("cast chain = %#v, want =", sel.Columns[0].Expr)
	}

	// jsonpath operators are recognized and rejected as unsupported.
	for _, sql := range []string{
		`SELECT doc @? '$.a' FROM docs`,
		`SELECT doc @@ '$.a' FROM docs`,
	} {
		l := lexer.New(sql)
		if _, err := New(l).Parse(); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Errorf("%s error = %v, want 0A000", sql, err)
		}
	}
}

// Non-btree index methods, including the GIN method used for jsonb in
// PostgreSQL, are rejected instead of creating a btree index with different
// behavior.
func TestParseCreateIndexMethod(t *testing.T) {
	if _, err := parseAllowError(t, `CREATE INDEX i ON docs USING BTREE (doc)`); err != nil {
		t.Fatalf("BTREE index: %v", err)
	}
	for _, sql := range []string{
		`CREATE INDEX i ON docs USING GIN (doc)`,
		`CREATE INDEX i ON docs USING GIST (doc)`,
	} {
		if _, err := parseAllowError(t, sql); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Errorf("%s error = %v, want 0A000", sql, err)
		}
	}
}

// The jsonb set-returning functions are recognized as FROM table functions,
// while unknown function-shaped table references stay unsupported.
func TestParseJSONBTableFunctions(t *testing.T) {
	for _, sql := range []string{
		`SELECT * FROM jsonb_each('{"a":1}')`,
		`SELECT * FROM jsonb_each_text('{"a":1}') AS e`,
		`SELECT * FROM jsonb_array_elements('[1]')`,
		`SELECT * FROM jsonb_array_elements_text('[1]')`,
		`SELECT * FROM jsonb_object_keys('{"a":1}')`,
	} {
		stmt := parse(t, sql)
		sel := stmt.(*SelectStmt)
		if len(sel.From) != 1 || sel.From[0].Function == nil {
			t.Fatalf("%s: FROM = %#v, want a table function", sql, sel.From)
		}
	}

	l := lexer.New(`SELECT * FROM mystery('x')`)
	if _, err := New(l).Parse(); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
		t.Fatalf("unknown table function error = %v, want 0A000", err)
	}
}

// parseAllowError parses without failing the test, so a rejected statement can
// be inspected.
func parseAllowError(t *testing.T, sql string) (Statement, error) {
	t.Helper()
	l := lexer.New(sql)
	return New(l).Parse()
}
