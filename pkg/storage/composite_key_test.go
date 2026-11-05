package storage

import (
	"errors"
	"strings"
	"testing"
)

// compositeTestTable creates a table whose primary key is (a, b). The key
// columns are declared nullable on purpose: CreateTable must make them NOT NULL.
func compositeTestTable(t *testing.T, schemas *SchemaManager) {
	t.Helper()
	if err := schemas.CreateTable(&Schema{
		Name:              "t",
		PrimaryKeyColumns: []string{"a", "b"},
		Columns: []Column{
			{Name: "a", Type: "TEXT", Nullable: true},
			{Name: "b", Type: "TEXT", Nullable: true},
			{Name: "v", Type: "TEXT", Nullable: true},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}
}

func TestCompositePrimaryKeyNormalizesSchema(t *testing.T) {
	_, _, schemas, _ := newTestSession(t)
	compositeTestTable(t, schemas)

	schema, err := schemas.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if schema.PrimaryKey != "" {
		t.Fatalf("PrimaryKey = %q, want empty for a composite key", schema.PrimaryKey)
	}
	if strings.Join(schema.PrimaryKeyList(), ",") != "a,b" {
		t.Fatalf("PrimaryKeyList = %v, want [a b]", schema.PrimaryKeyList())
	}
	if !schema.IsCompositePrimaryKey() || schema.IsSingleColumnPrimaryKey() {
		t.Fatal("composite key predicates disagree with the column list")
	}
	for _, column := range schema.Columns {
		if schema.IsPrimaryKeyColumn(column.Name) {
			if column.Nullable {
				t.Fatalf("key column %s is nullable", column.Name)
			}
			if !column.PrimaryKey {
				t.Fatalf("key column %s is not marked PrimaryKey", column.Name)
			}
		}
	}
}

func TestCompositePrimaryKeyEncodingIsUnambiguous(t *testing.T) {
	kv, _, schemas, tables := newTestSession(t)
	compositeTestTable(t, schemas)

	if err := tables.Insert("t", Row{"a": "ab", "b": "c", "v": "first"}); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	if err := tables.Insert("t", Row{"a": "a", "b": "bc", "v": "second"}); err != nil {
		t.Fatalf("insert second: %v", err)
	}

	// Each component is length-prefixed, so the two rows cannot share a key.
	if !kv.hasKey("testdb:_data:t:2:ab1:c") {
		t.Fatal("expected the first composite key to be 2:ab1:c")
	}
	if !kv.hasKey("testdb:_data:t:1:a2:bc") {
		t.Fatal("expected the second composite key to be 1:a2:bc")
	}

	row, err := tables.GetByPK("t", "2:ab1:c")
	if err != nil {
		t.Fatalf("get by composite key: %v", err)
	}
	if row["v"] != "first" {
		t.Fatalf("row under 2:ab1:c = %v, want first", row["v"])
	}

	// A single component, or a truncated encoding, is not a row key.
	for _, key := range []string{"ab", "2:ab", "1:a", "2:ab1:", "2:ab4:bc12"} {
		if _, err := tables.GetByPK("t", key); err != ErrKeyNotFound {
			t.Fatalf("GetByPK(%q) error = %v, want ErrKeyNotFound", key, err)
		}
	}
}

func TestCompositePrimaryKeyInsertErrors(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	compositeTestTable(t, schemas)

	var notNull *NotNullViolationError
	if err := tables.Insert("t", Row{"a": "x"}); !errors.As(err, &notNull) || notNull.Column != "b" {
		t.Fatalf("missing component error = %v, want NOT NULL violation on b", err)
	}
	notNull = nil
	if err := tables.Insert("t", Row{"a": "x", "b": nil}); !errors.As(err, &notNull) || notNull.Column != "b" {
		t.Fatalf("NULL component error = %v, want NOT NULL violation on b", err)
	}

	if err := tables.Insert("t", Row{"a": "x", "b": "y", "v": "one"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var duplicate *PrimaryKeyViolationError
	if err := tables.Insert("t", Row{"a": "x", "b": "y", "v": "again"}); !errors.As(err, &duplicate) {
		t.Fatalf("duplicate error = %v, want PrimaryKeyViolationError", err)
	}

	// Rejected inserts must not consume the row. Only the accepted row exists.
	rows, err := tables.Select("t", nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(rows) != 1 || rows[0]["v"] != "one" {
		t.Fatalf("rows after rejected inserts = %v", rows)
	}
}

// A duplicate composite key reports its components in declaration order, not the
// length-prefixed durable encoding, and a component that itself contains the
// encoding separator or a quote keeps its own tuple position.
func TestCompositePrimaryKeyViolationMessage(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	compositeTestTable(t, schemas)

	cases := []struct {
		row  Row
		want string
	}{
		{Row{"a": "x", "b": "y"}, "duplicate primary key: ('x','y')"},
		{Row{"a": "a:b", "b": "c"}, "duplicate primary key: ('a:b','c')"},
		{Row{"a": "a,b", "b": "it's"}, "duplicate primary key: ('a,b','it''s')"},
	}
	for _, tc := range cases {
		if err := tables.Insert("t", tc.row); err != nil {
			t.Fatalf("insert %v: %v", tc.row, err)
		}
		err := tables.Insert("t", tc.row)
		var duplicate *PrimaryKeyViolationError
		if !errors.As(err, &duplicate) {
			t.Fatalf("duplicate insert %v error = %v, want PrimaryKeyViolationError", tc.row, err)
		}
		if got := err.Error(); got != tc.want {
			t.Fatalf("duplicate insert %v message = %q, want %q", tc.row, got, tc.want)
		}
	}
}

// The transaction paths report the same rendering for a key staged in the
// overlay and for one already durable outside the transaction.
func TestSessionCompositePrimaryKeyViolationMessage(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	compositeTestTable(t, schemas)

	if err := tables.Insert("t", Row{"a": "durable", "b": "x:y"}); err != nil {
		t.Fatalf("insert durable: %v", err)
	}

	s := NewSession(schemas, tables)
	if err := s.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("t", Row{"a": "staged", "b": "y,z"}); err != nil {
		t.Fatalf("insert staged: %v", err)
	}

	for _, tc := range []struct {
		row  Row
		want string
	}{
		{Row{"a": "staged", "b": "y,z"}, "duplicate primary key: ('staged','y,z')"},
		{Row{"a": "durable", "b": "x:y"}, "duplicate primary key: ('durable','x:y')"},
	} {
		err := s.Insert("t", tc.row)
		var duplicate *PrimaryKeyViolationError
		if !errors.As(err, &duplicate) {
			t.Fatalf("transaction duplicate %v error = %v, want PrimaryKeyViolationError", tc.row, err)
		}
		if got := err.Error(); got != tc.want {
			t.Fatalf("transaction duplicate %v message = %q, want %q", tc.row, got, tc.want)
		}
	}
	if err := s.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}

func TestCompositePrimaryKeyUpdateAndDelete(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	compositeTestTable(t, schemas)

	if err := tables.Insert("t", Row{"a": "x", "b": "y", "v": "one"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tables.Insert("t", Row{"a": "x", "b": "z", "v": "two"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	old, ok, err := tables.UpdateByPK("t", "1:x1:y", func(Row) (Row, error) {
		return Row{"v": "updated"}, nil
	})
	if err != nil || !ok {
		t.Fatalf("UpdateByPK = ok %v, err %v", ok, err)
	}
	if old["v"] != "one" {
		t.Fatalf("UpdateByPK old row = %v, want one", old["v"])
	}
	row, err := tables.GetByPK("t", "1:x1:y")
	if err != nil || row["v"] != "updated" {
		t.Fatalf("row after update = %v, err %v", row, err)
	}

	// Moving a key component through the scan path moves the durable key.
	session := NewSession(schemas, tables)
	if err := session.Begin(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	count, err := session.UpdateFunc("t",
		func(Row) (Row, error) { return Row{"b": "w"}, nil },
		func(row Row) bool { return row["b"] == "y" },
	)
	if err != nil {
		t.Fatalf("update key component: %v", err)
	}
	if err := session.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if count != 1 {
		t.Fatalf("moved rows = %d, want 1", count)
	}
	if _, err := tables.GetByPK("t", "1:x1:y"); err != ErrKeyNotFound {
		t.Fatalf("old key error = %v, want ErrKeyNotFound", err)
	}
	row, err = tables.GetByPK("t", "1:x1:w")
	if err != nil || row["v"] != "updated" {
		t.Fatalf("row under moved key = %v, err %v", row, err)
	}

	deleted, ok, err := tables.DeleteByPK("t", "1:x1:w")
	if err != nil || !ok {
		t.Fatalf("DeleteByPK = ok %v, err %v", ok, err)
	}
	if deleted["v"] != "updated" {
		t.Fatalf("DeleteByPK row = %v, want updated", deleted["v"])
	}
	if _, err := tables.GetByPK("t", "1:x1:w"); err != ErrKeyNotFound {
		t.Fatalf("deleted key error = %v, want ErrKeyNotFound", err)
	}
	if rows, err := tables.Select("t", nil); err != nil || len(rows) != 1 {
		t.Fatalf("rows after delete = %v, err %v", rows, err)
	}
}

func TestCompositePrimaryKeyDurableReload(t *testing.T) {
	kv, pool, schemas, tables := newTestSession(t)
	compositeTestTable(t, schemas)
	if err := tables.Insert("t", Row{"a": "x", "b": "y", "v": "one"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tables.Insert("t", Row{"a": "x", "b": "z", "v": "two"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var stored string
	if err := pool.WithClient(func(c *KVClient) error {
		var err error
		stored, err = c.Read("testdb:_schema:t")
		return err
	}); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if !strings.Contains(stored, `"primary_key_columns":["a","b"]`) {
		t.Fatalf("persisted schema = %s, want a primary_key_columns list", stored)
	}

	// A new manager pair keeps only the durable state, as after a restart.
	reloadedSchemas := NewSchemaManager(pool, "testdb")
	reloadedTables := NewTableManager(pool, reloadedSchemas, "testdb")

	schema, err := reloadedSchemas.GetSchema("t")
	if err != nil {
		t.Fatalf("get reloaded schema: %v", err)
	}
	if strings.Join(schema.PrimaryKeyList(), ",") != "a,b" || schema.PrimaryKey != "" {
		t.Fatalf("reloaded key = %q/%v, want a,b", schema.PrimaryKey, schema.PrimaryKeyList())
	}

	row, err := reloadedTables.GetByPK("t", "1:x1:y")
	if err != nil || row["v"] != "one" {
		t.Fatalf("reloaded row = %v, err %v", row, err)
	}
	rows, err := reloadedTables.Select("t", nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("reloaded select = %v, err %v", rows, err)
	}

	// Writes after the reload keep the same encoding.
	if err := reloadedTables.Insert("t", Row{"a": "x", "b": "y", "v": "again"}); err == nil {
		t.Fatal("expected a duplicate key to fail after reload")
	}
	if err := reloadedTables.Insert("t", Row{"a": "q", "b": "r", "v": "three"}); err != nil {
		t.Fatalf("insert after reload: %v", err)
	}
	if !kv.hasKey("testdb:_data:t:1:q1:r") {
		t.Fatal("insert after reload did not use the composite encoding")
	}
}

func TestSingleColumnPrimaryKeyEncodingUnchanged(t *testing.T) {
	kv, _, schemas, tables := newTestSession(t)

	if err := schemas.CreateTable(&Schema{
		Name:    "ints",
		Columns: []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}, {Name: "v", Type: "TEXT"}},
	}); err != nil {
		t.Fatalf("create ints: %v", err)
	}
	if err := tables.Insert("ints", Row{"id": int64(7), "v": "seven"}); err != nil {
		t.Fatalf("insert ints: %v", err)
	}
	if !kv.hasKey("testdb:_data:ints:7") {
		t.Fatal("INTEGER primary key row key changed from the bare value")
	}
	if row, err := tables.GetByPK("ints", "7"); err != nil || row["v"] != "seven" {
		t.Fatalf("GetByPK = %v, err %v", row, err)
	}

	if err := schemas.CreateTable(&Schema{
		Name:    "texts",
		Columns: []Column{{Name: "name", Type: "TEXT", PrimaryKey: true}},
	}); err != nil {
		t.Fatalf("create texts: %v", err)
	}
	if err := tables.Insert("texts", Row{"name": "alpha"}); err != nil {
		t.Fatalf("insert texts: %v", err)
	}
	if !kv.hasKey("testdb:_data:texts:alpha") {
		t.Fatal("TEXT primary key row key changed from the bare value")
	}

	var stored string
	if err := schemas.GetPool().WithClient(func(c *KVClient) error {
		var err error
		stored, err = c.Read("testdb:_schema:ints")
		return err
	}); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if !strings.Contains(stored, `"primary_key":"id"`) {
		t.Fatalf("persisted schema = %s, want the legacy primary_key field", stored)
	}
	if strings.Contains(stored, "primary_key_columns") {
		t.Fatalf("persisted schema = %s, want no composite field for a single-column key", stored)
	}
}

func TestLegacySingleColumnSchemaLoads(t *testing.T) {
	kv, pool, schemas, tables := newTestSession(t)

	// A catalog written before composite keys only has "primary_key".
	const legacy = `{"name":"legacy","columns":[{"name":"id","type":"INTEGER","nullable":false,"primary_key":true},{"name":"v","type":"TEXT","nullable":true}],"primary_key":"id","created_at":"2026-01-01T00:00:00Z","next_rowid":0,"autoincrement":false}`
	if err := pool.WithClient(func(c *KVClient) error {
		return c.Write("testdb:_schema:legacy", legacy)
	}); err != nil {
		t.Fatalf("write legacy schema: %v", err)
	}

	schema, err := schemas.GetSchema("legacy")
	if err != nil {
		t.Fatalf("get legacy schema: %v", err)
	}
	if schema.PrimaryKey != "id" || strings.Join(schema.PrimaryKeyList(), ",") != "id" {
		t.Fatalf("legacy key = %q/%v, want id", schema.PrimaryKey, schema.PrimaryKeyList())
	}

	if err := tables.Insert("legacy", Row{"id": int64(5), "v": "five"}); err != nil {
		t.Fatalf("insert into legacy table: %v", err)
	}
	if !kv.hasKey("testdb:_data:legacy:5") {
		t.Fatal("legacy INTEGER primary key did not use the bare-value key")
	}
	if row, err := tables.GetByPK("legacy", "5"); err != nil || row["v"] != "five" {
		t.Fatalf("GetByPK = %v, err %v", row, err)
	}
}

// Insert resolves catalog defaults for key components and writes every resolved
// key value back into the caller's row, so callers (RETURNING, conflict
// matching) observe the key the row was stored under.
func TestCompositePrimaryKeyInsertWritesResolvedKeyBack(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	if err := schemas.CreateTable(&Schema{
		Name:              "t",
		PrimaryKeyColumns: []string{"a", "b"},
		Columns: []Column{
			{Name: "a", Type: "TEXT", Default: "d"},
			{Name: "b", Type: "TEXT"},
			{Name: "v", Type: "TEXT"},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	row := Row{"b": "y", "v": "one"}
	if err := tables.Insert("t", row); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if row["a"] != "d" || row["b"] != "y" {
		t.Fatalf("caller row after insert = %v, want a='d' b='y'", row)
	}
	stored, err := tables.GetByPK("t", "1:d1:y")
	if err != nil || stored["v"] != "one" {
		t.Fatalf("stored row = %v, err %v", stored, err)
	}
}
