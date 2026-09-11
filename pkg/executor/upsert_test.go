package executor

import "testing"

// TestOnConflictUniqueIndexUpsert reproduces Vikunja's task_buckets upsert:
// INSERT ... ON CONFLICT (a, b) DO UPDATE SET col = excluded.col against a
// unique index that is not the primary key.
func TestOnConflictUniqueIndexUpsert(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE task_buckets (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id INTEGER, project_view_id INTEGER, bucket_id INTEGER)")
	execMust(t, e, "CREATE UNIQUE INDEX uq_tb ON task_buckets (task_id, project_view_id)")
	execMust(t, e, "INSERT INTO task_buckets (task_id, project_view_id, bucket_id) VALUES (1, 12, 9)")

	execMust(t, e, "INSERT INTO task_buckets (task_id, project_view_id, bucket_id) VALUES (1, 12, 7) ON CONFLICT (task_id, project_view_id) DO UPDATE SET bucket_id = excluded.bucket_id")

	res := execMust(t, e, "SELECT bucket_id FROM task_buckets WHERE task_id = 1 AND project_view_id = 12")
	if res.RowCount != 1 {
		t.Fatalf("expected 1 row, got %d: %v", res.RowCount, res.Rows)
	}
	if res.Rows[0][0] != int64(7) {
		t.Fatalf("bucket_id = %v, want 7", res.Rows[0][0])
	}

	// A different (task_id, project_view_id) inserts a new row.
	execMust(t, e, "INSERT INTO task_buckets (task_id, project_view_id, bucket_id) VALUES (2, 12, 5) ON CONFLICT (task_id, project_view_id) DO UPDATE SET bucket_id = excluded.bucket_id")
	res = execMust(t, e, "SELECT count(*) FROM task_buckets")
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("expected 2 rows, got %v", res.Rows[0][0])
	}
}
