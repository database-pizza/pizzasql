package executor

import (
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// A catalog-backed predicate must be resolved before a statement takes the
// session lock. Evaluating it inside the locked scan re-enters the session lock
// (which is not re-entrant), so the statement hangs forever and every other
// session blocks on the table the stuck statement never releases.
//
// These regressions therefore run every statement under a bounded timeout and
// probe a second session afterwards.
const catalogLockTimeout = 10 * time.Second

type catalogLockOutcome struct {
	result *Result
	err    error
}

// runCatalogLockedStatement executes one statement and fails the test when it
// does not finish within catalogLockTimeout.
func runCatalogLockedStatement(t *testing.T, e *Executor, sql string) (*Result, error) {
	t.Helper()
	done := make(chan catalogLockOutcome, 1)
	go func() {
		result, err := execSQL(e, sql)
		done <- catalogLockOutcome{result: result, err: err}
	}()
	select {
	case out := <-done:
		return out.result, out.err
	case <-time.After(catalogLockTimeout):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("%s did not finish within %s (deadlock)\n%s", sql, catalogLockTimeout, buf[:n])
		return nil, nil
	}
}

// probeSessionLiveness proves a second connection can still read and write: a
// statement that deadlocks while holding the session lock leaks the table gate,
// so a later session's SELECT or INSERT blocks forever.
func probeSessionLiveness(t *testing.T, schema *storage.SchemaManager, table *storage.TableManager, tableName string) {
	t.Helper()
	observer := newExec(schema, table)
	if _, err := runCatalogLockedStatement(t, observer, "SELECT count(*) FROM "+tableName); err != nil {
		t.Fatalf("observer SELECT on %s failed: %v", tableName, err)
	}
	if _, err := runCatalogLockedStatement(t, observer, "INSERT INTO probe (id) VALUES (1)"); err != nil {
		t.Fatalf("observer INSERT failed: %v", err)
	}
}

// setupCatalogLockFixture builds the target table plus an unrelated table the
// observer session writes to.
func setupCatalogLockFixture(t *testing.T) (*Executor, *storage.SchemaManager, *storage.TableManager) {
	t.Helper()
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (1, 'a'), (2, 'b')")
	execMust(t, e, "CREATE TABLE other (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO other VALUES (1)")
	execMust(t, e, "CREATE TABLE probe (id INTEGER PRIMARY KEY)")
	return e, schema, table
}

func TestPGCatalogPredicateInLockedScanDoesNotDeadlock(t *testing.T) {
	cases := []struct {
		name      string
		sql       string
		wantError bool
		wantRows  int
		wantValue string
	}{
		{
			name:      "update unknown regclass",
			sql:       `UPDATE t SET v = 'z' WHERE v = 'a' AND 'missing_relation'::regclass IS NOT NULL`,
			wantError: true,
		},
		{
			name:     "update known regclass",
			sql:      `UPDATE t SET v = 'z' WHERE v = 'a' AND 't'::regclass IS NOT NULL`,
			wantRows: 1,
		},
		{
			name:      "delete unknown regclass",
			sql:       `DELETE FROM t WHERE v = 'a' AND 'missing_relation'::regclass IS NOT NULL`,
			wantError: true,
		},
		{
			name:     "update catalog function predicate",
			sql:      `UPDATE t SET v = 'z' WHERE v = 'a' AND format_type(23, -1) = 'integer'`,
			wantRows: 1,
		},
		{
			name:     "update in catalog subquery",
			sql:      `UPDATE t SET v = 'z' WHERE id IN (SELECT oid FROM pg_class)`,
			wantRows: 0,
		},
		{
			name:     "update in constant subquery",
			sql:      `UPDATE t SET v = 'z' WHERE id IN (SELECT 1)`,
			wantRows: 1,
		},
		{
			name:     "update in base-table subquery",
			sql:      `UPDATE t SET v = 'z' WHERE id IN (SELECT id FROM other)`,
			wantRows: 1,
		},
		{
			name:     "update returning regclass",
			sql:      `UPDATE t SET v = 'z' WHERE id = 1 RETURNING id, 't'::regclass`,
			wantRows: 1,
		},
		{
			name:      "update set catalog function",
			sql:       `UPDATE t SET v = 'fmt:' || format_type(23, -1) WHERE id = 1`,
			wantValue: "fmt:integer",
		},
		{
			name:     "update correlated catalog subquery",
			sql:      `UPDATE t SET v = 'z' WHERE EXISTS (SELECT 1 FROM pg_class c WHERE c.relname = t.v)`,
			wantRows: 0,
		},
		{
			name:     "update catalog relation predicate",
			sql:      `UPDATE t SET v = 'z' WHERE id IN (SELECT c.oid FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid)`,
			wantRows: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, schema, table := setupCatalogLockFixture(t)
			_, err := runCatalogLockedStatement(t, e, tc.sql)
			if tc.wantError {
				if !errors.Is(err, pgerror.ErrUndefinedTable) {
					t.Fatalf("%s: err = %v, want an undefined_table error", tc.sql, err)
				}
			} else if err != nil {
				t.Fatalf("%s: %v", tc.sql, err)
			} else if tc.wantRows > 0 {
				res, countErr := runCatalogLockedStatement(t, e, `SELECT count(*) FROM t WHERE v = 'z'`)
				if countErr != nil {
					t.Fatal(countErr)
				}
				if res.Rows[0][0] != int64(tc.wantRows) {
					t.Fatalf("%s: updated rows = %v, want %d", tc.sql, res.Rows[0][0], tc.wantRows)
				}
			} else if tc.wantValue != "" {
				res, valueErr := runCatalogLockedStatement(t, e, `SELECT v FROM t WHERE id = 1`)
				if valueErr != nil {
					t.Fatal(valueErr)
				}
				if fmt.Sprintf("%v", res.Rows[0][0]) != tc.wantValue {
					t.Fatalf("%s: v = %v, want %q", tc.sql, res.Rows[0][0], tc.wantValue)
				}
			}
			probeSessionLiveness(t, schema, table, "t")
		})
	}
}

