package storage

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func uniqueTable(t *testing.T, cols ...Column) (*SchemaManager, *TableManager) {
	t.Helper()
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", cols)
	return schemas, tables
}

func TestInsertWithRowIDReturnsActualRowID(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}})

	rid, err := tables.InsertWithRowID("t", Row{"id": int64(41)})
	if err != nil {
		t.Fatal(err)
	}
	if rid != 41 {
		t.Fatalf("explicit rowid = %d, want 41", rid)
	}
	// Auto-generated rowid is max+1, never a naive counter or MAX of a scan.
	rid, err = tables.InsertWithRowID("t", Row{})
	if err != nil {
		t.Fatal(err)
	}
	if rid != 42 {
		t.Fatalf("generated rowid = %d, want 42", rid)
	}
}

func TestInsertWithRowIDSkipsGaps(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}})

	for _, id := range []int64{100, 5, 7} {
		if _, err := tables.InsertWithRowID("t", Row{"id": id}); err != nil {
			t.Fatal(err)
		}
	}
	rid, err := tables.InsertWithRowID("t", Row{})
	if err != nil {
		t.Fatal(err)
	}
	if rid != 101 {
		t.Fatalf("generated rowid = %d, want 101 (max+1, not a re-used gap)", rid)
	}
}

func TestUniqueIndexEnforcesOnInsert(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "email": "a@x"}); err != nil {
		t.Fatal(err)
	}
	err := tables.Insert("t", Row{"id": int64(2), "email": "a@x"})
	if err == nil {
		t.Fatal("expected unique violation on duplicate email")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	// A distinct value still works.
	if err := tables.Insert("t", Row{"id": int64(2), "email": "b@x"}); err != nil {
		t.Fatalf("distinct email should succeed: %v", err)
	}
}

func TestUniqueIndexAllowsMultipleNulls(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT", Nullable: true},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}}); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		if err := tables.Insert("t", Row{"id": i, "email": nil}); err != nil {
			t.Fatalf("NULL insert %d should succeed: %v", i, err)
		}
	}
	if err := tables.Insert("t", Row{"id": int64(4), "email": "a@x"}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(5), "email": "a@x"}); err == nil {
		t.Fatal("expected unique violation for non-NULL duplicate")
	}
}

func TestUniqueCompositeIndex(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "a", Type: "TEXT"},
		Column{Name: "b", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_ab", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "a"}, {Name: "b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "a": "x", "b": "y"}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(2), "a": "x", "b": "z"}); err != nil {
		t.Fatalf("distinct composite should succeed: %v", err)
	}
	if err := tables.Insert("t", Row{"id": int64(3), "a": "x", "b": "y"}); err == nil {
		t.Fatal("expected unique violation for duplicate composite (x,y)")
	}
}

func TestCreateUniqueIndexRejectsExistingDuplicates(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT"},
	)
	for i := int64(1); i <= 2; i++ {
		if err := tables.Insert("t", Row{"id": i, "email": "dup@x"}); err != nil {
			t.Fatal(err)
		}
	}
	err := tables.CreateUniqueIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}})
	if err == nil {
		t.Fatal("expected CreateUniqueIndex to reject existing duplicates")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The index must not be registered after the failed create.
	if schemas.IndexExists("uq_email") {
		t.Fatal("index should not exist after validation failure")
	}
}

func TestUniqueIndexEnforcesOnUpdate(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "email": "a@x"}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(2), "email": "b@x"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := tables.UpdateByPK("t", "2", func(Row) (Row, error) { return Row{"email": "a@x"}, nil })
	if err == nil {
		t.Fatal("expected unique violation when updating email to existing value")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUniqueIndexFreesOnDelete(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "email": "a@x"}); err != nil {
		t.Fatal(err)
	}
	if _, deleted, err := tables.DeleteByPK("t", "1"); err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if err := tables.Insert("t", Row{"id": int64(2), "email": "a@x"}); err != nil {
		t.Fatalf("re-insert after delete should succeed: %v", err)
	}
}

func TestUniqueIndexTransactionCommitRejectsDuplicate(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "email": "a@x"}); err != nil {
		t.Fatal(err)
	}

	s := NewSession(schemas, tables)
	if err := s.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("t", Row{"id": int64(2), "email": "a@x"}); err != nil {
		t.Fatalf("staged duplicate insert should not fail until commit: %v", err)
	}
	if err := s.Commit(); err == nil {
		t.Fatal("expected commit to reject unique violation")
	} else if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("unexpected commit error: %v", err)
	}
	if _, err := tables.GetByPK("t", "2"); err != ErrKeyNotFound {
		t.Fatalf("conflicting row should not be durable: %v", err)
	}
}

