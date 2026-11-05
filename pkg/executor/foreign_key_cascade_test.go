package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/capabilities"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// requireNotNullViolation asserts err is a NOT NULL violation (23502).
func requireNotNullViolation(t *testing.T, err error) *storage.NotNullViolationError {
	t.Helper()
	var violation *storage.NotNullViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("error = %v, want NotNullViolationError", err)
	}
	return violation
}

// countRows returns the number of rows in table.
func countRows(t *testing.T, e *Executor, table string) int64 {
	t.Helper()
	res := execMust(t, e, "SELECT count(*) FROM "+table)
	return res.Rows[0][0].(int64)
}

// ON DELETE CASCADE removes the direct children and, transitively, their own
// children. rows_affected counts only the rows the statement matched directly.
func TestForeignKeyDeleteCascadeSingleAndMultiLevel(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE grandparent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY, gp_id INTEGER REFERENCES grandparent(id) ON DELETE CASCADE)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE)")
	execMust(t, e, "INSERT INTO grandparent (id) VALUES (1), (2)")
	execMust(t, e, "INSERT INTO parent (id, gp_id) VALUES (10, 1), (20, 2)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (100, 10), (101, 10), (200, 20)")

	res := execMust(t, e, "DELETE FROM grandparent WHERE id = 1")
	if res.RowsAffected != 1 {
		t.Fatalf("rows affected = %d, want 1 (cascaded rows are not counted)", res.RowsAffected)
	}
	if got := countRows(t, e, "grandparent"); got != 1 {
		t.Fatalf("grandparent rows = %d, want 1", got)
	}
	if got := countRows(t, e, "parent"); got != 1 {
		t.Fatalf("parent rows = %d, want 1", got)
	}
	if got := countRows(t, e, "child"); got != 1 {
		t.Fatalf("child rows = %d, want 1", got)
	}
	if res := execMust(t, e, "SELECT id FROM child"); res.Rows[0][0] != int64(200) {
		t.Fatalf("remaining child = %v, want 200", res.Rows)
	}

	// A scan delete cascades every matched parent row in one statement.
	execMust(t, e, "DELETE FROM grandparent")
	for _, tableName := range []string{"grandparent", "parent", "child"} {
		if got := countRows(t, e, tableName); got != 0 {
			t.Fatalf("%s rows after cascading scan delete = %d, want 0", tableName, got)
		}
	}
}

// ON UPDATE CASCADE copies the new parent key into every referencing child,
// including a referenced UNIQUE index column and a composite key.
func TestForeignKeyUpdateCascade(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY, code TEXT UNIQUE)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) ON UPDATE CASCADE, code TEXT REFERENCES parent(code) ON UPDATE CASCADE)")
	execMust(t, e, "INSERT INTO parent (id, code) VALUES (1, 'a')")
	execMust(t, e, "INSERT INTO child (id, parent_id, code) VALUES (10, 1, 'a')")

	execMust(t, e, "UPDATE parent SET id = 5 WHERE id = 1")
	if res := execMust(t, e, "SELECT parent_id, code FROM child WHERE id = 10"); res.Rows[0][0] != int64(5) || res.Rows[0][1] != "a" {
		t.Fatalf("child after primary-key cascade = %v, want parent_id 5", res.Rows)
	}
	execMust(t, e, "UPDATE parent SET code = 'b' WHERE id = 5")
	if res := execMust(t, e, "SELECT parent_id, code FROM child WHERE id = 10"); res.Rows[0][0] != int64(5) || res.Rows[0][1] != "b" {
		t.Fatalf("child after unique-key cascade = %v, want code b", res.Rows)
	}

	execMust(t, e, "CREATE TABLE cparent (a TEXT, b INTEGER, PRIMARY KEY (a, b))")
	execMust(t, e, "CREATE TABLE cchild (id INTEGER PRIMARY KEY, pa TEXT, pb INTEGER, FOREIGN KEY (pa, pb) REFERENCES cparent(a, b) ON UPDATE CASCADE)")
	execMust(t, e, "INSERT INTO cparent (a, b) VALUES ('x', 1)")
	execMust(t, e, "INSERT INTO cchild (id, pa, pb) VALUES (10, 'x', 1)")
	execMust(t, e, "UPDATE cparent SET a = 'y', b = 2")
	if res := execMust(t, e, "SELECT pa, pb FROM cchild WHERE id = 10"); res.Rows[0][0] != "y" || res.Rows[0][1] != int64(2) {
		t.Fatalf("composite child after cascade = %v, want ('y', 2)", res.Rows)
	}

	// A cascaded key move onto an existing row reports the key components, not
	// the durable length-prefixed encoding.
	execMust(t, e, "INSERT INTO cparent (a, b) VALUES ('z', 3)")
	_, err := execSQL(e, "UPDATE cparent SET a = 'z', b = 3 WHERE a = 'y' AND b = 2")
	var duplicate *storage.PrimaryKeyViolationError
	if !errors.As(err, &duplicate) {
		t.Fatalf("colliding cascade error = %v, want PrimaryKeyViolationError", err)
	}
	if got, want := err.Error(), "duplicate primary key: ('z',3)"; got != want {
		t.Fatalf("colliding cascade message = %q, want %q", got, want)
	}
}

