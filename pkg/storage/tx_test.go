package storage

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestSession(t *testing.T) (*testKVServer, *KVPool, *SchemaManager, *TableManager) {
	t.Helper()
	kv := newTestKVServer(t)
	pool := newTestKVPool(kv, 8, 5*time.Second)
	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")
	t.Cleanup(func() { pool.Close() })
	return kv, pool, schemas, tables
}

func createTestTable(t *testing.T, schemas *SchemaManager, name string, cols []Column) {
	t.Helper()
	if err := schemas.CreateTable(&Schema{Name: name, Columns: cols}); err != nil {
		t.Fatalf("create table %s: %v", name, err)
	}
}

func TestSessionReadYourWrites(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}, {Name: "v", Type: "TEXT"}})

	s := NewSession(schemas, tables)
	if err := s.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("t", Row{"id": int64(1), "v": "one"}); err != nil {
		t.Fatal(err)
	}
	// Point read observes the staged overlay before commit.
	row, err := s.GetByPK("t", "1")
	if err != nil || row["v"] != "one" {
		t.Fatalf("read-your-writes GetByPK: row=%v err=%v", row, err)
	}
	// Scan observes the staged overlay before commit.
	rows, err := s.Select("t", nil)
	if err != nil || len(rows) != 1 || rows[0]["v"] != "one" {
		t.Fatalf("read-your-writes Select: rows=%v err=%v", rows, err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	// Still visible from a fresh read after commit.
	if row, err := tables.GetByPK("t", "1"); err != nil || row["v"] != "one" {
		t.Fatalf("post-commit GetByPK: row=%v err=%v", row, err)
	}
}

func TestSessionIndexedReadTracksOnlyMatches(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "items", []Column{
		{Name: "id", Type: "INTEGER", PrimaryKey: true},
		{Name: "cart_id", Type: "TEXT"},
	})
	if err := schemas.CreateIndex(&Index{
		Name: "idx_items_cart", Table: "items",
		Columns: []IndexColumn{{Name: "cart_id"}},
	}); err != nil {
		t.Fatal(err)
	}
	rows := make([]Row, 50)
	for i := range rows {
		rows[i] = Row{"id": int64(i + 1), "cart_id": fmt.Sprintf("cart-%d", i)}
	}
	if _, err := tables.InsertBulk("items", rows); err != nil {
		t.Fatal(err)
	}
	if err := tables.BuildIndex("idx_items_cart", "items", []string{"cart_id"}); err != nil {
		t.Fatal(err)
	}

	s := NewSession(schemas, tables)
	if err := s.Begin(); err != nil {
		t.Fatal(err)
	}
	got, err := s.SelectByIndex("items", "idx_items_cart", "cart-37")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["id"] != int64(38) {
		t.Fatalf("indexed rows = %#v", got)
	}
	if len(s.reads) != 1 {
		t.Fatalf("indexed transaction captured %d row versions, want 1", len(s.reads))
	}
	if err := s.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionIndexedPredicateConflictsOnlyOnMatchingValue(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "items", []Column{
		{Name: "id", Type: "INTEGER", PrimaryKey: true},
		{Name: "cart_id", Type: "TEXT"},
	})
	if err := schemas.CreateIndex(&Index{
		Name: "idx_items_cart", Table: "items",
		Columns: []IndexColumn{{Name: "cart_id"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("items", Row{"id": int64(1), "cart_id": "cart-a"}); err != nil {
		t.Fatal(err)
	}
	if err := tables.BuildIndex("idx_items_cart", "items", []string{"cart_id"}); err != nil {
		t.Fatal(err)
	}

	unrelated := NewSession(schemas, tables)
	if err := unrelated.Begin(); err != nil {
		t.Fatal(err)
	}
	if _, err := unrelated.SelectByIndex("items", "idx_items_cart", "cart-a"); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("items", Row{"id": int64(2), "cart_id": "cart-b"}); err != nil {
		t.Fatal(err)
	}
	if err := unrelated.Commit(); err != nil {
		t.Fatalf("unrelated indexed insert caused conflict: %v", err)
	}

	matching := NewSession(schemas, tables)
	if err := matching.Begin(); err != nil {
		t.Fatal(err)
	}
	if _, err := matching.SelectByIndex("items", "idx_items_cart", "cart-c"); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("items", Row{"id": int64(3), "cart_id": "cart-c"}); err != nil {
		t.Fatal(err)
	}
	if err := matching.Commit(); err != ErrSerialization {
		t.Fatalf("matching indexed insert commit error = %v, want ErrSerialization", err)
	}
}

func TestSessionRollbackZeroDurableWrites(t *testing.T) {
	kv, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}})

	s := NewSession(schemas, tables)
	if err := s.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("t", Row{"id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, deleted, err := s.DeleteByPK("t", "1"); err != nil || !deleted {
		t.Fatalf("delete staged row: deleted=%v err=%v", deleted, err)
	}
	if err := s.Insert("t", Row{"id": int64(2)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := kv.countKeys("testdb:_data:t:"); got != 0 {
		t.Fatalf("rollback left %d durable rows", got)
	}
}

func TestSessionSavepoints(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}})

	s := NewSession(schemas, tables)
	if err := s.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("t", Row{"id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	sp := s.Snapshot()
	if err := s.Insert("t", Row{"id": int64(2)}); err != nil {
		t.Fatal(err)
	}
	s.RollbackTo(sp)
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := tables.GetByPK("t", "1"); err != nil {
		t.Fatalf("row 1 should be committed: %v", err)
	}
	if _, err := tables.GetByPK("t", "2"); err != ErrKeyNotFound {
		t.Fatalf("row 2 should be discarded, err=%v", err)
	}
}

func TestSessionAtomicMultiTableCommit(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "a", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}})
	createTestTable(t, schemas, "b", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}})

	s1 := NewSession(schemas, tables)
	s2 := NewSession(schemas, tables)

	if err := s1.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Insert("a", Row{"id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Insert("b", Row{"id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Insert("a", Row{"id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Insert("b", Row{"id": int64(2)}); err != nil {
		t.Fatal(err)
	}

	if err := s1.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Commit(); err != ErrSerialization {
		t.Fatalf("expected s2 commit to conflict, got %v", err)
	}

	// s1 committed both rows; s2 committed neither (atomic rollback).
	if _, err := tables.GetByPK("a", "1"); err != nil {
		t.Fatalf("a/1 missing: %v", err)
	}
	if _, err := tables.GetByPK("b", "1"); err != nil {
		t.Fatalf("b/1 missing: %v", err)
	}
	if _, err := tables.GetByPK("b", "2"); err != ErrKeyNotFound {
		t.Fatalf("b/2 should be absent after s2 conflict, err=%v", err)
	}
}

func TestSessionConflictingUpdateExactlyOneCommits(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "acct", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}, {Name: "bal", Type: "INTEGER"}})
	if err := tables.Insert("acct", Row{"id": int64(1), "bal": int64(100)}); err != nil {
		t.Fatal(err)
	}

	s1 := NewSession(schemas, tables)
	s2 := NewSession(schemas, tables)

	update := func(s *Session) (func() error, error) {
		if err := s.Begin(); err != nil {
			return nil, err
		}
		row, err := s.GetByPK("acct", "1")
		if err != nil {
			return nil, err
		}
		if _, _, err := s.UpdateByPK("acct", "1", func(Row) (Row, error) {
			return Row{"bal": row["bal"].(int64) + 10}, nil
		}); err != nil {
			return nil, err
		}
		return s.Commit, nil
	}

	c1, err := update(s1)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := update(s2)
	if err != nil {
		t.Fatal(err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = c1() }()
	go func() { defer wg.Done(); errs[1] = c2() }()
	wg.Wait()

	ok, conflict := 0, 0
	for _, e := range errs {
		if e == nil {
			ok++
		} else if e == ErrSerialization {
			conflict++
		} else {
			t.Fatalf("unexpected commit error: %v", e)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("expected exactly one commit and one conflict, got ok=%d conflict=%d", ok, conflict)
	}
	row, err := tables.GetByPK("acct", "1")
	if err != nil || row["bal"] != int64(110) {
		t.Fatalf("balance should be 110, got %v err=%v", row, err)
	}
}

func TestSessionNonConflictingConcurrentTransactions(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}, {Name: "v", Type: "TEXT"}})

	s1 := NewSession(schemas, tables)
	s2 := NewSession(schemas, tables)
	if err := s1.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Insert("t", Row{"id": int64(1), "v": "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Insert("t", Row{"id": int64(2), "v": "b"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = s1.Commit() }()
	go func() { defer wg.Done(); errs[1] = s2.Commit() }()
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("commit %d failed: %v", i, e)
		}
	}
	if rows, err := tables.Select("t", nil); err != nil || len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d err=%v", len(rows), err)
	}
}

