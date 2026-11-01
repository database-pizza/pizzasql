package storage

import (
	"errors"
	"fmt"
	"testing"
)

func TestUniqueCacheNotPublishedOnCompareConflict(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}, {Name: "v", Type: "INTEGER"}})
	if err := tables.CreateUniqueIndex(&Index{Name: "unique_v", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "v"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "v": int64(1)}); err != nil {
		t.Fatal(err)
	}
	session := NewSession(schemas, tables)
	if err := session.Begin(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.GetByPK("t", "1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.UpdateByPK("t", "1", func(Row) (Row, error) { return Row{"v": int64(2)}, nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tables.UpdateByPK("t", "1", func(Row) (Row, error) { return Row{"v": int64(3)}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := session.Commit(); !errors.Is(err, ErrSerialization) {
		t.Fatalf("commit: %v", err)
	}
	if err := tables.Insert("t", Row{"id": int64(2), "v": int64(2)}); err != nil {
		t.Fatalf("failed commit claimed a value: %v", err)
	}
	if err := tables.Insert("t", Row{"id": int64(3), "v": int64(3)}); err == nil {
		t.Fatal("failed commit lost durable owner")
	}
}

func TestUniqueExpressionCacheDoesNotFreezeEvaluator(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}, {Name: "v", Type: "TEXT"}})
	current := false
	tables.SetExpressionEvaluator(func(_ string, row Row) (interface{}, error) {
		if current {
			return "same", nil
		}
		return fmt.Sprint(row["v"]), nil
	})
	if err := tables.CreateUniqueIndex(&Index{Name: "expr", Table: "t", Unique: true, Columns: []IndexColumn{{Expression: "value(v)"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "v": "a"}); err != nil {
		t.Fatal(err)
	}
	current = true
	if err := tables.Insert("t", Row{"id": int64(2), "v": "b"}); err == nil {
		t.Fatal("constraint froze expression evaluation")
	}
	if len(tables.uniqueCache) != 0 {
		t.Fatal("expression UNIQUE cache retained without determinism proof")
	}
}