// SET NULL clears the child columns on delete and on a key update, and a NOT
// NULL child column rejects the action with 23502 without changing anything.
func TestForeignKeySetNull(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) ON DELETE SET NULL ON UPDATE SET NULL)")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")

	execMust(t, e, "UPDATE parent SET id = 2 WHERE id = 1")
	if res := execMust(t, e, "SELECT parent_id FROM child WHERE id = 10"); res.Rows[0][0] != nil {
		t.Fatalf("parent_id after ON UPDATE SET NULL = %v, want NULL", res.Rows[0][0])
	}
	execMust(t, e, "UPDATE child SET parent_id = 2 WHERE id = 10")
	execMust(t, e, "DELETE FROM parent WHERE id = 2")
	if res := execMust(t, e, "SELECT parent_id FROM child WHERE id = 10"); res.Rows[0][0] != nil {
		t.Fatalf("parent_id after ON DELETE SET NULL = %v, want NULL", res.Rows[0][0])
	}

	execMust(t, e, "CREATE TABLE required_parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE required_child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES required_parent(id) ON DELETE SET NULL)")
	execMust(t, e, "INSERT INTO required_parent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO required_child (id, parent_id) VALUES (10, 1)")

	_, err := execSQL(e, "DELETE FROM required_parent WHERE id = 1")
	requireNotNullViolation(t, err)
	if got := countRows(t, e, "required_parent"); got != 1 {
		t.Fatalf("required_parent rows after rejected cascade = %d, want 1", got)
	}
	if res := execMust(t, e, "SELECT parent_id FROM required_child WHERE id = 10"); res.Rows[0][0] != int64(1) {
		t.Fatalf("required_child.parent_id after rejected cascade = %v, want 1", res.Rows[0][0])
	}
}

