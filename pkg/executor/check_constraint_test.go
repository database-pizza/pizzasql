package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// requireCheckViolation asserts that err is a CHECK violation for constraint.
func requireCheckViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var violation *storage.CheckViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("error = %v, want CheckViolationError", err)
	}
	if constraint != "" && violation.Constraint != constraint {
		t.Fatalf("constraint = %q, want %q", violation.Constraint, constraint)
	}
	if !strings.HasPrefix(err.Error(), "CHECK constraint failed: ") {
		t.Fatalf("message = %q, want the CHECK constraint failure prefix", err.Error())
	}
}

// Inline, table-level, named, and unnamed CHECK declarations all persist and
// enforce the same predicate.
func TestCheckConstraintDeclarationForms(t *testing.T) {
	tests := []struct {
		name       string
		ddl        string
		constraint string // expected label on violation
	}{
		{
			name: "inline unnamed",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (age > 0))",
		},
		{
			name: "table level unnamed",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER, CHECK (age > 0))",
		},
		{
			name:       "column level named",
			ddl:        "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CONSTRAINT positive_age CHECK (age > 0))",
			constraint: "positive_age",
		},
		{
			name:       "table level named",
			ddl:        "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER, CONSTRAINT positive_age CHECK (age > 0))",
			constraint: "positive_age",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, schema, table := newTestDB(t)
			e := newExec(schema, table)
			execMust(t, e, tt.ddl)

			stored, err := schema.GetSchema("t")
			if err != nil {
				t.Fatalf("get schema: %v", err)
			}
			if len(stored.Checks) != 1 {
				t.Fatalf("checks = %+v, want exactly one persisted CHECK", stored.Checks)
			}
			if tt.constraint != "" && stored.Checks[0].Name != tt.constraint {
				t.Fatalf("check name = %q, want %q", stored.Checks[0].Name, tt.constraint)
			}

			if _, err := execSQL(e, "INSERT INTO t (id, age) VALUES (1, -1)"); err == nil {
				t.Fatal("insert of a violating row succeeded")
			} else {
				requireCheckViolation(t, err, tt.constraint)
			}
			execMust(t, e, "INSERT INTO t (id, age) VALUES (2, 1)")
			execMust(t, e, "INSERT INTO t (id, age) VALUES (3, NULL)")
		})
	}
}

// A CHECK predicate that is NULL or unknown for a row passes, matching SQL
// three-valued logic, on INSERT and on UPDATE.
func TestCheckConstraintNullAndUnknownPass(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER, CHECK (age > 0 AND age < 200))")

	execMust(t, e, "INSERT INTO t (id, age) VALUES (1, NULL)")
	execMust(t, e, "INSERT INTO t (id) VALUES (2)")

	execMust(t, e, "INSERT INTO t (id, age) VALUES (3, 30)")
	execMust(t, e, "UPDATE t SET age = NULL WHERE id = 3")

	res := execMust(t, e, "SELECT id, age FROM t ORDER BY id")
	if res.RowCount != 3 {
		t.Fatalf("rows = %d, want 3 nullable rows", res.RowCount)
	}
	for _, row := range res.Rows {
		if row[1] != nil {
			t.Fatalf("age = %v, want NULL", row[1])
		}
	}
}

// Multi-row INSERT is atomic: a violating row rejects the whole statement and
// leaves no rows behind.
func TestCheckConstraintMultiRowInsertIsAtomic(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (age > 0))")

	_, err := execSQL(e, "INSERT INTO t (id, age) VALUES (1, 10), (2, -5)")
	requireCheckViolation(t, err, "")

	if res := execMust(t, e, "SELECT id FROM t"); res.RowCount != 0 {
		t.Fatalf("rows after rejected multi-row insert = %d, want 0", res.RowCount)
	}

	execMust(t, e, "INSERT INTO t (id, age) VALUES (1, 10), (2, 20)")
	if res := execMust(t, e, "SELECT id FROM t ORDER BY id"); res.RowCount != 2 {
		t.Fatalf("rows = %d, want 2", res.RowCount)
	}
}

