package executor

import (
	"fmt"
	"sync"
	"testing"
)

// A child insert and a parent delete that run concurrently must never both
// commit, and the table set must never contain a dangling child reference.
// Both statements take the foreign-key write gate, so exactly one wins; if that
// ever regresses, the loser is not a foreign-key violation or the invariant scan
// finds an orphan.
func TestForeignKeyConcurrentChildInsertAndParentDelete(t *testing.T) {
	_, schema, table := newTestDB(t)
	inserter := newExec(schema, table)
	deleter := newExec(schema, table)
	execMust(t, inserter, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, inserter, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")

	const rounds = 100
	for round := 1; round <= rounds; round++ {
		execMust(t, inserter, fmt.Sprintf("INSERT INTO parent (id) VALUES (%d)", round))

		start := make(chan struct{})
		insertDone := make(chan error, 1)
		deleteDone := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := execSQL(inserter, fmt.Sprintf("INSERT INTO child (id, parent_id) VALUES (%d, %d)", round, round))
			insertDone <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := execSQL(deleter, fmt.Sprintf("DELETE FROM parent WHERE id = %d", round))
			deleteDone <- err
		}()
		close(start)
		wg.Wait()
		insertErr := <-insertDone
		deleteErr := <-deleteDone

		switch {
		case insertErr == nil && deleteErr == nil:
			t.Fatalf("round %d: child insert and parent delete both committed", round)
		case insertErr == nil:
			requireForeignKeyViolation(t, deleteErr)
		case deleteErr == nil:
			requireForeignKeyViolation(t, insertErr)
		default:
			t.Fatalf("round %d: both statements failed: insert=%v delete=%v", round, insertErr, deleteErr)
		}

		dangling := execMust(t, inserter, "SELECT count(*) FROM child c WHERE NOT EXISTS (SELECT 1 FROM parent p WHERE p.id = c.parent_id)")
		if dangling.Rows[0][0] != int64(0) {
			t.Fatalf("round %d: %v dangling child rows", round, dangling.Rows[0][0])
		}

		execMust(t, inserter, "DELETE FROM child")
		execMust(t, inserter, fmt.Sprintf("DELETE FROM parent WHERE id = %d", round))
	}
}
