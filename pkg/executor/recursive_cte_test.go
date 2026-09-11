package executor

import "testing"

func TestRecursiveCTE(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, parent_id INTEGER)")
	execMust(t, e, "INSERT INTO t VALUES (1, NULL)")
	execMust(t, e, "INSERT INTO t VALUES (2, 1)")
	execMust(t, e, "INSERT INTO t VALUES (3, 2)")
	execMust(t, e, "INSERT INTO t VALUES (4, NULL)")

	res := execMust(t, e, `WITH RECURSIVE hier AS (
		SELECT id, parent_id, 0 AS level, id AS root FROM t WHERE id = 3
		UNION ALL
		SELECT t.id, t.parent_id, h.level + 1, h.root FROM t INNER JOIN hier h ON t.id = h.parent_id
	)
	SELECT id, level, root FROM hier ORDER BY id`)

	if res.RowCount != 3 {
		t.Fatalf("expected 3 rows, got %d: %v", res.RowCount, res.Rows)
	}
	want := map[int64]int64{3: 0, 2: 1, 1: 2}
	for _, row := range res.Rows {
		id := row[0].(int64)
		if row[1].(int64) != want[id] {
			t.Fatalf("id %d level = %v, want %v", id, row[1], want[id])
		}
		if row[2].(int64) != 3 {
			t.Fatalf("id %d root = %v, want 3", id, row[2])
		}
	}
}

func TestRowNumberWindow(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE s (id INTEGER PRIMARY KEY, grp INTEGER, val INTEGER)")
	execMust(t, e, "INSERT INTO s VALUES (1, 1, 10)")
	execMust(t, e, "INSERT INTO s VALUES (2, 1, 5)")
	execMust(t, e, "INSERT INTO s VALUES (3, 2, 7)")
	execMust(t, e, "INSERT INTO s VALUES (4, 2, 7)")

	res := execMust(t, e, "SELECT id, ROW_NUMBER() OVER (PARTITION BY grp ORDER BY val) AS rn FROM s ORDER BY id")
	want := map[int64]int64{1: 2, 2: 1, 3: 1, 4: 2}
	if res.RowCount != 4 {
		t.Fatalf("expected 4 rows, got %d: %v", res.RowCount, res.Rows)
	}
	for _, row := range res.Rows {
		id := row[0].(int64)
		if row[1].(int64) != want[id] {
			t.Fatalf("id %d rn = %v, want %v (rows %v)", id, row[1], want[id], res.Rows)
		}
	}
}

// TestRecursiveCTEWithWindow reproduces the combined shape of Vikunja's
// subscription query: a recursive CTE, a dependent CTE, and a windowed derived
// table joined back to a real table.
func TestRecursiveCTEWithWindow(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE projects (id INTEGER PRIMARY KEY, parent_project_id INTEGER)")
	execMust(t, e, "CREATE TABLE subscriptions (id INTEGER PRIMARY KEY, entity_type INTEGER, entity_id INTEGER, user_id INTEGER, muted INTEGER)")
	execMust(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT)")
	execMust(t, e, "INSERT INTO projects VALUES (1, NULL)")
	execMust(t, e, "INSERT INTO projects VALUES (2, 1)")
	execMust(t, e, "INSERT INTO projects VALUES (3, 2)")
	execMust(t, e, "INSERT INTO subscriptions VALUES (1, 1, 1, 10, 0)")
	execMust(t, e, "INSERT INTO subscriptions VALUES (2, 1, 3, 20, 0)")
	execMust(t, e, "INSERT INTO users VALUES (10, 'alice')")
	execMust(t, e, "INSERT INTO users VALUES (20, 'bob')")

	res := execMust(t, e, `WITH RECURSIVE project_hierarchy AS (
		SELECT id, parent_project_id, 0 AS level, id AS original_project_id FROM projects WHERE id IN (3)
		UNION ALL
		SELECT p.id, p.parent_project_id, ph.level + 1, ph.original_project_id
		FROM projects p INNER JOIN project_hierarchy ph ON p.id = ph.parent_project_id
	),
	subscription_hierarchy AS (
		SELECT s.id, s.entity_type, s.entity_id, s.user_id, s.muted,
			CASE WHEN s.entity_id = ph.original_project_id THEN 1 ELSE ph.level + 1 END AS priority,
			ph.original_project_id
		FROM subscriptions s INNER JOIN project_hierarchy ph ON s.entity_id = ph.id
		WHERE s.entity_type = 1
	)
	SELECT p.id AS original_entity_id, sh.id AS subscription_id, sh.user_id
	FROM projects p
		LEFT JOIN (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY original_project_id, user_id ORDER BY priority) AS rn
			FROM subscription_hierarchy
		) sh ON p.id = sh.original_project_id AND sh.rn = 1
	WHERE p.id IN (3)
	ORDER BY p.id, sh.user_id`)

	if res.RowCount != 2 {
		t.Fatalf("expected 2 rows, got %d: %v", res.RowCount, res.Rows)
	}
	got := map[int64]bool{}
	for _, row := range res.Rows {
		got[row[1].(int64)] = true
	}
	if !got[1] || !got[2] {
		t.Fatalf("expected subscriptions 1 and 2, got %v", res.Rows)
	}
}
