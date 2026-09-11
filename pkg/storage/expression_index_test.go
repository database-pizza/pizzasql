package storage

import (
	"fmt"
	"strings"
	"testing"
)

// registerLowerEvaluator installs a tiny expression evaluator that understands
// the lower(col) form, mirroring what the SQL executor registers in production.
func registerLowerEvaluator(t *testing.T, tables *TableManager) {
	t.Helper()
	tables.SetExpressionEvaluator(func(expression string, row Row) (interface{}, error) {
		if strings.HasPrefix(expression, "lower(") && strings.HasSuffix(expression, ")") {
			col := expression[len("lower(") : len(expression)-1]
			if v, ok := row[col]; ok && v != nil {
				return strings.ToLower(fmt.Sprintf("%v", v)), nil
			}
			return nil, nil
		}
		return nil, fmt.Errorf("unsupported expression %q", expression)
	})
}

func TestExpressionUniqueIndexEnforced(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "users", []Column{
		{Name: "id", Type: "INTEGER", PrimaryKey: true},
		{Name: "email", Type: "TEXT"},
	})
	registerLowerEvaluator(t, tables)

	if err := schemas.CreateIndex(&Index{
		Name:   "users_email_lower",
		Table:  "users",
		Unique: true,
		Columns: []IndexColumn{
			{Name: "lower(email)", Expression: "lower(email)"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := tables.Insert("users", Row{"id": int64(1), "email": "Alice@Example.com"}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := tables.Insert("users", Row{"id": int64(2), "email": "alice@example.com"})
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("expected case-insensitive uniqueness violation, got %v", err)
	}
	if err := tables.Insert("users", Row{"id": int64(3), "email": "bob@example.com"}); err != nil {
		t.Fatalf("distinct insert: %v", err)
	}
}

func TestExpressionUniqueIndexAllowsNull(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "users", []Column{
		{Name: "id", Type: "INTEGER", PrimaryKey: true},
		{Name: "email", Type: "TEXT", Nullable: true},
	})
	registerLowerEvaluator(t, tables)
	if err := schemas.CreateIndex(&Index{
		Name: "users_email_lower", Table: "users", Unique: true,
		Columns: []IndexColumn{{Name: "lower(email)", Expression: "lower(email)"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("users", Row{"id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("users", Row{"id": int64(2)}); err != nil {
		t.Fatalf("NULL expression values must be exempt from uniqueness: %v", err)
	}
}

func TestGeneratedColumnMetadataRoundTrip(t *testing.T) {
	_, _, schemas, _ := newTestSession(t)
	schema := &Schema{
		Name: "t",
		Columns: []Column{
			{Name: "a", Type: "INTEGER", Nullable: true},
			{Name: "b", Type: "INTEGER", Nullable: true, GeneratedExpr: "a + 1", GeneratedStored: true},
		},
	}
	if err := schemas.CreateTable(schema); err != nil {
		t.Fatal(err)
	}
	got, err := schemas.GetSchema("t")
	if err != nil {
		t.Fatal(err)
	}
	b, ok := got.GetColumn("b")
	if !ok {
		t.Fatal("column b missing")
	}
	if b.GeneratedExpr != "a + 1" || !b.GeneratedStored {
		t.Fatalf("generated metadata not persisted: %#v", b)
	}
}

func TestIndexExpressionMetadataRoundTrip(t *testing.T) {
	_, _, schemas, _ := newTestSession(t)
	createTestTable(t, schemas, "users", []Column{{Name: "email", Type: "TEXT"}})
	if err := schemas.CreateIndex(&Index{
		Name: "users_email_lower", Table: "users", Unique: true,
		Columns: []IndexColumn{{Name: "lower(email)", Expression: "lower(email)"}},
	}); err != nil {
		t.Fatal(err)
	}
	idx, err := schemas.GetIndex("users_email_lower")
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Columns) != 1 || idx.Columns[0].Expression != "lower(email)" {
		t.Fatalf("expression metadata not persisted: %#v", idx.Columns)
	}
}

func TestExpressionIndexEvaluatorErrorPropagates(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "users", []Column{
		{Name: "id", Type: "INTEGER", PrimaryKey: true},
		{Name: "email", Type: "TEXT", Nullable: true},
	})
	registerLowerEvaluator(t, tables)
	if err := schemas.CreateIndex(&Index{
		Name: "users_email_lower", Table: "users",
		Columns: []IndexColumn{{Name: "lower(email)", Expression: "lower(email)"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tables.BuildIndex("users_email_lower", "users", []string{"lower(email)"}); err != nil {
		t.Fatal(err)
	}

	// A broken evaluator must surface, not be skipped during index maintenance.
	tables.SetExpressionEvaluator(func(expression string, row Row) (interface{}, error) {
		return nil, fmt.Errorf("evaluator boom")
	})
	if err := tables.Insert("users", Row{"id": int64(1), "email": "a@x"}); err == nil {
		t.Fatal("expected evaluator error to propagate from Insert")
	}
}
