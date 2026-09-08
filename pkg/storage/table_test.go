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

func TestPointUpdateDoesNotBlockPointReadBehindTableWriter(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{
		{Name: "id", Type: "INTEGER", PrimaryKey: true},
		{Name: "value", Type: "INTEGER"},
	})
	if err := tables.Insert("t", Row{"id": int64(1), "value": int64(1)}); err != nil {
		t.Fatal(err)
	}

	gate := tables.tableLock("t")
	gate.RLock()

	writerDone := make(chan error, 1)
	go func() {
		_, err := tables.Delete("t", func(Row) bool { return false })
		writerDone <- err
	}()

	deadline := time.Now().Add(time.Second)
	for gate.TryRLock() {
		gate.RUnlock()
		if time.Now().After(deadline) {
			gate.RUnlock()
			t.Fatal("table writer did not queue")
		}
		time.Sleep(time.Millisecond)
	}

	updateDone := make(chan error, 1)
	go func() {
		_, _, err := tables.UpdateByPK("t", "1", func(Row) (Row, error) {
			return Row{"value": int64(2)}, nil
		})
		updateDone <- err
	}()

	// Give the update time to reach the queued table gate. It must not hold the
	// row stripe while waiting, or this point read completes only after timeout.
	time.Sleep(10 * time.Millisecond)
	readDone := make(chan error, 1)
	go func() {
		_, err := tables.GetByPK("t", "1")
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err != nil {
			gate.RUnlock()
			t.Fatalf("point read: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		gate.RUnlock()
		<-writerDone
		<-updateDone
		<-readDone
		t.Fatal("point read deadlocked behind queued table writer")
	}

	gate.RUnlock()
	if err := <-writerDone; err != nil {
		t.Fatalf("table writer: %v", err)
	}
	if err := <-updateDone; err != nil {
		t.Fatalf("point update: %v", err)
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
	_, multiGetsBefore := kv.readStats()
	if rows, err := tables.SelectByIndex("items", "idx_kind", "k1"); err != nil || len(rows) != 10 {
		t.Fatalf("cached indexed select: len=%d err=%v", len(rows), err)
	}
	opensAfter, _, _ := kv.scanStats()
	_, multiGetsAfter := kv.readStats()
	if opensAfter != opensBefore {
		t.Fatalf("indexed select opened %d table scans after index build", opensAfter-opensBefore)
	}
	if multiGetsAfter-multiGetsBefore != 1 {
		t.Fatalf("indexed select issued %d multi-get requests, want 1", multiGetsAfter-multiGetsBefore)
	}
}

func TestPrimaryKeyMutationsDoNotScan(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	pool := newTestKVPool(kv, 4, 5*time.Second)
	defer pool.Close()
	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")
	if err := schemas.CreateTable(&Schema{
		Name: "items",
		Columns: []Column{
			{Name: "id", Type: "TEXT", PrimaryKey: true},
			{Name: "value", Type: "INTEGER"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := tables.Insert("items", Row{"id": fmt.Sprintf("item-%d", i), "value": int64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	opensBefore, _, _ := kv.scanStats()
	oldRow, updated, err := tables.UpdateByPK("items", "item-10", func(Row) (Row, error) {
		return Row{"value": int64(99)}, nil
	})
	if err != nil || !updated || oldRow["value"] != int64(10) {
		t.Fatalf("point update: updated=%v old=%v err=%v", updated, oldRow, err)
	}
	deletedRow, deleted, err := tables.DeleteByPK("items", "item-11")
	if err != nil || !deleted || deletedRow["value"] != int64(11) {
		t.Fatalf("point delete: deleted=%v old=%v err=%v", deleted, deletedRow, err)
	}
	opensAfter, _, _ := kv.scanStats()
	if opensAfter != opensBefore {
		t.Fatalf("primary-key mutations opened %d scans", opensAfter-opensBefore)
	}
	row, err := tables.GetByPK("items", "item-10")
	if err != nil || row["value"] != int64(99) {
		t.Fatalf("updated row=%v err=%v", row, err)
	}
	if _, err := tables.GetByPK("items", "item-11"); err != ErrKeyNotFound {
		t.Fatalf("deleted row error=%v, want ErrKeyNotFound", err)
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
