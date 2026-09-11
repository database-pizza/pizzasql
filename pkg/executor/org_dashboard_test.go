package executor

import "testing"

// TestGogsOrganizationDashboardJoin reproduces the exact SQL Gogs emits for the
// dashboard "switch context" organization list.
func TestGogsOrganizationDashboardJoin(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	execMust(t, e, "CREATE TABLE `user` (id INTEGER PRIMARY KEY, lower_name TEXT, name TEXT, type INTEGER)")
	execMust(t, e, "CREATE TABLE org_user (id INTEGER PRIMARY KEY, uid INTEGER, org_id INTEGER, is_public INTEGER, is_owner INTEGER, num_teams INTEGER)")
	execMust(t, e, "INSERT INTO `user` VALUES (1, 'danfragoso', 'danfragoso', 0)")
	execMust(t, e, "INSERT INTO `user` VALUES (2, 'database.pizza', 'database.pizza', 1)")
	execMust(t, e, "INSERT INTO org_user VALUES (1, 1, 2, 0, 1, 1)")

	t.Run("gorm_raw_select_star", func(t *testing.T) {
		res := execMust(t, e, "SELECT * FROM `user` JOIN org_user ON org_user.org_id = user.id WHERE org_user.uid = 1 ORDER BY user.id ASC")
		t.Logf("columns=%v rows=%d", res.Columns, res.RowCount)
		if res.RowCount != 1 {
			t.Fatalf("expected 1 row, got %d", res.RowCount)
		}
	})

	t.Run("qualified_user_star", func(t *testing.T) {
		res := execMust(t, e, "SELECT user.* FROM user JOIN org_user ON org_user.org_id = user.id WHERE org_user.uid = 1 ORDER BY user.id ASC")
		t.Logf("columns=%v rows=%d", res.Columns, res.RowCount)
		if res.RowCount != 1 {
			t.Fatalf("expected 1 row, got %d", res.RowCount)
		}
	})

	t.Run("quoted_user_join", func(t *testing.T) {
		res := execMust(t, e, "SELECT * FROM `user` JOIN org_user ON org_user.org_id = `user`.id WHERE org_user.uid = 1 ORDER BY `user`.id ASC")
		t.Logf("columns=%v rows=%d", res.Columns, res.RowCount)
		if res.RowCount != 1 {
			t.Fatalf("expected 1 row, got %d", res.RowCount)
		}
	})

	t.Run("left_join_where_on_joined_table", func(t *testing.T) {
		res := execMust(t, e, "SELECT user.* FROM user LEFT JOIN org_user ON org_user.org_id = user.id WHERE org_user.uid = 1")
		if res.RowCount != 1 {
			t.Fatalf("expected 1 row, got %d", res.RowCount)
		}
		if res.Rows[0][1] != "database.pizza" {
			t.Fatalf("expected organization row, got %v", res.Rows[0])
		}
	})

	t.Run("left_join_where_on_left_table_still_works", func(t *testing.T) {
		res := execMust(t, e, "SELECT user.* FROM user LEFT JOIN org_user ON org_user.org_id = user.id WHERE user.id = 1")
		if res.RowCount != 1 {
			t.Fatalf("expected 1 row, got %d", res.RowCount)
		}
		if res.Rows[0][1] != "danfragoso" {
			t.Fatalf("expected individual row, got %v", res.Rows[0])
		}
	})
}