func TestDuplicateInsertConcurrent(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}})

	const n = 32
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = tables.Insert("t", Row{"id": int64(1)})
		}()
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		} else if !(e != nil && fmt.Sprintf("%s", e) == "duplicate primary key: 1") {
			t.Fatalf("unexpected insert error: %v", e)
		}
	}
	if ok != 1 {
		t.Fatalf("expected exactly one successful insert, got %d", ok)
	}
	if got := kvCount(t, tables, "t"); got != 1 {
		t.Fatalf("expected 1 row, got %d", got)
	}
}

func kvCount(t *testing.T, tables *TableManager, table string) int {
	t.Helper()
	n, err := tables.CountFast(table)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPerTableConcurrentRowIDs(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "name", Type: "TEXT"}}) // _rowid_ PK

	const n = 100
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := tables.Insert("t", Row{"name": fmt.Sprintf("n%d", i)}); err != nil {
				errCh <- fmt.Errorf("insert %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	if rows, err := tables.Select("t", nil); err != nil || len(rows) != n {
		t.Fatalf("expected %d rows, got %d err=%v", n, len(rows), err)
	}
}

func TestPointOpsDifferentKeysProgressConcurrently(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "TEXT", PrimaryKey: true}, {Name: "v", Type: "INTEGER"}})

	const n = 50
	var wg sync.WaitGroup
	errCh := make(chan error, n*3)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i)
			if err := tables.Insert("t", Row{"id": key, "v": int64(i)}); err != nil {
				errCh <- fmt.Errorf("insert %s: %v", key, err)
				return
			}
			if _, _, err := tables.UpdateByPK("t", key, func(Row) (Row, error) { return Row{"v": int64(i + 100)}, nil }); err != nil {
				errCh <- fmt.Errorf("update %s: %v", key, err)
				return
			}
			if _, err := tables.GetByPK("t", key); err != nil {
				errCh <- fmt.Errorf("get %s: %v", key, err)
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("point operations on distinct keys did not progress concurrently")
	}
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