// UPDATE re-evaluates every CHECK predicate against the post-assignment row and
// rolls the row back when a predicate fails.
func TestCheckConstraintUpdateViolation(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (age > 0))")
	execMust(t, e, "INSERT INTO t (id, age) VALUES (1, 10)")

	_, err := execSQL(e, "UPDATE t SET age = -1 WHERE id = 1")
	requireCheckViolation(t, err, "")

	res := execMust(t, e, "SELECT age FROM t WHERE id = 1")
	if res.RowCount != 1 || res.Rows[0][0] != int64(10) {
		t.Fatalf("age after rejected update = %v, want 10", res.Rows)
	}

	execMust(t, e, "UPDATE t SET age = age + 5 WHERE id = 1")
	res = execMust(t, e, "SELECT age FROM t WHERE id = 1")
	if res.Rows[0][0] != int64(15) {
		t.Fatalf("age after valid update = %v, want 15", res.Rows[0][0])
	}
}

// ON CONFLICT DO UPDATE validates the merged row against the CHECK predicates
// before it is staged.
func TestCheckConstraintOnConflictDoUpdateViolation(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, qty INTEGER CHECK (qty > 0))")
	execMust(t, e, "INSERT INTO t (id, qty) VALUES (1, 5)")

	_, err := execSQL(e, "INSERT INTO t (id, qty) VALUES (1, -2) ON CONFLICT (id) DO UPDATE SET qty = excluded.qty")
	requireCheckViolation(t, err, "")

	res := execMust(t, e, "SELECT qty FROM t WHERE id = 1")
	if res.Rows[0][0] != int64(5) {
		t.Fatalf("qty after rejected upsert = %v, want 5", res.Rows[0][0])
	}

	execMust(t, e, "INSERT INTO t (id, qty) VALUES (1, 7) ON CONFLICT (id) DO UPDATE SET qty = excluded.qty")
	res = execMust(t, e, "SELECT qty FROM t WHERE id = 1")
	if res.Rows[0][0] != int64(7) {
		t.Fatalf("qty after valid upsert = %v, want 7", res.Rows[0][0])
	}
}