// A predicate that fails part-way through the scan must not leave rows written
// by the rows the scan already visited.
func TestPGCatalogPredicateErrorLeavesNoPartialWrite(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	// No UNIQUE index: nothing else makes the statement atomic for it.
	execMust(t, e, "CREATE TABLE plain (id INTEGER, v TEXT)")
	execMust(t, e, "INSERT INTO plain VALUES (1, 'one'), (2, 'two'), (3, 'three')")
	execMust(t, e, "CREATE TABLE probe (id INTEGER PRIMARY KEY)")

	// The predicate succeeds for rows 1 and 3 and fails on row 2, so a
	// non-atomic update would leave rows 1 and 3 changed.
	_, err := runCatalogLockedStatement(t, e, `UPDATE plain SET v = 'changed' WHERE id <> 2 OR 'missing_relation'::regclass IS NOT NULL`)
	if !errors.Is(err, pgerror.ErrUndefinedTable) {
		t.Fatalf("update err = %v, want an undefined_table error", err)
	}
	res := execMust(t, e, `SELECT count(*) FROM plain WHERE v = 'changed'`)
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("partial write: %v rows changed", res.Rows[0][0])
	}

	_, err = runCatalogLockedStatement(t, e, `DELETE FROM plain WHERE id <> 2 OR 'missing_relation'::regclass IS NOT NULL`)
	if !errors.Is(err, pgerror.ErrUndefinedTable) {
		t.Fatalf("delete err = %v, want an undefined_table error", err)
	}
	res = execMust(t, e, `SELECT count(*) FROM plain`)
	if res.Rows[0][0] != int64(3) {
		t.Fatalf("partial delete: %v rows left, want 3", res.Rows[0][0])
	}
	probeSessionLiveness(t, schema, table, "plain")
}