// SET DEFAULT rewrites the child columns to their declared defaults, honoring
// NOT NULL, CHECK, and FOREIGN KEY on the resulting row.
func TestForeignKeySetDefault(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER DEFAULT 1 REFERENCES parent(id) ON DELETE SET DEFAULT ON UPDATE SET DEFAULT)")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1), (2), (4)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 2), (11, 4)")

	execMust(t, e, "DELETE FROM parent WHERE id = 2")
	if res := execMust(t, e, "SELECT parent_id FROM child WHERE id = 10"); res.Rows[0][0] != int64(1) {
		t.Fatalf("parent_id after ON DELETE SET DEFAULT = %v, want 1", res.Rows[0][0])
	}
	execMust(t, e, "UPDATE parent SET id = 3 WHERE id = 4")
	if res := execMust(t, e, "SELECT parent_id FROM child WHERE id = 11"); res.Rows[0][0] != int64(1) {
		t.Fatalf("parent_id after ON UPDATE SET DEFAULT = %v, want the stationary default 1", res.Rows[0][0])
	}

	// A default that violates CHECK rolls the whole delete back with 23514.
	execMust(t, e, "CREATE TABLE checked_parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE checked_child (id INTEGER PRIMARY KEY, parent_id INTEGER DEFAULT 0 CHECK (parent_id > 0) REFERENCES checked_parent(id) ON DELETE SET DEFAULT)")
	execMust(t, e, "INSERT INTO checked_parent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO checked_child (id, parent_id) VALUES (10, 1)")
	_, err := execSQL(e, "DELETE FROM checked_parent WHERE id = 1")
	requireCheckViolation(t, err, "")
	if got := countRows(t, e, "checked_parent"); got != 1 {
		t.Fatalf("checked_parent rows after rejected cascade = %d, want 1", got)
	}

	// A NOT NULL column without a default yields NULL, which is rejected.
	execMust(t, e, "CREATE TABLE strict_parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE strict_child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES strict_parent(id) ON DELETE SET DEFAULT)")
	execMust(t, e, "INSERT INTO strict_parent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO strict_child (id, parent_id) VALUES (10, 1)")
	_, err = execSQL(e, "DELETE FROM strict_parent WHERE id = 1")
	requireNotNullViolation(t, err)

	// A default with no matching parent is a dangling reference at commit.
	execMust(t, e, "CREATE TABLE dangling_parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE dangling_child (id INTEGER PRIMARY KEY, parent_id INTEGER DEFAULT 99 REFERENCES dangling_parent(id) ON DELETE SET DEFAULT)")
	execMust(t, e, "INSERT INTO dangling_parent (id) VALUES (10)")
	execMust(t, e, "INSERT INTO dangling_child (id, parent_id) VALUES (20, 10)")
	_, err = execSQL(e, "DELETE FROM dangling_parent WHERE id = 10")
	requireForeignKeyViolation(t, err)
	if got := countRows(t, e, "dangling_child"); got != 1 {
		t.Fatalf("dangling_child rows after rejected cascade = %d, want 1", got)
	}
}

// A self-referential tree cascades transitively without visiting the deleted row
// as its own child, and ON UPDATE CASCADE rewrites a row that references itself.
func TestForeignKeySelfReferentialCascade(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE nodes (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE ON UPDATE CASCADE)")
	execMust(t, e, "INSERT INTO nodes (id, parent_id) VALUES (1, NULL), (2, 1), (3, 2), (4, NULL), (5, 5)")

	execMust(t, e, "DELETE FROM nodes WHERE id = 1")
	if res := execMust(t, e, "SELECT id FROM nodes ORDER BY id"); res.RowCount != 2 || res.Rows[0][0] != int64(4) || res.Rows[1][0] != int64(5) {
		t.Fatalf("nodes after cascading delete = %v, want 4 and 5", res.Rows)
	}
	// A row that references itself leaves with its own reference.
	execMust(t, e, "DELETE FROM nodes WHERE id = 5")
	if got := countRows(t, e, "nodes"); got != 1 {
		t.Fatalf("nodes after self-referencing delete = %d, want 1", got)
	}

	execMust(t, e, "DELETE FROM nodes")
	execMust(t, e, "INSERT INTO nodes (id, parent_id) VALUES (1, 1), (2, 1)")
	execMust(t, e, "UPDATE nodes SET id = 10 WHERE id = 1")
	if res := execMust(t, e, "SELECT id, parent_id FROM nodes ORDER BY id"); res.RowCount != 2 ||
		res.Rows[0][0] != int64(2) || res.Rows[0][1] != int64(10) ||
		res.Rows[1][0] != int64(10) || res.Rows[1][1] != int64(10) {
		t.Fatalf("nodes after self-referencing key update = %v, want (2,10) and (10,10)", res.Rows)
	}
}

// installForeignKey appends a foreign key to table's persisted catalog. A
// two-table cycle cannot be declared through CREATE TABLE because a forward
// reference to a table that does not exist yet is rejected, so the test writes
// the durable catalog directly and reopens the database.
func installForeignKey(t *testing.T, pool *storage.KVPool, table string, fk storage.ForeignKeyConstraint) {
	t.Helper()
	schema := storage.NewSchemaManager(pool, "testdb")
	stored, err := schema.GetSchema(table)
	if err != nil {
		t.Fatalf("get schema %s: %v", table, err)
	}
	stored.ForeignKeys = append(stored.ForeignKeys, fk)
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal schema %s: %v", table, err)
	}
	key := "testdb:_schema:" + strings.ToLower(table)
	if err := pool.WithClient(func(c *storage.KVClient) error { return c.Write(key, string(data)) }); err != nil {
		t.Fatalf("write schema %s: %v", table, err)
	}
}

