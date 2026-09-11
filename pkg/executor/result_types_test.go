package executor

import "testing"

func TestSelectColumnTypesFromSchema(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE users (
		id BIGINT PRIMARY KEY,
		name TEXT NOT NULL,
		created_at TIMESTAMP NULL,
		score DOUBLE
	)`)

	res := execMust(t, e, "SELECT id, name, created_at, score FROM users")
	if len(res.ColumnTypes) != 4 {
		t.Fatalf("got %d column types, want 4", len(res.ColumnTypes))
	}
	want := []string{"BIGINT", "TEXT", "TIMESTAMP", "DOUBLE"}
	for i, w := range want {
		if res.ColumnTypes[i] != w {
			t.Errorf("column %d type = %q, want %q", i, res.ColumnTypes[i], w)
		}
	}
}

func TestSelectColumnTypesStar(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, label TEXT)")
	res := execMust(t, e, "SELECT * FROM t")
	if len(res.ColumnTypes) != 2 {
		t.Fatalf("got %d column types, want 2", len(res.ColumnTypes))
	}
	if res.ColumnTypes[0] != "INTEGER" || res.ColumnTypes[1] != "TEXT" {
		t.Fatalf("unexpected column types %v", res.ColumnTypes)
	}
}

func TestSelectColumnTypesAlias(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	res := execMust(t, e, "SELECT id AS ident FROM t")
	if len(res.ColumnTypes) != 1 || res.ColumnTypes[0] != "BIGINT" {
		t.Fatalf("unexpected column types %v", res.ColumnTypes)
	}
}

func TestSelectColumnTypesUnknownExpression(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	res := execMust(t, e, "SELECT id + 1, id * 2 FROM t")
	if len(res.ColumnTypes) != 2 || res.ColumnTypes[0] != "TEXT" || res.ColumnTypes[1] != "TEXT" {
		t.Fatalf("unexpected column types %v", res.ColumnTypes)
	}
}

func TestSelectColumnTypesEmptyResult(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id BIGINT PRIMARY KEY, created_at DATETIME)")
	execMust(t, e, "INSERT INTO t VALUES (1, '2024-01-01T00:00:00Z')")
	res := execMust(t, e, "SELECT id, created_at FROM t WHERE id = 999")
	if res.RowCount != 0 {
		t.Fatalf("expected empty result, got %d rows", res.RowCount)
	}
	if len(res.ColumnTypes) != 2 || res.ColumnTypes[0] != "BIGINT" || res.ColumnTypes[1] != "DATETIME" {
		t.Fatalf("empty result types = %v, want [BIGINT DATETIME]", res.ColumnTypes)
	}
}

func TestUUIDColumnRoundTrip(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE upload (id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL, uuid UUID NULL, name TEXT NULL)")
	execMust(t, e, "INSERT INTO upload (uuid, name) VALUES ('abc-123', 'x')")

	res := execMust(t, e, "SELECT uuid, name FROM upload")
	if res.RowCount != 1 {
		t.Fatalf("expected 1 row, got %d", res.RowCount)
	}
	if res.Rows[0][0] != "abc-123" {
		t.Fatalf("uuid value = %v, want %q", res.Rows[0][0], "abc-123")
	}
	if len(res.ColumnTypes) != 2 || res.ColumnTypes[0] != "UUID" || res.ColumnTypes[1] != "TEXT" {
		t.Fatalf("column types = %v, want [UUID TEXT]", res.ColumnTypes)
	}
}