func TestUniqueIndexTransactionSwap(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "email": "a@x"}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(2), "email": "b@x"}); err != nil {
		t.Fatal(err)
	}

	s := NewSession(schemas, tables)
	if err := s.Begin(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpdateByPK("t", "1", func(Row) (Row, error) { return Row{"email": "b@x"}, nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpdateByPK("t", "2", func(Row) (Row, error) { return Row{"email": "a@x"}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("swap of unique values should commit: %v", err)
	}
}

func TestUniqueIndexConcurrentInserts(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}}); err != nil {
		t.Fatal(err)
	}

	const n = 32
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = tables.Insert("t", Row{"id": int64(i + 1), "email": "same@x"})
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		} else if !strings.Contains(e.Error(), "UNIQUE constraint failed") {
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

func TestUniqueIndexConcurrentTransactions(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}}); err != nil {
		t.Fatal(err)
	}

	s1 := NewSession(schemas, tables)
	s2 := NewSession(schemas, tables)
	if err := s1.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Insert("t", Row{"id": int64(1), "email": "x@y"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Insert("t", Row{"id": int64(2), "email": "x@y"}); err != nil {
		t.Fatal(err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = s1.Commit() }()
	go func() { defer wg.Done(); errs[1] = s2.Commit() }()
	wg.Wait()

	ok, conflict := 0, 0
	for _, e := range errs {
		if e == nil {
			ok++
		} else if strings.Contains(e.Error(), "UNIQUE constraint failed") {
			conflict++
		} else {
			t.Fatalf("unexpected commit error: %v", e)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("expected one commit and one unique conflict, got ok=%d conflict=%d", ok, conflict)
	}
	if got := kvCount(t, tables, "t"); got != 1 {
		t.Fatalf("expected 1 durable row, got %d", got)
	}
}

func TestUniqueIndexUpdateNoChangeDoesNotSelfConflict(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT"},
		Column{Name: "name", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "email": "a@x", "name": "old"}); err != nil {
		t.Fatal(err)
	}
	// Update a non-indexed column; the unchanged unique value must not conflict.
	if _, updated, err := tables.UpdateByPK("t", "1", func(Row) (Row, error) { return Row{"name": "new"}, nil }); err != nil || !updated {
		t.Fatalf("no-op unique update: updated=%v err=%v", updated, err)
	}
}

func TestUniqueIndexValueEncodingDistinguishesTypes(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "v", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_v", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "v"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "v": int64(1)}); err != nil {
		t.Fatal(err)
	}
	// INTEGER 1 and TEXT "1" are distinct under a unique index.
	if err := tables.Insert("t", Row{"id": int64(2), "v": "1"}); err != nil {
		t.Fatalf("TEXT '1' should be distinct from INTEGER 1: %v", err)
	}
	if err := tables.Insert("t", Row{"id": int64(3), "v": int64(1)}); err == nil {
		t.Fatal("expected duplicate INTEGER 1 to be rejected")
	}
}

// TestConcurrentCreateUniqueIndexAndInsert races a duplicate insert against a
// CREATE UNIQUE INDEX. The invariant is that the unique index can never end up
// present while two rows share a value: either the index creation wins and the
// insert fails the uniqueness scan, or the insert wins and the index creation
// fails validating the existing duplicate.
func TestConcurrentCreateUniqueIndexAndInsert(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		_, _, schemas, tables := newTestSession(t)
		createTestTable(t, schemas, "t", []Column{
			{Name: "id", Type: "INTEGER", PrimaryKey: true},
			{Name: "v", Type: "TEXT"},
		})
		if err := tables.Insert("t", Row{"id": int64(1), "v": "x"}); err != nil {
			t.Fatal(err)
		}

		var insertErr, indexErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			insertErr = tables.Insert("t", Row{"id": int64(2), "v": "x"})
		}()
		go func() {
			defer wg.Done()
			<-start
			indexErr = tables.CreateUniqueIndex(&Index{Name: "uq_v", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "v"}}})
		}()
		close(start)
		wg.Wait()

		if schemas.IndexExists("uq_v") {
			rows, err := tables.Select("t", func(r Row) bool { return fmt.Sprintf("%v", r["v"]) == "x" })
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("iteration %d: unique index present but %d rows with v='x' (insertErr=%v indexErr=%v)", iter, len(rows), insertErr, indexErr)
			}
		}
	}
}