// Two tables that reference each other form a cascade cycle. Deleting or
// re-keying a row must terminate instead of bouncing between the tables.
func TestForeignKeyMutualCascadeCycleTerminates(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE a (id INTEGER PRIMARY KEY, b_id INTEGER)")
	execMust(t, e, "CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id) ON DELETE CASCADE ON UPDATE CASCADE)")
	installForeignKey(t, pool, "a", storage.ForeignKeyConstraint{
		Name:       "a_b_fk",
		Columns:    []string{"b_id"},
		RefTable:   "b",
		RefColumns: []string{"id"},
		OnDelete:   "CASCADE",
		OnUpdate:   "CASCADE",
	})

	reloaded := storage.NewSchemaManager(pool, "testdb")
	cyc := newExec(reloaded, storage.NewTableManager(pool, reloaded, "testdb"))

	insertCycle := func(aID, bID int) {
		t.Helper()
		execMust(t, cyc, "BEGIN")
		execMust(t, cyc, fmt.Sprintf("INSERT INTO b (id, a_id) VALUES (%d, %d)", bID, aID))
		execMust(t, cyc, fmt.Sprintf("INSERT INTO a (id, b_id) VALUES (%d, %d)", aID, bID))
		execMust(t, cyc, "COMMIT")
	}

	insertCycle(1, 1)
	execMust(t, cyc, "DELETE FROM a WHERE id = 1")
	if got := countRows(t, cyc, "a") + countRows(t, cyc, "b"); got != 0 {
		t.Fatalf("cycle rows after cascading delete = %d, want 0", got)
	}

	insertCycle(1, 1)
	execMust(t, cyc, "UPDATE a SET id = 2 WHERE id = 1")
	res := execMust(t, cyc, "SELECT id, b_id FROM a")
	if res.RowCount != 1 || res.Rows[0][0] != int64(2) || res.Rows[0][1] != int64(1) {
		t.Fatalf("a after cycle key update = %v, want (2,1)", res.Rows)
	}
	res = execMust(t, cyc, "SELECT id, a_id FROM b")
	if res.RowCount != 1 || res.Rows[0][0] != int64(1) || res.Rows[0][1] != int64(2) {
		t.Fatalf("b after cycle key update = %v, want (1,2)", res.Rows)
	}
	execMust(t, cyc, "UPDATE b SET id = 5 WHERE id = 1")
	res = execMust(t, cyc, "SELECT id, b_id FROM a")
	if res.RowCount != 1 || res.Rows[0][0] != int64(2) || res.Rows[0][1] != int64(5) {
		t.Fatalf("a after second cycle key update = %v, want (2,5)", res.Rows)
	}
}

// NO ACTION and RESTRICT keep the immediate 23503 behavior.
func TestForeignKeyNoActionRestrictStillViolate(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE restricting (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) ON DELETE RESTRICT ON UPDATE RESTRICT)")
	execMust(t, e, "CREATE TABLE no_action (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO restricting (id, parent_id) VALUES (10, 1)")
	execMust(t, e, "INSERT INTO no_action (id, parent_id) VALUES (20, 1)")

	_, err := execSQL(e, "DELETE FROM parent WHERE id = 1")
	requireForeignKeyViolation(t, err)
	_, err = execSQL(e, "UPDATE parent SET id = 2 WHERE id = 1")
	requireForeignKeyViolation(t, err)

	execMust(t, e, "DELETE FROM restricting")
	_, err = execSQL(e, "DELETE FROM parent WHERE id = 1")
	requireForeignKeyViolation(t, err)
	execMust(t, e, "DELETE FROM no_action")
	execMust(t, e, "DELETE FROM parent WHERE id = 1")
}

