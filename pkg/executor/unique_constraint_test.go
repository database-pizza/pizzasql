package executor

import (
	"strings"
	"testing"
)

func TestCreateTableInlineUniqueConstraint(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE login_source (id INTEGER PRIMARY KEY, name TEXT UNIQUE)")

	execMust(t, e, "INSERT INTO login_source VALUES (1, 'github')")
	_, err := execSQL(e, "INSERT INTO login_source VALUES (2, 'github')")
	if err == nil {
		t.Fatal("expected unique violation for inline column UNIQUE")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateTableCompositeUniqueConstraint(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE access_token (id INTEGER PRIMARY KEY, sha1 TEXT, sha256 TEXT, CONSTRAINT uni_access_token_sha1 UNIQUE(sha1), CONSTRAINT uni_access_token_sha256 UNIQUE(sha256))")

	execMust(t, e, "INSERT INTO access_token (sha1, sha256) VALUES ('s1', 'h1')")
	if _, err := execSQL(e, "INSERT INTO access_token (sha1, sha256) VALUES ('s1', 'h2')"); err == nil {
		t.Fatal("expected unique violation on sha1")
	}
	if _, err := execSQL(e, "INSERT INTO access_token (sha1, sha256) VALUES ('s2', 'h1')"); err == nil {
		t.Fatal("expected unique violation on sha256")
	}
	execMust(t, e, "INSERT INTO access_token (sha1, sha256) VALUES ('s2', 'h2')")
}

func TestCreateTableMultiColumnUniqueConstraint(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, b TEXT, CONSTRAINT uq_ab UNIQUE(a, b))")

	execMust(t, e, "INSERT INTO t VALUES (1, 'x', 'y')")
	execMust(t, e, "INSERT INTO t VALUES (2, 'x', 'z')")
	if _, err := execSQL(e, "INSERT INTO t VALUES (3, 'x', 'y')"); err == nil {
		t.Fatal("expected unique violation on composite (x,y)")
	}
}

func TestCreateTableUniqueConstraintReportedInCatalog(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, CONSTRAINT uq_t_name UNIQUE(name))")

	res := execMust(t, e, "PRAGMA index_list('t')")
	found := false
	for _, row := range res.Rows {
		// columns: seq, name, unique, origin, partial
		if row[1] == "uq_t_name" {
			found = true
			if row[2] != int64(1) {
				t.Fatalf("index uq_t_name unique flag = %v, want 1", row[2])
			}
		}
	}
	if !found {
		t.Fatalf("named unique constraint not reported in index_list: %v", res.Rows)
	}
}

func TestCreateTableIfNotExistsWithUniqueConstraint(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY, name TEXT UNIQUE)")
	execMust(t, e, "INSERT INTO t VALUES (1, 'a')")
	// Second CREATE IF NOT EXISTS must be a no-op and not fail on the duplicate index.
	execMust(t, e, "CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY, name TEXT UNIQUE)")
	if _, err := execSQL(e, "INSERT INTO t VALUES (2, 'a')"); err == nil {
		t.Fatal("expected unique violation to still be enforced after no-op recreate")
	}
}