// TestConcurrentCreateUniqueIndexAndTransactionCommit exercises the same race
// against a buffered transaction whose commit validates the final overlay.
func TestConcurrentCreateUniqueIndexAndTransactionCommit(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		_, _, schemas, tables := newTestSession(t)
		createTestTable(t, schemas, "t", []Column{
			{Name: "id", Type: "INTEGER", PrimaryKey: true},
			{Name: "v", Type: "TEXT"},
		})
		if err := tables.Insert("t", Row{"id": int64(1), "v": "x"}); err != nil {
			t.Fatal(err)
		}

		s := NewSession(schemas, tables)
		if err := s.Begin(); err != nil {
			t.Fatal(err)
		}
		if err := s.Insert("t", Row{"id": int64(2), "v": "x"}); err != nil {
			t.Fatal(err)
		}

		var commitErr, indexErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			commitErr = s.Commit()
		}()
		go func() {
			defer wg.Done()
			<-start
			indexErr = tables.CreateUniqueIndex(&Index{Name: "uq_v", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "v"}}})
		}()
		close(start)
		wg.Wait()

		if schemas.IndexExists("uq_v") {
			rows, err := tables.Select("t", func(r Row) bool { return fmt.Sprintf("%v", r["v"]) == "x" })
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("iteration %d: unique index present but %d rows with v='x' (commitErr=%v indexErr=%v)", iter, len(rows), commitErr, indexErr)
			}
		}
	}
}

func TestUniqueIndexIntegralNumericCanonicalization(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "v", Type: "REAL"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_v", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "v"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "v": int64(1)}); err != nil {
		t.Fatal(err)
	}
	// A computed integral real (1.5-0.5) must collide with the integer 1.
	err := tables.Insert("t", Row{"id": int64(2), "v": 1.5 - 0.5})
	if err == nil {
		t.Fatal("expected float64(1.0) to collide with int64(1)")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	// A non-integral real remains distinct.
	if err := tables.Insert("t", Row{"id": int64(3), "v": 1.5}); err != nil {
		t.Fatalf("distinct non-integral real should succeed: %v", err)
	}
}

func TestUniqueIndexUnsignedCanonicalization(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "v", Type: "INTEGER"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_v", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "v"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": int64(1), "v": uint64(7)}); err != nil {
		t.Fatal(err)
	}
	// unsigned 7 and signed 7 are the same integer value.
	if err := tables.Insert("t", Row{"id": int64(2), "v": int64(7)}); err == nil {
		t.Fatal("expected uint64(7) to collide with int64(7)")
	}
}

func TestUniqueIndexDeleteReinsertTextPK(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "TEXT", PrimaryKey: true},
		Column{Name: "email", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "uq_email", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "email"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.Insert("t", Row{"id": "alice", "email": "a@x"}); err != nil {
		t.Fatal(err)
	}

	// Deleting and re-inserting the same TEXT primary key with the same unique
	// value must not self-conflict: the re-insert is a new rowid but replaces the
	// same durable data key.
	s := NewSession(schemas, tables)
	if err := s.Begin(); err != nil {
		t.Fatal(err)
	}
	if _, deleted, err := s.DeleteByPK("t", "alice"); err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if err := s.Insert("t", Row{"id": "alice", "email": "a@x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("delete+reinsert same PK should commit, got: %v", err)
	}
	rows, err := tables.Select("t", nil)
	if err != nil || len(rows) != 1 || rows[0]["email"] != "a@x" {
		t.Fatalf("expected exactly one row with email a@x, got %v (err=%v)", rows, err)
	}
}

func TestUniqueIndexesSameValueDifferentIndexes(t *testing.T) {
	schemas, tables := uniqueTable(t,
		Column{Name: "id", Type: "INTEGER", PrimaryKey: true},
		Column{Name: "name", Type: "TEXT"},
		Column{Name: "lower_name", Type: "TEXT"},
	)
	if err := schemas.CreateIndex(&Index{Name: "UQE_user_name", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "name"}}}); err != nil {
		t.Fatal(err)
	}
	if err := schemas.CreateIndex(&Index{Name: "UQE_user_lower_name", Table: "t", Unique: true, Columns: []IndexColumn{{Name: "lower_name"}}}); err != nil {
		t.Fatal(err)
	}

	// A single row carrying the same value in two different unique indexes must
	// not self-collide (the seen set is per-index, not per-value).
	if err := tables.Insert("t", Row{"id": int64(1), "name": "alice", "lower_name": "alice"}); err != nil {
		t.Fatalf("same value across two unique indexes should not self-collide: %v", err)
	}
	// A second row with the same value in ONE index must still collide.
	if err := tables.Insert("t", Row{"id": int64(2), "name": "bob", "lower_name": "alice"}); err == nil {
		t.Fatal("expected unique violation on lower_name")
	}
	if err := tables.Insert("t", Row{"id": int64(2), "name": "alice", "lower_name": "bob"}); err == nil {
		t.Fatal("expected unique violation on name")
	}
	if err := tables.Insert("t", Row{"id": int64(2), "name": "bob", "lower_name": "bob"}); err != nil {
		t.Fatalf("distinct values should insert: %v", err)
	}
}