// A cascaded row that is itself referenced by a NO ACTION child aborts the whole
// statement; no partially cascaded state is durable.
func TestForeignKeyCascadeDownstreamFailureIsAtomic(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE grandparent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY, gp_id INTEGER REFERENCES grandparent(id) ON DELETE CASCADE)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO grandparent (id) VALUES (1), (2)")
	execMust(t, e, "INSERT INTO parent (id, gp_id) VALUES (10, 1), (20, 2)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (100, 10)")

	_, err := execSQL(e, "DELETE FROM grandparent WHERE id = 1")
	requireForeignKeyViolation(t, err)
	if got := countRows(t, e, "grandparent"); got != 2 {
		t.Fatalf("grandparent rows after rejected cascade = %d, want 2", got)
	}
	if got := countRows(t, e, "parent"); got != 2 {
		t.Fatalf("parent rows after rejected cascade = %d, want 2", got)
	}
	if got := countRows(t, e, "child"); got != 1 {
		t.Fatalf("child rows after rejected cascade = %d, want 1", got)
	}

	// A multi-row delete is atomic: the row with the blocked chain also keeps
	// the unrelated row in place.
	_, err = execSQL(e, "DELETE FROM grandparent")
	requireForeignKeyViolation(t, err)
	if got := countRows(t, e, "grandparent"); got != 2 {
		t.Fatalf("grandparent rows after rejected scan delete = %d, want 2", got)
	}
	if got := countRows(t, e, "parent"); got != 2 {
		t.Fatalf("parent rows after rejected scan delete = %d, want 2", got)
	}

	// Removing the downstream reference lets the same delete cascade.
	execMust(t, e, "DELETE FROM child")
	execMust(t, e, "DELETE FROM grandparent")
	if got := countRows(t, e, "grandparent") + countRows(t, e, "parent"); got != 0 {
		t.Fatalf("rows after successful cascade = %d, want 0", got)
	}
}

// A cascade inside an explicit transaction interacts with the commit-time final
// state check: a consistent result commits and a dangling one does not.
func TestForeignKeyCascadeInTransaction(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE)")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1), (2)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1), (11, 1), (20, 2)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "DELETE FROM parent WHERE id = 1")
	execMust(t, e, "SELECT count(*) FROM child")
	execMust(t, e, "COMMIT")
	if got := countRows(t, e, "child"); got != 1 {
		t.Fatalf("child rows after transactional cascade = %d, want 1", got)
	}

	// Re-adding a child under the deleted parent makes the final state dangling,
	// so the commit is rejected and the cascade is discarded.
	execMust(t, e, "BEGIN")
	execMust(t, e, "DELETE FROM parent WHERE id = 2")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (99, 2)")
	_, err := execSQL(e, "COMMIT")
	requireForeignKeyViolation(t, err)
	if got := countRows(t, e, "parent"); got != 1 {
		t.Fatalf("parent rows after rejected commit = %d, want 1", got)
	}
	if res := execMust(t, e, "SELECT id, parent_id FROM child ORDER BY id"); res.RowCount != 1 || res.Rows[0][0] != int64(20) {
		t.Fatalf("child rows after rejected commit = %v, want only row 20", res.Rows)
	}

	// A point delete inside a transaction cascades too.
	execMust(t, e, "BEGIN")
	execMust(t, e, "DELETE FROM parent WHERE id = 2")
	execMust(t, e, "COMMIT")
	if got := countRows(t, e, "parent") + countRows(t, e, "child"); got != 0 {
		t.Fatalf("rows after transactional point cascade = %d, want 0", got)
	}
}

// Referential actions persist with the schema and are enforced after a reload.
func TestForeignKeyCascadeDurableReload(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, CONSTRAINT child_parent FOREIGN KEY (parent_id) REFERENCES parent(id) ON DELETE CASCADE ON UPDATE CASCADE)")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")

	reloaded := storage.NewSchemaManager(pool, "testdb")
	stored, err := reloaded.GetSchema("child")
	if err != nil {
		t.Fatalf("reload schema: %v", err)
	}
	if len(stored.ForeignKeys) != 1 || stored.ForeignKeys[0].OnDelete != "CASCADE" || stored.ForeignKeys[0].OnUpdate != "CASCADE" {
		t.Fatalf("reloaded foreign keys = %+v, want ON DELETE/UPDATE CASCADE", stored.ForeignKeys)
	}

	reopened := newExec(reloaded, storage.NewTableManager(pool, reloaded, "testdb"))
	execMust(t, reopened, "UPDATE parent SET id = 2 WHERE id = 1")
	if res := execMust(t, reopened, "SELECT parent_id FROM child WHERE id = 10"); res.Rows[0][0] != int64(2) {
		t.Fatalf("child after reloaded ON UPDATE CASCADE = %v, want 2", res.Rows[0][0])
	}
	execMust(t, reopened, "DELETE FROM parent WHERE id = 2")
	if got := countRows(t, reopened, "child"); got != 0 {
		t.Fatalf("child rows after reloaded ON DELETE CASCADE = %d, want 0", got)
	}
}

