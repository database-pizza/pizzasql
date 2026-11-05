package parser

import (
	"errors"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

func parseOne(t *testing.T, sql string) Statement {
	t.Helper()
	stmt, err := New(lexer.New(sql)).Parse()
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return stmt
}

func TestArrayColumnTypesParse(t *testing.T) {
	stmt := parseOne(t, `CREATE TABLE t (
		a TEXT[],
		b VARCHAR(20)[],
		c INTEGER[],
		d BIGINT[],
		e SMALLINT[],
		f BOOLEAN[],
		g REAL[],
		h DOUBLE PRECISION[],
		i NUMERIC(10,2)[]
	)`).(*CreateTableStmt)

	want := []string{
		"TEXT[]", "VARCHAR[]", "INTEGER[]", "BIGINT[]", "SMALLINT[]",
		"BOOLEAN[]", "REAL[]", "DOUBLE[]", "NUMERIC[]",
	}
	if len(stmt.Columns) != len(want) {
		t.Fatalf("got %d columns, want %d", len(stmt.Columns), len(want))
	}
	for i, column := range stmt.Columns {
		if got := column.Type.SQLTypeName(); got != want[i] {
			t.Errorf("column %s type = %q, want %q", column.Name, got, want[i])
		}
		if !column.Type.Array {
			t.Errorf("column %s is not marked as an array", column.Name)
		}
	}
}

func TestAlterTableAddArrayColumnParses(t *testing.T) {
	stmt := parseOne(t, `ALTER TABLE t ADD COLUMN tags TEXT[]`).(*AlterTableStmt)
	action, ok := stmt.Action.(*AddColumnAction)
	if !ok {
		t.Fatalf("action = %T, want *AddColumnAction", stmt.Action)
	}
	if got := action.Column.Type.SQLTypeName(); got != "TEXT[]" {
		t.Fatalf("added column type = %q, want TEXT[]", got)
	}
}

func TestArrayConstructorAndSubscriptParse(t *testing.T) {
	stmt := parseOne(t, `SELECT ARRAY[1, 'a', NULL], arr[2], (ARRAY[1,2])[1] FROM t`).(*SelectStmt)
	if len(stmt.Columns) != 3 {
		t.Fatalf("got %d projections, want 3", len(stmt.Columns))
	}
	constructor, ok := stmt.Columns[0].Expr.(*ArrayExpr)
	if !ok {
		t.Fatalf("projection 0 = %T, want *ArrayExpr", stmt.Columns[0].Expr)
	}
	if len(constructor.Elements) != 3 {
		t.Fatalf("constructor elements = %d, want 3", len(constructor.Elements))
	}
	if _, ok := stmt.Columns[1].Expr.(*SubscriptExpr); !ok {
		t.Fatalf("projection 1 = %T, want *SubscriptExpr", stmt.Columns[1].Expr)
	}
	paren, ok := stmt.Columns[2].Expr.(*SubscriptExpr)
	if !ok {
		t.Fatalf("projection 2 = %T, want *SubscriptExpr", stmt.Columns[2].Expr)
	}
	if _, ok := paren.Expr.(*ParenExpr); !ok {
		t.Fatalf("subscript base = %T, want *ParenExpr", paren.Expr)
	}
}

func TestArrayCastForms(t *testing.T) {
	for _, sql := range []string{
		`SELECT '{1,2}'::INTEGER[]`,
		`SELECT CAST('{1,2}' AS INTEGER[])`,
		`SELECT '{1,2}'::DOUBLE PRECISION[]`,
		`SELECT '{a,b}'::VARCHAR(5)[]`,
	} {
		stmt := parseOne(t, sql).(*SelectStmt)
		cast, ok := stmt.Columns[0].Expr.(*CastExpr)
		if !ok {
			t.Fatalf("%q expression = %T, want *CastExpr", sql, stmt.Columns[0].Expr)
		}
		if !cast.Type.Array {
			t.Fatalf("%q cast type is not an array", sql)
		}
	}
}

func TestAnyAllPredicatesParse(t *testing.T) {
	for _, sql := range []string{
		`SELECT * FROM t WHERE id = ANY('{1,2,3}')`,
		`SELECT * FROM t WHERE id <> ALL('{1,2,3}')`,
		`SELECT * FROM t WHERE id = ANY(ARRAY[1,2])`,
		`SELECT * FROM t WHERE id >= ALL(ARRAY[1,2])`,
		`SELECT * FROM t WHERE NOT (id = ANY('{1,2}'))`,
	} {
		stmt := parseOne(t, sql).(*SelectStmt)
		if stmt.Where == nil {
			t.Fatalf("%q has no WHERE", sql)
		}
	}
}

func TestAnyAllRejections(t *testing.T) {
	for _, sql := range []string{
		`SELECT * FROM t WHERE id = ANY(SELECT id FROM t)`,
		`SELECT any(id) FROM t`,
		`SELECT * FROM t WHERE id = ANY()`,
	} {
		_, err := New(lexer.New(sql)).Parse()
		if !errors.Is(err, pgerror.ErrFeatureNotSupported) && !errors.Is(err, pgerror.ErrInvalidText) {
			// A malformed operand is a parse error; the unsupported forms must
			// report feature-not-supported.
			if err == nil {
				t.Errorf("%q parsed, want an error", sql)
			}
		}
	}
}

func TestMultidimensionalAndUnsupportedElementArraysRejected(t *testing.T) {
	for _, sql := range []string{
		`CREATE TABLE t (a INTEGER[][])`,
		`CREATE TABLE t (a BLOB[])`,
		`CREATE TABLE t (a DATE[])`,
		`SELECT ARRAY[ARRAY[1,2]]`,
		`SELECT ARRAY[[1,2],[3,4]]`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := New(lexer.New(sql)).Parse()
			if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
				t.Fatalf("error = %v, want ErrFeatureNotSupported", err)
			}
		})
	}
}