// The same deadlock class applies to any scan that runs inside an open
// transaction, including a SELECT.
func TestPGCatalogPredicateInExplicitTransactionDoesNotDeadlock(t *testing.T) {
	e, schema, table := setupCatalogLockFixture(t)

	if _, err := runCatalogLockedStatement(t, e, "BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	_, err := runCatalogLockedStatement(t, e, `SELECT * FROM t WHERE v = 'a' AND 'missing_relation'::regclass IS NOT NULL`)
	if !errors.Is(err, pgerror.ErrUndefinedTable) {
		t.Fatalf("select err = %v, want an undefined_table error", err)
	}
	_, err = runCatalogLockedStatement(t, e, `UPDATE t SET v = 'z' WHERE v = 'a' AND 'missing_relation'::regclass IS NOT NULL`)
	if !errors.Is(err, pgerror.ErrUndefinedTable) {
		t.Fatalf("update err = %v, want an undefined_table error", err)
	}
	// A catalog relation in a predicate still works inside the transaction.
	res, err := runCatalogLockedStatement(t, e, `SELECT count(*) FROM t WHERE id IN (SELECT 1)`)
	if err != nil {
		t.Fatalf("catalog-free predicate: %v", err)
	}
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("count = %v, want 1", res.Rows[0][0])
	}
	if _, err := runCatalogLockedStatement(t, e, "ROLLBACK"); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}

	// The transaction held no lock after the rollback.
	probeSessionLiveness(t, schema, table, "t")

	// A catalog predicate that matches still updates inside a transaction.
	if _, err := runCatalogLockedStatement(t, e, "BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := runCatalogLockedStatement(t, e, `UPDATE t SET v = 'z' WHERE id = 2 AND 't'::regclass IS NOT NULL`); err != nil {
		t.Fatalf("update in transaction: %v", err)
	}
	if _, err := runCatalogLockedStatement(t, e, "COMMIT"); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	res = execMust(t, e, `SELECT v FROM t WHERE id = 2`)
	if fmt.Sprintf("%v", res.Rows[0][0]) != "z" {
		t.Fatalf("committed value = %v, want z", res.Rows[0][0])
	}
}

// A correlated subquery in a write predicate cannot read the durable schema while
// the scan holds the session lock. It must report that (0A000) instead of
// deadlocking, and the failed statement must leave no partial write and no lock.
func TestPGCatalogCorrelatedPredicateErrorsInsteadOfDeadlocking(t *testing.T) {
	e, schema, table := setupCatalogLockFixture(t)
	_, err := runCatalogLockedStatement(t, e, `UPDATE t SET v = 'z' WHERE EXISTS (SELECT 1 FROM other o WHERE o.id = t.id)`)
	if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
		t.Fatalf("err = %v, want a feature_not_supported error", err)
	}
	res := execMust(t, e, `SELECT count(*) FROM t WHERE v = 'z'`)
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("partial write: %v rows changed", res.Rows[0][0])
	}
	probeSessionLiveness(t, schema, table, "t")
}

// A catalog-only correlated subquery resolves from the statement snapshot, so it
// still works inside the locked scan.
func TestPGCatalogCorrelatedSubqueryResolvesFromSnapshot(t *testing.T) {
	e, schema, table := setupCatalogLockFixture(t)
	if _, err := runCatalogLockedStatement(t, e, `UPDATE t SET v = 'pg_class' WHERE EXISTS (SELECT 1 FROM pg_class c WHERE c.relname = 'pg_class')`); err != nil {
		t.Fatalf("catalog-only correlated subquery: %v", err)
	}
	res := execMust(t, e, `SELECT count(*) FROM t WHERE v = 'pg_class'`)
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("updated rows = %v, want 2", res.Rows[0][0])
	}
	probeSessionLiveness(t, schema, table, "t")
}
