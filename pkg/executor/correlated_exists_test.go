package executor

import (
	"testing"
	"time"
)

// TestCorrelatedExistsInWhere reproduces Vikunja's task filter: a single-table
// scan whose WHERE contains a correlated EXISTS. It must not deadlock.
func TestCorrelatedExistsInWhere(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE tasks (id INTEGER PRIMARY KEY, done INTEGER, deleted_at TEXT)")
	execMust(t, e, "CREATE TABLE task_assignees (task_id INTEGER, user_id INTEGER)")
	execMust(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT)")
	execMust(t, e, "INSERT INTO tasks VALUES (1, 0, NULL)")
	execMust(t, e, "INSERT INTO tasks VALUES (2, 0, NULL)")
	execMust(t, e, "INSERT INTO task_assignees VALUES (1, 10)")
	execMust(t, e, "INSERT INTO users VALUES (10, 'alice')")

	done := make(chan *Result, 1)
	go func() {
		res, err := execSQL(e, "SELECT tasks.id FROM tasks WHERE tasks.done = 0 AND EXISTS (SELECT 1 FROM task_assignees INNER JOIN users ON users.id = user_id WHERE (tasks.id = task_id) AND username IN ('alice'))")
		if err != nil {
			done <- nil
			return
		}
		done <- res
	}()

	select {
	case res := <-done:
		if res == nil {
			t.Fatal("correlated EXISTS query failed")
		}
		if res.RowCount != 1 || res.Rows[0][0] != int64(1) {
			t.Fatalf("expected only task 1, got %v", res.Rows)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("correlated EXISTS query deadlocked")
	}
}
