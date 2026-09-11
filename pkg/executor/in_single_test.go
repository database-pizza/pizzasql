package executor

import "testing"

func TestInSingleValueNoMatch(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE unsplash_photos (id INTEGER PRIMARY KEY, file_id INTEGER)")
	execMust(t, e, "INSERT INTO unsplash_photos VALUES (1, 5)")
	res := execMust(t, e, "SELECT id, file_id FROM unsplash_photos WHERE file_id IN (0)")
	if res.RowCount != 0 {
		t.Fatalf("expected 0 rows, got %d: %v", res.RowCount, res.Rows)
	}
}
