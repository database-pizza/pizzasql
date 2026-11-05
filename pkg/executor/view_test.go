package executor

import "testing"

// A view joined to another table must keep the join when the view expands to a
// derived table. Dropping the join returned rows from the view alone (or failed
// to resolve the joined table) instead of the joined result.
func TestViewJoinIsPreserved(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	execMust(t, e, `CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER, amount INTEGER)`)
	execMust(t, e, `INSERT INTO users VALUES (1, 'ada'), (2, 'grace')`)
	execMust(t, e, `INSERT INTO orders VALUES (10, 1, 50), (11, 1, 70), (12, 2, 30)`)
	execMust(t, e, `CREATE VIEW order_view AS SELECT user_id, amount FROM orders`)

	res := execMust(t, e, `SELECT u.name, v.amount FROM order_view v JOIN users u ON u.id = v.user_id ORDER BY v.amount`)
	want := [][]interface{}{{"grace", int64(30)}, {"ada", int64(50)}, {"ada", int64(70)}}
	if len(res.Rows) != len(want) {
		t.Fatalf("joined view rows = %v, want %v", res.Rows, want)
	}
	for i := range want {
		if len(res.Rows[i]) != 2 || res.Rows[i][0] != want[i][0] || res.Rows[i][1] != want[i][1] {
			t.Fatalf("joined view rows = %v, want %v", res.Rows, want)
		}
	}
}