// The emulated SQLite catalog reproduces every accepted referential action.
func TestForeignKeyCascadeInSQLiteCatalog(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, CONSTRAINT child_parent FOREIGN KEY (parent_id) REFERENCES parent(id) ON DELETE CASCADE ON UPDATE SET NULL)")
	execMust(t, e, "CREATE TABLE defaulted (id INTEGER PRIMARY KEY, parent_id INTEGER DEFAULT 1 REFERENCES parent(id) ON DELETE SET DEFAULT)")

	res := execMust(t, e, "SELECT sql FROM sqlite_master WHERE name = 'child'")
	sql, _ := res.Rows[0][0].(string)
	want := "CONSTRAINT `child_parent` FOREIGN KEY (`parent_id`) REFERENCES `parent` (`id`) ON DELETE CASCADE ON UPDATE SET NULL"
	if !strings.Contains(sql, want) {
		t.Fatalf("catalog SQL %q does not contain %q", sql, want)
	}
	res = execMust(t, e, "SELECT sql FROM sqlite_master WHERE name = 'defaulted'")
	sql, _ = res.Rows[0][0].(string)
	if !strings.Contains(sql, "ON DELETE SET DEFAULT") {
		t.Fatalf("catalog SQL %q does not contain the SET DEFAULT action", sql)
	}
}

// The capability document advertises the supported referential actions.
func TestForeignKeyCascadeCapability(t *testing.T) {
	notes := capabilities.Current().Constraints.ForeignKey.Notes
	for _, want := range []string{"CASCADE", "SET NULL", "SET DEFAULT", "autocommit", "no later than COMMIT", "O(FK component)"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("foreign_key capability notes = %q, want %q", notes, want)
		}
	}
}

// A cascading delete that races a child insert must not deadlock on the
// database-wide foreign-key write gate. Either statement may lose (a
// serialization failure or a 23503), but the pair always completes and never
// leaves a dangling child row.
func TestForeignKeyCascadeConcurrentDeleteNoDeadlock(t *testing.T) {
	_, schema, table := newTestDB(t)
	deleter := newExec(schema, table)
	inserter := newExec(schema, table)
	execMust(t, inserter, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, inserter, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE)")

	const rounds = 40
	for round := 1; round <= rounds; round++ {
		execMust(t, inserter, fmt.Sprintf("INSERT INTO parent (id) VALUES (%d)", round))

		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := execSQL(deleter, fmt.Sprintf("DELETE FROM parent WHERE id = %d", round))
			results <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := execSQL(inserter, fmt.Sprintf("INSERT INTO child (id, parent_id) VALUES (%d, %d)", round, round))
			results <- err
		}()
		close(start)

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("round %d: cascading delete and child insert deadlocked", round)
		}
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
				continue
			}
			var violation *storage.ForeignKeyViolationError
			if !errors.As(err, &violation) && !errors.Is(err, storage.ErrSerialization) {
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if successes == 0 {
			t.Fatalf("round %d: both statements failed", round)
		}

		dangling := execMust(t, inserter, "SELECT count(*) FROM child c WHERE NOT EXISTS (SELECT 1 FROM parent p WHERE p.id = c.parent_id)")
		if dangling.Rows[0][0] != int64(0) {
			t.Fatalf("round %d: %v dangling child rows", round, dangling.Rows[0][0])
		}
		execMust(t, inserter, "DELETE FROM child")
		execMust(t, inserter, fmt.Sprintf("DELETE FROM parent WHERE id = %d", round))
	}
}

