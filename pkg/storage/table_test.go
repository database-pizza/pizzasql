package storage

import (
	"fmt"
	"testing"
	"time"
)

// TestSelectWithLimitStopsBeforeAllPages verifies that a limited scan stops
// consuming pages as soon as the limit is satisfied instead of reading the
// whole table.
func TestSelectWithLimitStopsBeforeAllPages(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 4, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	if err := schemas.CreateTable(&Schema{
		Name: "t",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "name", Type: "TEXT", Nullable: true},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	const n = 20
	for i := int64(1); i <= n; i++ {
		if err := tables.Insert("t", Row{"id": i, "name": fmt.Sprintf("n%d", i)}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// Force small pages so early termination is observable.
	kv.maxScanPage = 2

	_, nextsBefore, _ := kv.scanStats()
	rows, err := tables.SelectWithLimit("t", nil, 3, 0)
	if err != nil {
		t.Fatalf("SelectWithLimit: %v", err)
	}
	_, nextsAfter, _ := kv.scanStats()

	if len(rows) != 3 {
		t.Fatalf("SelectWithLimit returned %d rows, want 3", len(rows))
	}

	fullPages := (n + 1) / 2 // ceil(n / pageSize)
	if got := nextsAfter - nextsBefore; got >= fullPages {
		t.Fatalf("SelectWithLimit consumed %d pages, want < %d (should stop early)", got, fullPages)
	}
}

// TestDropTableDeletesDurableRows verifies that a direct DropTable removes all
// durable row keys, not just the schema entry.
func TestDropTableDeletesDurableRows(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 4, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	if err := schemas.CreateTable(&Schema{
		Name: "t",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	for i := int64(1); i <= 5; i++ {
		if err := tables.Insert("t", Row{"id": i}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if got := kv.countKeys("testdb:_data:t:"); got != 5 {
		t.Fatalf("expected 5 durable rows before drop, got %d", got)
	}

	if err := schemas.DropTable("t"); err != nil {
		t.Fatalf("DropTable: %v", err)
	}

	if got := kv.countKeys("testdb:_data:t:"); got != 0 {
		t.Fatalf("expected 0 durable rows after drop, got %d", got)
	}
	if kv.hasKey("testdb:_schema:t") {
		t.Fatalf("schema key still present after drop")
	}
	if kv.hasKey("testdb:_sys:rowid:t") {
		t.Fatalf("rowid counter key still present after drop")
	}
}

// TestScanKeysReturnsKeysOnly verifies that the key-only scan constructor
// returns keys with empty values.
func TestScanKeysReturnsKeysOnly(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	c := kv.client()
	defer c.Close()

	if _, err := c.Put([]byte("p:a"), []byte("value-a")); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if _, err := c.Put([]byte("p:b"), []byte("value-b")); err != nil {
		t.Fatalf("put b: %v", err)
	}

	scan, err := c.ScanKeys([]byte("p:"))
	if err != nil {
		t.Fatalf("ScanKeys: %v", err)
	}
	defer scan.Close()

	entries, done, err := scan.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !done || len(entries) != 2 {
		t.Fatalf("done=%v len=%d, want done and 2 entries", done, len(entries))
	}
	for _, e := range entries {
		if len(e.Key) == 0 {
			t.Fatalf("expected non-empty key")
		}
		if len(e.Value) != 0 {
			t.Fatalf("key-only scan returned a value %q for key %q", e.Value, e.Key)
		}
	}
}

// TestCountFastFirstDerivationUsesKeyOnlyScan verifies that the first-time
// COUNT(*) derivation issues a key-only scan rather than pulling row values.
func TestCountFastFirstDerivationUsesKeyOnlyScan(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 4, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	if err := schemas.CreateTable(&Schema{
		Name: "t",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	for i := int64(1); i <= 5; i++ {
		if err := tables.Insert("t", Row{"id": i}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	before := kv.keyOnlyOpenCount()
	got, err := tables.CountFast("t")
	if err != nil {
		t.Fatalf("CountFast: %v", err)
	}
	after := kv.keyOnlyOpenCount()

	if got != 5 {
		t.Fatalf("CountFast = %d, want 5", got)
	}
	if after-before != 1 {
		t.Fatalf("expected CountFast first derivation to use one key-only scan, got %d", after-before)
	}
}

func TestInsertBulkStringPrimaryKeyCount(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 4, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")
	if err := schemas.CreateTable(&Schema{
		Name: "labels",
		Columns: []Column{
			{Name: "id", Type: "TEXT", Nullable: false, PrimaryKey: true},
			{Name: "value", Type: "BLOB", Nullable: true},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	n, err := tables.InsertBulk("labels", []Row{
		{"id": "a", "value": []byte{0, 1, 2}},
		{"id": "b", "value": []byte{'|', '\r', '\n'}},
	})
	if err != nil {
		t.Fatalf("InsertBulk: %v", err)
	}
	if n != 2 {
		t.Fatalf("InsertBulk count = %d, want 2", n)
	}
	rows, err := tables.Select("labels", nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Select returned %d rows, want 2", len(rows))
	}
	if n, err := tables.InsertBulk("labels", []Row{{"id": "a", "value": "duplicate"}}); err == nil || n != 0 {
		t.Fatalf("existing duplicate: n=%d err=%v", n, err)
	}
	if n, err := tables.InsertBulk("labels", []Row{{"id": "c"}, {"id": "c"}}); err == nil || n != 0 {
		t.Fatalf("batch duplicate: n=%d err=%v", n, err)
	}
	if _, err := tables.GetByPK("labels", "c"); err == nil {
		t.Fatal("duplicate batch persisted a row")
	}
}

func TestSelectByIndexUsesPointReadsAfterBuild(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	pool := newTestKVPool(kv, 4, 5*time.Second)
	defer pool.Close()
	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")
	if err := schemas.CreateTable(&Schema{
		Name: "items",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", PrimaryKey: true},
			{Name: "kind", Type: "TEXT"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := schemas.CreateIndex(&Index{Name: "idx_kind", Table: "items", Columns: []IndexColumn{{Name: "kind"}}}); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 20; i++ {
		if err := tables.Insert("items", Row{"id": i, "kind": fmt.Sprintf("k%d", i%2)}); err != nil {
			t.Fatal(err)
		}
	}
	if rows, err := tables.SelectByIndex("items", "idx_kind", "k1"); err != nil || len(rows) != 10 {
		t.Fatalf("initial indexed select: len=%d err=%v", len(rows), err)
	}
	opensBefore, _, _ := kv.scanStats()
	if rows, err := tables.SelectByIndex("items", "idx_kind", "k1"); err != nil || len(rows) != 10 {
		t.Fatalf("cached indexed select: len=%d err=%v", len(rows), err)
	}
	opensAfter, _, _ := kv.scanStats()
	if opensAfter != opensBefore {
		t.Fatalf("indexed select opened %d table scans after index build", opensAfter-opensBefore)
	}
}

func TestCountFastResetsAfterDirectDropAndRecreate(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()
	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")
	create := func() {
		if err := schemas.CreateTable(&Schema{Name: "events", Columns: []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}}}); err != nil {
			t.Fatal(err)
		}
	}
	create()
	if err := tables.Insert("events", Row{"id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if count, err := tables.CountFast("events"); err != nil || count != 1 {
		t.Fatalf("initial count=%d err=%v", count, err)
	}
	if err := schemas.DropTable("events"); err != nil {
		t.Fatal(err)
	}
	create()
	if count, err := tables.CountFast("events"); err != nil || count != 0 {
		t.Fatalf("recreated count=%d err=%v", count, err)
	}
}