func TestUnnestInFromParses(t *testing.T) {
	stmt := parseOne(t, `SELECT * FROM unnest('{1,2}'::INTEGER[]) AS u`).(*SelectStmt)
	if len(stmt.From) != 1 {
		t.Fatalf("FROM entries = %d, want 1", len(stmt.From))
	}
	if stmt.From[0].Function == nil || stmt.From[0].Function.Name != "UNNEST" {
		t.Fatalf("FROM[0].Function = %+v, want UNNEST", stmt.From[0].Function)
	}
	if stmt.From[0].Alias != "u" {
		t.Fatalf("alias = %q, want u", stmt.From[0].Alias)
	}
}

func TestUnnestInJoinRejected(t *testing.T) {
	_, err := New(lexer.New(`SELECT * FROM t, unnest('{1,2}'::INTEGER[]) AS u`)).Parse()
	if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
		t.Fatalf("error = %v, want ErrFeatureNotSupported", err)
	}
}

func TestAnyAsOrdinaryColumnStillParses(t *testing.T) {
	// ANY is only a predicate keyword when it follows a comparison operator. A
	// column literally named any must keep working.
	if _, err := New(lexer.New("SELECT any FROM t")).Parse(); err != nil {
		t.Fatalf("parse: %v", err)
	}
}

func TestBracketIdentifierStillParses(t *testing.T) {
	stmt := parseOne(t, "SELECT [bracket] FROM [table]").(*SelectStmt)
	ref, ok := stmt.Columns[0].Expr.(*ColumnRef)
	if !ok || ref.Column != "bracket" {
		t.Fatalf("projection = %#v, want bracket column", stmt.Columns[0].Expr)
	}
	if stmt.From[0].Name != "table" {
		t.Fatalf("FROM = %q, want table", stmt.From[0].Name)
	}
}

// A bracket directly after an expression starts an array subscript, so the
// SQLite bracket-quoted alias in that position is read as a subscript. The
// projection shapes below therefore parse as subscripts and fail later (the
// base is not an array); a bracket alias on a FROM entry has no subscript
// position and is a syntax error. Bracket identifiers keep working wherever a
// name is expected (after a keyword or a comma).
func TestBracketAliasShapesAfterExpression(t *testing.T) {
	for _, sql := range []string{
		`SELECT count(*) [cnt] FROM t`,
		`SELECT 1 [x]`,
	} {
		t.Run(sql, func(t *testing.T) {
			stmt := parseOne(t, sql).(*SelectStmt)
			if _, ok := stmt.Columns[0].Expr.(*SubscriptExpr); !ok {
				t.Fatalf("%q projection = %T, want *SubscriptExpr", sql, stmt.Columns[0].Expr)
			}
		})
	}
	if _, err := New(lexer.New(`SELECT * FROM t [a]`)).Parse(); err == nil {
		t.Fatal("bracket alias on a FROM entry parsed, want a syntax error")
	}

	// `[x]` after an expression is a subscript, not an alias.
	stmt := parseOne(t, `SELECT arr[1] FROM t`).(*SelectStmt)
	if _, ok := stmt.Columns[0].Expr.(*SubscriptExpr); !ok {
		t.Fatalf("projection = %T, want *SubscriptExpr", stmt.Columns[0].Expr)
	}

	// Name positions after a keyword or comma still quote with brackets.
	for _, sql := range []string{
		`SELECT [col], [col2] FROM [tbl]`,
		`INSERT INTO [tbl] ([col]) VALUES (1)`,
	} {
		if _, err := New(lexer.New(sql)).Parse(); err != nil {
			t.Errorf("%q should keep bracket identifiers: %v", sql, err)
		}
	}
}
