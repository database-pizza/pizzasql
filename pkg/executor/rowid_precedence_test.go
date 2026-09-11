package executor

import "testing"

func TestRowIDAliasPrecedenceRealOidColumn(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	// id is the implicit integer PK (the rowid); oid is a real TEXT column.
	execMust(t, e, "CREATE TABLE lfs_object (id INTEGER PRIMARY KEY, oid TEXT, size INTEGER)")
	execMust(t, e, "INSERT INTO lfs_object VALUES (1, 'ef79c8f0', 1234)")

	// Projection: oid must be the real string, not the hidden rowid.
	res := execMust(t, e, "SELECT oid FROM lfs_object")
	if res.Rows[0][0] != "ef79c8f0" {
		t.Fatalf("SELECT oid = %v (%T), want the real string", res.Rows[0][0], res.Rows[0][0])
	}

	// Predicate: WHERE oid = '...' must match the real string column.
	res = execMust(t, e, "SELECT id FROM lfs_object WHERE oid = 'ef79c8f0'")
	if res.RowCount != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("WHERE oid filter wrong: %v", res.Rows)
	}

	// SELECT * returns the real oid value, not the rowid in its place.
	res = execMust(t, e, "SELECT * FROM lfs_object")
	if len(res.Rows[0]) != 3 || res.Rows[0][1] != "ef79c8f0" {
		t.Fatalf("SELECT * row = %v, want [1 ef79c8f0 1234]", res.Rows[0])
	}
}

func TestRowIDAliasPrecedenceRealRowidColumn(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, rowid TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (5, 'custom-rowid')")

	res := execMust(t, e, "SELECT rowid FROM t WHERE id = 5")
	if res.Rows[0][0] != "custom-rowid" {
		t.Fatalf("SELECT rowid = %v, want 'custom-rowid' (real column)", res.Rows[0][0])
	}
}

func TestRowIDAliasWhenNoRealColumn(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (1, 'a')")
	execMust(t, e, "INSERT INTO t VALUES (2, 'b')")

	// Without a real oid/rowid column, the aliases fall back to the hidden rowid.
	res := execMust(t, e, "SELECT rowid FROM t WHERE id = 2")
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("SELECT rowid (hidden alias) = %v, want 2", res.Rows[0][0])
	}
	res = execMust(t, e, "SELECT oid FROM t WHERE id = 1")
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("SELECT oid (hidden alias) = %v, want 1", res.Rows[0][0])
	}
}