// A cascaded rewrite moves the child's primary key when a foreign-key column is
// part of it.
func TestForeignKeyCascadeChildPrimaryKeyMoves(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER, parent_id INTEGER REFERENCES parent(id) ON UPDATE CASCADE ON DELETE CASCADE, PRIMARY KEY (parent_id, id))")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1), (11, 1)")

	execMust(t, e, "UPDATE parent SET id = 2 WHERE id = 1")
	res := execMust(t, e, "SELECT id, parent_id FROM child ORDER BY id")
	if res.RowCount != 2 || res.Rows[0][1] != int64(2) || res.Rows[1][1] != int64(2) {
		t.Fatalf("child rows after composite key move = %v, want both parent_id 2", res.Rows)
	}

	execMust(t, e, "DELETE FROM parent WHERE id = 2")
	if got := countRows(t, e, "child"); got != 0 {
		t.Fatalf("child rows after cascade = %d, want 0", got)
	}
}

// A cascade that makes a UNIQUE index collide rolls back the whole delete.
func TestForeignKeyCascadeUniqueViolationRollsBack(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER UNIQUE DEFAULT 1 REFERENCES parent(id) ON DELETE SET DEFAULT)")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1), (2)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1), (11, 2)")

	_, err := execSQL(e, "DELETE FROM parent WHERE id = 2")
	if err == nil {
		t.Fatal("cascade with a UNIQUE collision was accepted")
	}
	var violation *storage.UniqueViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("error = %v, want UniqueViolationError", err)
	}
	if got := countRows(t, e, "parent"); got != 2 {
		t.Fatalf("parent rows after rejected cascade = %d, want 2", got)
	}
	if res := execMust(t, e, "SELECT id, parent_id FROM child ORDER BY id"); res.RowCount != 2 || res.Rows[1][1] != int64(2) {
		t.Fatalf("child rows after rejected cascade = %v, want the original values", res.Rows)
	}
}

// Inside an explicit transaction the commit-time foreign-key closure validation
// rejects a cascade that leaves a downstream NO ACTION row dangling, and the
// whole transaction is discarded.
func TestForeignKeyCascadeTransactionDownstreamFailure(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE grandparent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY, gp_id INTEGER REFERENCES grandparent(id) ON DELETE CASCADE)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO grandparent (id) VALUES (1)")
	execMust(t, e, "INSERT INTO parent (id, gp_id) VALUES (10, 1)")
	execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (100, 10)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "DELETE FROM grandparent WHERE id = 1")
	_, err := execSQL(e, "COMMIT")
	requireForeignKeyViolation(t, err)
	if got := countRows(t, e, "grandparent"); got != 1 {
		t.Fatalf("grandparent rows after rejected commit = %d, want 1", got)
	}
	if got := countRows(t, e, "parent"); got != 1 {
		t.Fatalf("parent rows after rejected commit = %d, want 1", got)
	}
	if got := countRows(t, e, "child"); got != 1 {
		t.Fatalf("child rows after rejected commit = %d, want 1", got)
	}

	// The same transaction commits once the downstream row is part of the
	// cascade itself.
	execMust(t, e, "BEGIN")
	execMust(t, e, "DELETE FROM child WHERE id = 100")
	execMust(t, e, "DELETE FROM grandparent WHERE id = 1")
	execMust(t, e, "COMMIT")
	if got := countRows(t, e, "grandparent") + countRows(t, e, "parent") + countRows(t, e, "child"); got != 0 {
		t.Fatalf("rows after committed cascade = %d, want 0", got)
	}
}

// A referenced UNIQUE column may be nullable: moving it to NULL cascades the
// NULL into the child, which leaves the reference in its MATCH SIMPLE form.
func TestForeignKeyUpdateCascadeToNull(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY, code TEXT UNIQUE)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, code TEXT REFERENCES parent(code) ON UPDATE CASCADE)")
	execMust(t, e, "INSERT INTO parent (id, code) VALUES (1, 'x')")
	execMust(t, e, "INSERT INTO child (id, code) VALUES (10, 'x')")

	execMust(t, e, "UPDATE parent SET code = NULL WHERE id = 1")
	if res := execMust(t, e, "SELECT code FROM child WHERE id = 10"); res.Rows[0][0] != nil {
		t.Fatalf("child.code after cascade to NULL = %v, want NULL", res.Rows[0][0])
	}
}

