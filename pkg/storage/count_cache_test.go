package storage

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestCountFastLifecycle verifies the COUNT(*) metadata counter stays exact
// across insert, update, delete, bulk insert, and truncate.
func TestCountFastLifecycle(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	err := schemas.CreateTable(&Schema{
		Name: "users",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "name", Type: "TEXT", Nullable: true},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	assertCount := func(want int) {
		t.Helper()
		got, err := tables.CountFast("users")
		if err != nil {
			t.Fatalf("CountFast: %v", err)
		}
		if got != want {
			t.Fatalf("CountFast = %d, want %d", got, want)
		}
	}

	assertCount(0)

	for i := int64(1); i <= 3; i++ {
		if err := tables.Insert("users", Row{"id": i, "name": fmt.Sprintf("u%d", i)}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	assertCount(3)

	// UPDATE does not change the count.
	n, err := tables.Update("users", Row{"name": "renamed"}, func(r Row) bool {
		return fmt.Sprintf("%v", r["id"]) == "1"
	})
	if err != nil || n != 1 {
		t.Fatalf("update: n=%d err=%v", n, err)
	}
	assertCount(3)

	// DELETE decrements.
	n, err = tables.Delete("users", func(r Row) bool {
		return fmt.Sprintf("%v", r["id"]) == "2"
	})
	if err != nil || n != 1 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
	assertCount(2)

	// Bulk insert adds len(rows).
	n, err = tables.InsertBulk("users", []Row{
		{"id": 4, "name": "u4"},
		{"id": 5, "name": "u5"},
		{"id": 6, "name": "u6"},
	})
	if err != nil || n != 3 {
		t.Fatalf("bulk insert: n=%d err=%v", n, err)
	}
	assertCount(5)

	// Truncate zeroes the count.
	if _, err := tables.Truncate("users"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	assertCount(0)
}

// TestCountFastDerivedAfterRestart verifies the counter is re-derived from
// durable rows when a fresh TableManager (process restart) has no in-memory
// count yet.
func TestCountFastDerivedAfterRestart(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	err := schemas.CreateTable(&Schema{
		Name: "events",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := int64(1); i <= 4; i++ {
		if err := tables.Insert("events", Row{"id": i}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	restartedSchemas := NewSchemaManager(pool, "testdb")
	restartedTables := NewTableManager(pool, restartedSchemas, "testdb")

	got, err := restartedTables.CountFast("events")
	if err != nil {
		t.Fatalf("CountFast after restart: %v", err)
	}
	if got != 4 {
		t.Fatalf("CountFast after restart = %d, want 4", got)
	}
}

// TestIncrementalCacheAndIndexMaintenance verifies that writes keep
// already-built in-memory indexes coherent without full-table invalidation.
func TestIncrementalCacheAndIndexMaintenance(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	err := schemas.CreateTable(&Schema{
		Name: "users",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "status", Type: "TEXT", Nullable: false},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	err = schemas.CreateIndex(&Index{
		Name:  "idx_users_status",
		Table: "users",
		Columns: []IndexColumn{
			{Name: "status"},
		},
	})
	if err != nil {
		t.Fatalf("create index: %v", err)
	}

	// Seed: 1 active, 1 inactive.
	if err := tables.Insert("users", Row{"id": 1, "status": "active"}); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if err := tables.Insert("users", Row{"id": 2, "status": "inactive"}); err != nil {
		t.Fatalf("insert 2: %v", err)
	}

	// Build the in-memory index (Select only streams rows).
	all, err := tables.Select("users", nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("initial select: len=%d err=%v", len(all), err)
	}
	active, err := tables.SelectByIndex("users", "idx_users_status", "active")
	if err != nil || len(active) != 1 {
		t.Fatalf("initial active index: len=%d err=%v", len(active), err)
	}

	// Bulk insert two more active rows; the index must reflect them.
	if n, err := tables.InsertBulk("users", []Row{
		{"id": 3, "status": "active"},
		{"id": 4, "status": "active"},
	}); err != nil || n != 2 {
		t.Fatalf("bulk insert: n=%d err=%v", n, err)
	}
	active, err = tables.SelectByIndex("users", "idx_users_status", "active")
	if err != nil || len(active) != 3 {
		t.Fatalf("active index after bulk: len=%d err=%v", len(active), err)
	}
	all, err = tables.Select("users", nil)
	if err != nil || len(all) != 4 {
		t.Fatalf("select after bulk: len=%d err=%v", len(all), err)
	}

	// Update one active -> inactive; both index buckets must stay coherent.
	if n, err := tables.Update("users", Row{"status": "inactive"}, func(r Row) bool {
		return fmt.Sprintf("%v", r["id"]) == "3"
	}); err != nil || n != 1 {
		t.Fatalf("update: n=%d err=%v", n, err)
	}
	active, _ = tables.SelectByIndex("users", "idx_users_status", "active")
	inactive, _ := tables.SelectByIndex("users", "idx_users_status", "inactive")
	if len(active) != 2 || len(inactive) != 2 {
		t.Fatalf("indexes after update: active=%d inactive=%d", len(active), len(inactive))
	}

	// Delete one row; cache and index must shrink.
	if n, err := tables.Delete("users", func(r Row) bool {
		return fmt.Sprintf("%v", r["id"]) == "4"
	}); err != nil || n != 1 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
	all, err = tables.Select("users", nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("select after delete: len=%d err=%v", len(all), err)
	}
	active, _ = tables.SelectByIndex("users", "idx_users_status", "active")
	if len(active) != 1 {
		t.Fatalf("active index after delete: len=%d", len(active))
	}
}

func TestClearIndexDropsInMemoryEntries(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")
	if err := schemas.CreateTable(&Schema{
		Name: "users",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "status", Type: "TEXT", Nullable: false},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := schemas.CreateIndex(&Index{Name: "idx_status", Table: "users", Columns: []IndexColumn{{Name: "status"}}}); err != nil {
		t.Fatalf("create index: %v", err)
	}
	if err := tables.Insert("users", Row{"id": 1, "status": "old"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if rows, err := tables.SelectByIndex("users", "idx_status", "old"); err != nil || len(rows) != 1 {
		t.Fatalf("build index: len=%d err=%v", len(rows), err)
	}
	if err := tables.ClearIndex("idx_status", "users", []string{"status"}); err != nil {
		t.Fatalf("clear index: %v", err)
	}

	tables.cacheMu.RLock()
	_, cached := tables.indexCache["idx_status"]
	_, mapped := tables.indexTable["idx_status"]
	tables.cacheMu.RUnlock()
	if cached || mapped {
		t.Fatalf("cleared index remains cached: cache=%v table=%v", cached, mapped)
	}
	if rows, err := tables.SelectByIndex("users", "idx_status", "old"); err != nil || len(rows) != 0 {
		t.Fatalf("cleared index rebuilt during drop: len=%d err=%v", len(rows), err)
	}

	if err := tables.BuildIndex("idx_status", "users", []string{"status"}); err != nil {
		t.Fatalf("rebuild index: %v", err)
	}
	if rows, err := tables.SelectByIndex("users", "idx_status", "old"); err != nil || len(rows) != 1 {
		t.Fatalf("rebuilt index: len=%d err=%v", len(rows), err)
	}
}

func TestConcurrentDuplicateInsertKeepsCountExact(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")
	if err := schemas.CreateTable(&Schema{
		Name:    "users",
		Columns: []Column{{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true}},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if got, err := tables.CountFast("users"); err != nil || got != 0 {
		t.Fatalf("initial count=%d err=%v", got, err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			errs <- tables.Insert("users", Row{"id": 1})
		}()
	}
	close(start)
	successes := 0
	for i := 0; i < 2; i++ {
		if err := <-errs; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful inserts=%d, want 1", successes)
	}
	if got, err := tables.CountFast("users"); err != nil || got != 1 {
		t.Fatalf("final count=%d err=%v, want 1", got, err)
	}
}

// TestConcurrentFirstLoadAndInsert runs a first scan (Select on a table)
// concurrently with inserts, then verifies the table ends up coherent with
// durable rows. Run under -race to detect map/slice data races.
func TestConcurrentFirstLoadAndInsert(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 16, 10*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	err := schemas.CreateTable(&Schema{
		Name: "users",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "name", Type: "TEXT", Nullable: true},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	const n = 200
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				if err := tables.Insert("users", Row{"id": int64(i + 1), "name": fmt.Sprintf("u%d", i)}); err != nil {
					t.Errorf("insert %d: %v", i, err)
					return
				}
			} else {
				if _, err := tables.Select("users", nil); err != nil {
					t.Errorf("select: %v", err)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	rows, err := tables.Select("users", nil)
	if err != nil {
		t.Fatalf("final select: %v", err)
	}
	if want := n / 2; len(rows) != want {
		t.Fatalf("final select = %d rows, want %d", len(rows), want)
	}
	if got, _ := tables.CountFast("users"); got != n/2 {
		t.Fatalf("final count = %d, want %d", got, n/2)
	}
}

// TestConcurrentFirstCountFastAndInsert runs first-time count derivation
// concurrently with inserts, then verifies the count ends up exact.
func TestConcurrentFirstCountFastAndInsert(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 16, 10*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	err := schemas.CreateTable(&Schema{
		Name: "items",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	const n = 200
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				if err := tables.Insert("items", Row{"id": int64(i + 1)}); err != nil {
					t.Errorf("insert %d: %v", i, err)
				}
			} else {
				if _, err := tables.CountFast("items"); err != nil {
					t.Errorf("count: %v", err)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if got, _ := tables.CountFast("items"); got != n/2 {
		t.Fatalf("final count = %d, want %d", got, n/2)
	}
	rows, err := tables.Select("items", nil)
	if err != nil {
		t.Fatalf("final select: %v", err)
	}
	if len(rows) != n/2 {
		t.Fatalf("final select = %d rows, want %d", len(rows), n/2)
	}
}