// Every CHECK on a table is enforced, and the violation names the constraint
// that rejected the row.
func TestCheckConstraintMultipleChecks(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (
		id INTEGER PRIMARY KEY,
		age INTEGER CHECK (age > 0),
		score INTEGER,
		CONSTRAINT score_range CHECK (score >= 0 AND score <= 100)
	)`)

	stored, err := schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if len(stored.Checks) != 2 {
		t.Fatalf("checks = %+v, want 2", stored.Checks)
	}

	_, err = execSQL(e, "INSERT INTO t (id, age, score) VALUES (1, -1, 50)")
	requireCheckViolation(t, err, "")

	_, err = execSQL(e, "INSERT INTO t (id, age, score) VALUES (2, 10, 101)")
	requireCheckViolation(t, err, "score_range")

	execMust(t, e, "INSERT INTO t (id, age, score) VALUES (3, 10, 100)")
	if res := execMust(t, e, "SELECT id FROM t"); res.RowCount != 1 {
		t.Fatalf("rows = %d, want 1", res.RowCount)
	}
}

// ADD COLUMN ... CHECK validates the rows that already exist, using the value
// the constant-default backfill will write (or NULL when there is no default).
func TestAlterTableAddColumnCheckValidatesExistingRows(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t (id) VALUES (1), (2)")

	// A default satisfying the predicate is backfilled and the DDL succeeds.
	execMust(t, e, "ALTER TABLE t ADD COLUMN qty INTEGER DEFAULT 5 CHECK (qty >= 0)")

	stored, err := schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if _, ok := stored.GetColumn("qty"); !ok {
		t.Fatal("ADD COLUMN ... CHECK published no column")
	}
	if len(stored.Checks) != 1 {
		t.Fatalf("checks = %+v, want 1", stored.Checks)
	}
	res := execMust(t, e, "SELECT qty FROM t ORDER BY id")
	if len(res.Rows) != 2 || res.Rows[0][0] != int64(5) || res.Rows[1][0] != int64(5) {
		t.Fatalf("backfilled qty = %v, want [5 5]", res.Rows)
	}

	// A violation is enforced for later writes as well.
	if _, err := execSQL(e, "INSERT INTO t (id, qty) VALUES (3, -1)"); err == nil {
		t.Fatal("insert violating the ADD COLUMN check succeeded")
	} else {
		requireCheckViolation(t, err, "")
	}

	// A default that violates the predicate rejects the DDL and publishes
	// nothing: no column, no constraint, and no rewritten rows.
	_, err = execSQL(e, "ALTER TABLE t ADD COLUMN bad INTEGER DEFAULT -1 CHECK (bad >= 0)")
	requireCheckViolation(t, err, "")
	stored, err = schema.GetSchema("t")
	if err != nil {
		t.Fatalf("get schema after rejected DDL: %v", err)
	}
	if _, ok := stored.GetColumn("bad"); ok {
		t.Fatal("rejected ADD COLUMN ... CHECK left the column in the schema")
	}
	if len(stored.Checks) != 1 {
		t.Fatalf("checks after rejected DDL = %+v, want the original one", stored.Checks)
	}

	// Without a default the existing rows read NULL, which passes.
	execMust(t, e, "ALTER TABLE t ADD COLUMN note TEXT CHECK (LENGTH(note) > 0)")
	if res := execMust(t, e, "SELECT id, note FROM t ORDER BY id"); res.RowCount != 2 || res.Rows[0][1] != nil {
		t.Fatalf("rows after nullable ADD COLUMN = %v, want NULL notes", res.Rows)
	}
}

// A transaction that stages a row before a concurrent ADD COLUMN ... CHECK is
// validated at commit, so the backfilled default cannot bypass the predicate.
func TestAlterTableAddColumnCheckRejectsRowStagedBeforeDDL(t *testing.T) {
	_, schema, table := newTestDB(t)
	writer := newExec(schema, table)
	ddl := newExec(schema, table)
	execMust(t, writer, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	execMust(t, writer, "BEGIN")
	execMust(t, writer, "INSERT INTO t (id) VALUES (1)")

	// The table is durably empty, so the ALTER accepts the constraint.
	execMust(t, ddl, "ALTER TABLE t ADD COLUMN qty INTEGER DEFAULT -1 CHECK (qty >= 0)")

	_, err := execSQL(writer, "COMMIT")
	requireCheckViolation(t, err, "")

	if got := execMust(t, ddl, "SELECT id FROM t").RowCount; got != 0 {
		t.Fatalf("rows after rejected commit = %d, want 0", got)
	}
}

// The persisted predicate is durable: a fresh SchemaManager loaded from the same
// store keeps enforcing it.
func TestCheckConstraintDurableReload(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER, CONSTRAINT positive_age CHECK (age > 0))")
	execMust(t, e, "INSERT INTO t (id, age) VALUES (1, 10)")

	reloaded := storage.NewSchemaManager(pool, "testdb")
	reloadedTable := storage.NewTableManager(pool, reloaded, "testdb")
	reopened := newExec(reloaded, reloadedTable)

	stored, err := reloaded.GetSchema("t")
	if err != nil {
		t.Fatalf("reload schema: %v", err)
	}
	if len(stored.Checks) != 1 || stored.Checks[0].Name != "positive_age" || !strings.Contains(stored.Checks[0].Expression, "age") {
		t.Fatalf("reloaded checks = %+v, want the persisted positive_age predicate", stored.Checks)
	}

	_, err = execSQL(reopened, "INSERT INTO t (id, age) VALUES (2, -3)")
	requireCheckViolation(t, err, "positive_age")

	execMust(t, reopened, "INSERT INTO t (id, age) VALUES (3, 30)")
	execMust(t, reopened, "UPDATE t SET age = 40 WHERE id = 3")
	if _, err := execSQL(reopened, "UPDATE t SET age = -40 WHERE id = 3"); err == nil {
		t.Fatal("update violating the reloaded check succeeded")
	}
}

// The emulated SQLite catalog exposes the constraint in the table SQL.
func TestCheckConstraintInSQLiteCatalog(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER, CONSTRAINT positive_age CHECK (age > 0), CHECK (id > 0))")

	res := execMust(t, e, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 't'")
	if res.RowCount != 1 {
		t.Fatalf("sqlite_master rows = %d, want 1", res.RowCount)
	}
	sql, _ := res.Rows[0][0].(string)
	for _, want := range []string{"CONSTRAINT `positive_age` CHECK", "(id > 0)"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("catalog SQL %q does not contain %q", sql, want)
		}
	}

	res = execMust(t, e, "SELECT sql FROM sqlite_schema WHERE name = 't'")
	if res.RowCount != 1 || !strings.Contains(res.Rows[0][0].(string), "positive_age") {
		t.Fatalf("sqlite_schema SQL = %v, want the named CHECK", res.Rows)
	}
}

// Non-deterministic and non-row predicates are rejected with feature-not-
// supported instead of being accepted as constraints the engine cannot enforce.
func TestCheckConstraintRejectsUnsupportedExpressions(t *testing.T) {
	tests := []struct {
		name  string
		setup []string
		ddl   string
	}{
		{
			name: "subquery",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (age > (SELECT 1)))",
		},
		{
			name: "aggregate",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (COUNT(age) > 0))",
		},
		{
			name: "window function",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (ROW_NUMBER() OVER (ORDER BY age) > 0))",
		},
		{
			name: "non-deterministic function",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (RANDOM() > 0))",
		},
		{
			name: "qualified column",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (t.age > 0))",
		},
		{
			name: "unknown column",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (missing > 0))",
		},
		{
			name:  "alter add column subquery",
			setup: []string{"CREATE TABLE t (id INTEGER PRIMARY KEY)"},
			ddl:   "ALTER TABLE t ADD COLUMN age INTEGER CHECK (age > (SELECT 1))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, schema, table := newTestDB(t)
			e := newExec(schema, table)
			for _, sql := range tt.setup {
				execMust(t, e, sql)
			}

			if _, err := execSQL(e, tt.ddl); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
				t.Fatalf("%s: error = %v, want ErrFeatureNotSupported", tt.ddl, err)
			}
			// A rejected declaration publishes nothing: no table when the DDL
			// was CREATE TABLE, and no added column or check for ALTER TABLE.
			stored, err := schema.GetSchema("t")
			if err != nil {
				if len(tt.setup) == 0 {
					return
				}
				t.Fatalf("get schema: %v", err)
			}
			if len(stored.Checks) != 0 {
				t.Fatalf("checks after rejected DDL = %+v, want none", stored.Checks)
			}
			if _, ok := stored.GetColumn("age"); ok {
				t.Fatalf("column age after rejected DDL = ok, want absent")
			}
		})
	}
}

// A CHECK predicate is stored as SQL text, so DROP/RENAME COLUMN refuse to leave
// it referencing a column that no longer exists.
func TestCheckConstraintGuardsColumnChange(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (age > 0), note TEXT)")

	for _, sql := range []string{
		"ALTER TABLE t RENAME COLUMN age TO years",
		"ALTER TABLE t DROP COLUMN age",
	} {
		if _, err := execSQL(e, sql); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Fatalf("%s: error = %v, want ErrFeatureNotSupported", sql, err)
		}
	}

	// Columns the predicate does not reference can still be renamed or dropped.
	execMust(t, e, "ALTER TABLE t RENAME COLUMN note TO memo")
	execMust(t, e, "INSERT INTO t (id, age, memo) VALUES (1, 5, 'ok')")
	execMust(t, e, "ALTER TABLE t DROP COLUMN memo")
}

// The accepted expression surface survives the round trip through the durable
// schema text: each predicate is re-parsed and still enforced after a reload.
func TestCheckConstraintExpressionRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		check    string
		columns  string
		valid    string // VALUES list for a row that satisfies the predicate
		violated string // VALUES list for a row that violates it
	}{
		{"comparison and boolean", "age > 0 AND age < 200", "id, age", "(1, 10)", "(1, 500)"},
		{"LIKE", "name LIKE 'a%'", "id, name", "(1, 'abc')", "(1, 'zzz')"},
		{"CAST", "CAST(score AS INTEGER) >= 0", "id, score", "(1, 5)", "(1, -5)"},
		{"BETWEEN", "LENGTH(code) BETWEEN 1 AND 8", "id, code", "(1, 'abcd')", "(1, 'toolongcode')"},
		{"function", "LOWER(status) = 'ready'", "id, status", "(1, 'READY')", "(1, 'waiting')"},
		{"is null", "note IS NOT NULL", "id, note", "(1, 'x')", "(1, NULL)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, schema, table := newTestDB(t)
			e := newExec(schema, table)
			execMust(t, e, `CREATE TABLE t (
				id INTEGER PRIMARY KEY,
				age INTEGER,
				name TEXT,
				score INTEGER,
				code TEXT,
				status TEXT,
				note TEXT,
				CHECK (`+tt.check+`)
			)`)
			execMust(t, e, "INSERT INTO t ("+tt.columns+") VALUES "+tt.valid)

			reloaded := storage.NewSchemaManager(pool, "testdb")
			reopened := newExec(reloaded, storage.NewTableManager(pool, reloaded, "testdb"))

			if _, err := execSQL(reopened, "INSERT INTO t ("+tt.columns+") VALUES "+tt.violated); err == nil {
				t.Fatalf("reloaded table accepted a row violating %s", tt.check)
			} else {
				requireCheckViolation(t, err, "")
			}
		})
	}
}
