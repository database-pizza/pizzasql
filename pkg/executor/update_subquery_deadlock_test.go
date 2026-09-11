package executor

import (
	"testing"
	"time"
)

// runWithTimeout runs fn and fails if it does not finish, which detects the
// organization-delete deadlock (UPDATE ... WHERE id IN (SELECT ...)).
func runWithTimeout(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("operation did not finish within %s (deadlock)", d)
	}
}

func TestUpdateWhereInSubqueryInTransaction(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	execMust(t, e, "CREATE TABLE repository (id INTEGER PRIMARY KEY, num_watches INTEGER)")
	execMust(t, e, "CREATE TABLE watch (id INTEGER PRIMARY KEY, user_id INTEGER, repo_id INTEGER)")
	execMust(t, e, "INSERT INTO repository VALUES (1, 5)")
	execMust(t, e, "INSERT INTO repository VALUES (2, 7)")
	execMust(t, e, "INSERT INTO watch VALUES (1, 1, 1)")

	execMust(t, e, "BEGIN")
	runWithTimeout(t, 5*time.Second, func() {
		execMust(t, e, "UPDATE repository SET num_watches = num_watches - 1 WHERE id IN (SELECT repo_id FROM watch WHERE user_id = 1)")
	})
	execMust(t, e, "COMMIT")

	res := execMust(t, e, "SELECT num_watches FROM repository WHERE id = 1")
	if res.Rows[0][0] != int64(4) {
		t.Fatalf("num_watches = %v, want 4", res.Rows[0][0])
	}
}

func TestDeleteWhereInSubqueryInTransaction(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	execMust(t, e, "CREATE TABLE repository (id INTEGER PRIMARY KEY, num_watches INTEGER)")
	execMust(t, e, "CREATE TABLE watch (id INTEGER PRIMARY KEY, user_id INTEGER, repo_id INTEGER)")
	execMust(t, e, "INSERT INTO repository VALUES (1, 5)")
	execMust(t, e, "INSERT INTO repository VALUES (2, 7)")
	execMust(t, e, "INSERT INTO watch VALUES (1, 1, 1)")

	execMust(t, e, "BEGIN")
	runWithTimeout(t, 5*time.Second, func() {
		execMust(t, e, "DELETE FROM repository WHERE id IN (SELECT repo_id FROM watch WHERE user_id = 1)")
	})
	execMust(t, e, "COMMIT")

	res := execMust(t, e, "SELECT id FROM repository")
	if res.RowCount != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("remaining rows = %v, want [2]", res.Rows)
	}
}
