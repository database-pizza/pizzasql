package executor

import (
	"errors"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

func TestStorageScanUpdateCommitConflictHasNoPartialEffects(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	other := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (1,'old'),(2,'old')")
	raced := false
	count, err := table.UpdateFunc("t", func(storage.Row) (storage.Row, error) {
		if !raced {
			raced = true
			execMust(t, other, "UPDATE t SET v='external' WHERE id=2")
		}
		return storage.Row{"v": "staged"}, nil
	}, nil)
	if !errors.Is(err, storage.ErrSerialization) || count != 0 {
		t.Fatalf("update returned count=%d err=%v", count, err)
	}
	if got := execMust(t, e, "SELECT v FROM t WHERE id=1").Rows[0][0]; got != "old" {
		t.Fatalf("conflicted batch published earlier row: %v", got)
	}
	if got := execMust(t, e, "SELECT v FROM t WHERE id=2").Rows[0][0]; got != "external" {
		t.Fatalf("conflicted batch overwrote concurrent writer: %v", got)
	}
}
