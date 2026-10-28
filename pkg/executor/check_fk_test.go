package executor

import (
	"errors"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

func TestCheckConstraintsRejectedAtDDL(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	for _, sql := range []string{
		"CREATE TABLE t1 (id INTEGER PRIMARY KEY, age INTEGER CHECK (age > 0))",
		"CREATE TABLE t2 (id INTEGER PRIMARY KEY, age INTEGER, CHECK (age > 0))",
		"CREATE TABLE t3 (id INTEGER PRIMARY KEY, age INTEGER, CONSTRAINT positive_age CHECK (age > 0))",
	} {
		if _, err := execSQL(e, sql); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Fatalf("%s: error = %v, want ErrFeatureNotSupported", sql, err)
		}
	}

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	if _, err := execSQL(e, "ALTER TABLE t ADD COLUMN age INTEGER CHECK (age > 0)"); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
		t.Fatalf("ALTER CHECK error = %v, want ErrFeatureNotSupported", err)
	}
}

func TestForeignKeyConstraintsRejectedAtDDL(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")

	for _, sql := range []string{
		"CREATE TABLE child1 (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))",
		"CREATE TABLE child2 (id INTEGER PRIMARY KEY, parent_id INTEGER, FOREIGN KEY (parent_id) REFERENCES parent(id))",
		"CREATE TABLE child3 (id INTEGER PRIMARY KEY, parent_id INTEGER, CONSTRAINT child_parent FOREIGN KEY (parent_id) REFERENCES parent(id) ON DELETE CASCADE)",
	} {
		if _, err := execSQL(e, sql); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Fatalf("%s: error = %v, want ErrFeatureNotSupported", sql, err)
		}
	}

	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	if _, err := execSQL(e, "ALTER TABLE t ADD COLUMN parent_id INTEGER REFERENCES parent(id)"); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
		t.Fatalf("ALTER FK error = %v, want ErrFeatureNotSupported", err)
	}
}
