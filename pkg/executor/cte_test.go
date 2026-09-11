package executor

import "testing"

// TestNonRecursiveCTE reproduces the shape of Vikunja's project-permission
// query: a CTE with a column list over a UNION ALL subquery, joined back to a
// real table and grouped.
func TestNonRecursiveCTE(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	execMust(t, e, "CREATE TABLE projects (id INTEGER PRIMARY KEY, owner_id INTEGER)")
	execMust(t, e, "CREATE TABLE users_projects (project_id INTEGER, user_id INTEGER, permission INTEGER)")
	execMust(t, e, "CREATE TABLE team_projects (project_id INTEGER, team_id INTEGER, permission INTEGER)")
	execMust(t, e, "CREATE TABLE team_members (team_id INTEGER, user_id INTEGER)")
	execMust(t, e, "CREATE TABLE project_ancestors (ancestor_id INTEGER, project_id INTEGER, depth INTEGER)")

	execMust(t, e, "INSERT INTO projects VALUES (1, 3)")
	execMust(t, e, "INSERT INTO projects VALUES (2, 9)")
	execMust(t, e, "INSERT INTO projects VALUES (3, 9)")
	execMust(t, e, "INSERT INTO users_projects VALUES (2, 3, 1)")
	execMust(t, e, "INSERT INTO team_projects VALUES (3, 7, 2)")
	execMust(t, e, "INSERT INTO team_members VALUES (7, 3)")
	execMust(t, e, "INSERT INTO project_ancestors VALUES (1, 1, 0)")
	execMust(t, e, "INSERT INTO project_ancestors VALUES (2, 2, 0)")
	execMust(t, e, "INSERT INTO project_ancestors VALUES (3, 3, 0)")

	res := execMust(t, e, `WITH grants (project_id, permission) AS (
		SELECT project_id, MAX(permission) FROM (
			SELECT id AS project_id, 2 AS permission FROM projects WHERE owner_id = 3
			UNION ALL SELECT project_id, permission FROM users_projects WHERE user_id = 3
			UNION ALL SELECT tp.project_id, tp.permission
				FROM team_projects tp INNER JOIN team_members tm ON tm.team_id = tp.team_id
				WHERE tm.user_id = 3
		) direct_grants GROUP BY project_id
	)
	SELECT pa.project_id AS id, MAX(g.permission) AS permission
	FROM grants g INNER JOIN project_ancestors pa ON pa.ancestor_id = g.project_id
	GROUP BY pa.project_id`)

	if res.RowCount != 3 {
		t.Fatalf("expected 3 rows, got %d: %v", res.RowCount, res.Rows)
	}
	got := map[int64]int64{}
	for _, row := range res.Rows {
		got[row[0].(int64)] = row[1].(int64)
	}
	want := map[int64]int64{1: 2, 2: 1, 3: 2}
	for id, perm := range want {
		if got[id] != perm {
			t.Fatalf("permission for project %d = %v, want %v (all: %v)", id, got[id], perm, got)
		}
	}
}

// TestCTEChained verifies a CTE that references an earlier CTE.
func TestCTEChained(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)")
	execMust(t, e, "INSERT INTO t VALUES (1, 10)")
	execMust(t, e, "INSERT INTO t VALUES (2, 20)")

	res := execMust(t, e, `WITH base AS (SELECT id, v FROM t WHERE v > 5),
		doubled AS (SELECT id, v * 2 AS d FROM base)
		SELECT id, d FROM doubled ORDER BY id`)
	if res.RowCount != 2 {
		t.Fatalf("expected 2 rows, got %d: %v", res.RowCount, res.Rows)
	}
	if res.Rows[0][1] != int64(20) || res.Rows[1][1] != int64(40) {
		t.Fatalf("unexpected rows: %v", res.Rows)
	}
}

// TestRecursiveCTENonCompound verifies a plain CTE under WITH RECURSIVE is
// treated as non-recursive.
func TestRecursiveCTENonCompound(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)")
	execMust(t, e, "INSERT INTO t VALUES (1, 10)")
	execMust(t, e, "INSERT INTO t VALUES (2, 20)")
	res := execMust(t, e, "WITH RECURSIVE r AS (SELECT id, v FROM t) SELECT id, v FROM r ORDER BY id")
	if res.RowCount != 2 {
		t.Fatalf("expected 2 rows, got %d: %v", res.RowCount, res.Rows)
	}
}
