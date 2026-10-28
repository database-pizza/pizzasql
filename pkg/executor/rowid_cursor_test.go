package executor

import (
	"strconv"
	"sync"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

func rowIDs(t *testing.T, e *Executor, sql string) []int64 {
	t.Helper()
	res := execMust(t, e, sql)
	ids := make([]int64, 0, res.RowCount)
	for _, row := range res.Rows {
		id, ok := row[0].(int64)
		if !ok {
			t.Fatalf("rowid %v has type %T, want int64", row[0], row[0])
		}
		ids = append(ids, id)
	}
	return ids
}

func TestRowIDSelectableStableAcrossUpdateAndTypedBigint(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE items (id TEXT PRIMARY KEY, name TEXT)")
	execMust(t, e, "INSERT INTO items (id, name) VALUES ('a', 'a'), ('b', 'b'), ('c', 'c')")

	res := execMust(t, e, "SELECT _rowid_, rowid, oid FROM items ORDER BY _rowid_")
	if res.RowCount != 3 {
		t.Fatalf("row count = %d, want 3", res.RowCount)
	}
	if got := res.GetColumnType(0); got != "BIGINT" {
		t.Fatalf("_rowid_ type = %q, want BIGINT", got)
	}
	if got := res.Rows[0][0]; got != int64(1) {
		t.Fatalf("first _rowid_ = %v, want 1", got)
	}
	for i := 1; i < res.RowCount; i++ {
		if res.Rows[i][0].(int64) <= res.Rows[i-1][0].(int64) {
			t.Fatalf("_rowid_ is not monotonically increasing: %v", res.Rows)
		}
		if res.Rows[i][0] != res.Rows[i][1] || res.Rows[i][0] != res.Rows[i][2] {
			t.Fatalf("rowid/oid aliases disagree: %v", res.Rows[i])
		}
	}

	execMust(t, e, "UPDATE items SET name = 'updated' WHERE id = 'b'")
	execMust(t, e, "UPDATE items SET id = 'bb' WHERE id = 'b'")
	ids := rowIDs(t, e, "SELECT _rowid_ FROM items WHERE id = 'bb'")
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("rowid after UPDATE = %v, want [2]", ids)
	}
}

func TestRowIDExplicitColumnTakesPrecedence(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, rowid TEXT)")
	execMust(t, e, "INSERT INTO t (id, rowid) VALUES (1, 'explicit')")

	res := execMust(t, e, "SELECT rowid, _rowid_ FROM t")
	if got := res.Rows[0][0]; got != "explicit" {
		t.Fatalf("explicit rowid column = %v, want explicit", got)
	}
	if got := res.Rows[0][1]; got != int64(1) {
		t.Fatalf("hidden _rowid_ = %v, want 1", got)
	}
}

func TestRowIDAllocationSurvivesManagerRestart(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b'), (3, 'c')")

	// A new manager pair reads the durable schema and rowid allocator state.
	reopenedSchema := storage.NewSchemaManager(pool, "testdb")
	reopenedTable := storage.NewTableManager(pool, reopenedSchema, "testdb")
	reopened := newExec(reopenedSchema, reopenedTable)
	execMust(t, reopened, "INSERT INTO t (id, v) VALUES (4, 'd')")

	ids := rowIDs(t, reopened, "SELECT _rowid_ FROM t ORDER BY _rowid_")
	want := []int64{1, 2, 3, 4}
	if len(ids) != len(want) {
		t.Fatalf("rowids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("rowids = %v, want %v", ids, want)
		}
	}
}

func TestRowIDKeysetPaginationDoesNotSkipOrRepeat(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE events (id INTEGER PRIMARY KEY, payload TEXT)")
	for i := 1; i <= 5; i++ {
		execMust(t, e, "INSERT INTO events (id, payload) VALUES ("+strconv.Itoa(i)+", 'x')")
	}

	// Paginate with literals so the test exercises the executor filter and order
	// path directly; insert new rows between pages to prove the cursor stays
	// monotonic.
	var seen []int64
	cursor := int64(0)
	for {
		res := execMust(t, e, "SELECT _rowid_ FROM events WHERE _rowid_ > "+strconv.FormatInt(cursor, 10)+" ORDER BY _rowid_ LIMIT 3")
		if res.RowCount == 0 {
			break
		}
		for _, row := range res.Rows {
			id := row[0].(int64)
			if len(seen) > 0 && id <= seen[len(seen)-1] {
				t.Fatalf("rowid repeated or out of order: seen=%v next=%d", seen, id)
			}
			seen = append(seen, id)
		}
		cursor = seen[len(seen)-1]
		if cursor == 3 {
			// New rows arriving after the first page must appear later.
			execMust(t, e, "INSERT INTO events (id, payload) VALUES (100, 'late')")
		}
	}
	if len(seen) != 6 {
		t.Fatalf("paginated %v, want 6 rows including the late insert", seen)
	}
}

func TestRowIDUniqueAndMonotonicUnderConcurrentInsert(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")

	const workers, perWorker = 4, 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ids []int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := strconv.Itoa(worker*perWorker + i + 1)
				if _, err := execSQL(e, "INSERT INTO t (id, v) VALUES ("+id+", 'x')"); err != nil {
					t.Errorf("insert %s: %v", id, err)
					return
				}
				res, err := execSQL(e, "SELECT _rowid_ FROM t WHERE id = "+id)
				if err != nil {
					t.Errorf("select %s: %v", id, err)
					return
				}
				mu.Lock()
				ids = append(ids, res.Rows[0][0].(int64))
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate rowid %d", id)
		}
		seen[id] = true
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("got %d unique rowids, want %d", len(seen), workers*perWorker)
	}
}

// INTEGER PRIMARY KEY is SQLite's rowid alias: a row inserted with id=10 gets
// _rowid_=10. UPDATE never rewrites the stored _rowid_, so a keyset cursor is
// stable across value and primary-key updates even though the rowid is not
// independent of the key. Clients must not insert descending integer keys if
// they rely on rowid monotonicity.
func TestRowIDIntegerPrimaryKeyAliasIsStableAcrossUpdate(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	execMust(t, e, "INSERT INTO t (id, name) VALUES (10, 'a'), (20, 'b'), (30, 'c')")

	res := execMust(t, e, "SELECT id, _rowid_ FROM t ORDER BY id")
	for _, row := range res.Rows {
		if row[0] != row[1] {
			t.Fatalf("integer primary key %v does not alias _rowid_ %v", row[0], row[1])
		}
	}

	execMust(t, e, "UPDATE t SET id = 200, name = 'updated' WHERE id = 20")
	res = execMust(t, e, "SELECT id, _rowid_ FROM t WHERE id = 200")
	if got := res.Rows[0][1]; got != int64(20) {
		t.Fatalf("rowid after primary-key update = %v, want the original 20", got)
	}
}