// UPDATE ... FROM that moves a parent key must apply ON UPDATE actions like a
// plain UPDATE. The executor used to implement the move as DELETE + INSERT,
// which fired ON DELETE CASCADE and silently dropped the children.
func TestForeignKeyUpdateFromPrimaryKeyMoveCascades(t *testing.T) {
	for _, tc := range []struct {
		name       string
		constraint string
	}{
		{name: "on delete and update cascade", constraint: "ON DELETE CASCADE ON UPDATE CASCADE"},
		{name: "on update cascade only", constraint: "ON UPDATE CASCADE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, schema, table := newTestDB(t)
			e := newExec(schema, table)
			execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
			execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) "+tc.constraint+")")
			execMust(t, e, "CREATE TABLE src (id INTEGER PRIMARY KEY, new_id INTEGER)")
			execMust(t, e, "INSERT INTO parent (id) VALUES (1)")
			execMust(t, e, "INSERT INTO child (id, parent_id) VALUES (10, 1)")
			execMust(t, e, "INSERT INTO src (id, new_id) VALUES (1, 2)")

			res := execMust(t, e, "UPDATE parent SET id = src.new_id FROM src WHERE src.id = parent.id")
			if res.RowsAffected != 1 {
				t.Fatalf("rows affected = %d, want 1", res.RowsAffected)
			}
			if got := countRows(t, e, "child"); got != 1 {
				t.Fatalf("child rows after UPDATE ... FROM key move = %d, want 1 (children were deleted)", got)
			}
			res = execMust(t, e, "SELECT id, parent_id FROM child")
			if res.Rows[0][0] != int64(10) || res.Rows[0][1] != int64(2) {
				t.Fatalf("child after UPDATE ... FROM key move = %v, want (10, 2)", res.Rows)
			}
			if res := execMust(t, e, "SELECT id FROM parent"); res.RowCount != 1 || res.Rows[0][0] != int64(2) {
				t.Fatalf("parent after UPDATE ... FROM key move = %v, want id 2", res.Rows)
			}
		})
	}
}

// A cascade failure inside an explicit transaction is detected at the statement
// and rolls back to the statement savepoint, so the transaction can continue.
// A plain NO ACTION/RESTRICT violation is instead deferred to COMMIT, and the
// statement itself succeeds.
func TestForeignKeyCascadeTransactionErrorTiming(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE required (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id) ON DELETE SET NULL)")
	execMust(t, e, "CREATE TABLE restricted (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent (id) VALUES (1), (2)")
	execMust(t, e, "INSERT INTO required (id, parent_id) VALUES (10, 1)")

	// SET NULL on a NOT NULL child fails at the statement with 23502.
	execMust(t, e, "BEGIN")
	_, err := execSQL(e, "DELETE FROM parent WHERE id = 1")
	requireNotNullViolation(t, err)
	// The failed cascade rolled back to the statement savepoint: the parent and
	// child are still visible to the transaction and the transaction is usable.
	if res := execMust(t, e, "SELECT count(*) FROM parent"); res.Rows[0][0] != int64(2) {
		t.Fatalf("parent rows after statement rollback = %v, want 2", res.Rows)
	}
	if res := execMust(t, e, "SELECT parent_id FROM required WHERE id = 10"); res.Rows[0][0] != int64(1) {
		t.Fatalf("required.parent_id after statement rollback = %v, want 1", res.Rows[0][0])
	}
	execMust(t, e, "ROLLBACK")

	// A NO ACTION/RESTRICT violation is deferred to COMMIT: the DELETE succeeds
	// as a statement and the transaction fails on commit.
	execMust(t, e, "INSERT INTO restricted (id, parent_id) VALUES (20, 2)")
	execMust(t, e, "BEGIN")
	execMust(t, e, "DELETE FROM parent WHERE id = 2")
	_, err = execSQL(e, "COMMIT")
	requireForeignKeyViolation(t, err)
	if res := execMust(t, e, "SELECT count(*) FROM parent"); res.Rows[0][0] != int64(2) {
		t.Fatalf("parent rows after rejected commit = %v, want 2", res.Rows)
	}
}
