package storage

import (
	"strings"
	"testing"
	"time"
)

// A validation failure late in a multi-batch ADD COLUMN rewrite must not leave
// the new column's values in rows the schema never published. The dry-run
// validation added for this case must make the ALTER leave durable rows
// byte-identical.
func TestAddColumnFailureLeavesRowsByteIdentical(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	pool := newTestKVPool(kv, 4, 5*time.Second)
	defer pool.Close()
	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	if err := schemas.CreateTable(&Schema{
		Name:       "t",
		PrimaryKey: "id",
		Columns:    []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true, Nullable: false}},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1100; i++ {
		if err := tables.Insert("t", Row{"id": int64(i)}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	prefix := "testdb:_data:t:"
	rawRows := func() map[string]string {
		kv.mu.Lock()
		defer kv.mu.Unlock()
		rows := make(map[string]string)
		for key, value := range kv.data {
			if strings.HasPrefix(key, prefix) {
				rows[key] = string(value)
			}
		}
		return rows
	}
	before := rawRows()

	next := 0
	schemas.SetDefaultEvaluator(func(string, time.Time) (interface{}, error) {
		next++
		return int64(next % 1024), nil
	})
	err := schemas.AddColumn("t", Column{
		Name:        "seq",
		Type:        "INTEGER",
		Nullable:    true,
		DefaultExpr: "next_value()",
	}, nil, &Index{
		Name:    "uniq_t_seq",
		Table:   "t",
		Unique:  true,
		Columns: []IndexColumn{{Name: "seq"}},
	})
	if err == nil {
		t.Fatal("expected duplicate default values to reject ADD COLUMN")
	}
	var uniqueErr *UniqueViolationError
	if !asUniqueViolation(err, &uniqueErr) {
		t.Fatalf("error = %v, want UniqueViolationError", err)
	}

	after := rawRows()
	if len(before) != len(after) {
		t.Fatalf("row count changed from %d to %d", len(before), len(after))
	}
	for key, value := range before {
		if after[key] != value {
			t.Fatalf("row %s changed after rejected ADD COLUMN", key)
		}
	}
}

func asUniqueViolation(err error, target **UniqueViolationError) bool {
	unique, ok := err.(*UniqueViolationError)
	if ok {
		*target = unique
	}
	return ok
}
