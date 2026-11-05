package executor

import (
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// transactional DDL: CREATE TABLE and its rows are visible to the transaction
// that issued them, invisible to other sessions, and discarded by ROLLBACK.
func TestTransactionalDDLCreateTableRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	writer := newExec(schema, table)
	observer := newExec(schema, table)

	execMust(t, writer, "BEGIN")
	execMust(t, writer, "CREATE TABLE staged (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, writer, "INSERT INTO staged VALUES (1, 'one')")

	// Read-your-writes inside the transaction.
	if res := execMust(t, writer, "SELECT v FROM staged WHERE id = 1"); res.RowCount != 1 || res.Rows[0][0] != "one" {
		t.Fatalf("staged table read = %v", res.Rows)
	}
	// Another session must not see the staged table.
	if _, err := execSQL(observer, "SELECT * FROM staged"); err == nil {
		t.Fatal("observer saw a table created in another session's uncommitted transaction")
	}

	execMust(t, writer, "ROLLBACK")

	if _, err := execSQL(writer, "SELECT * FROM staged"); err == nil {
		t.Fatal("staged table survived ROLLBACK")
	}
	if schema.TableExists("staged") {
		t.Fatal("durable catalog contains a rolled-back table")
	}
	if _, err := schema.GetSchema("staged"); err == nil {
		t.Fatal("durable catalog still resolves the rolled-back table")
	}
	// DDL and its rows are one unit: the staged INSERT left no durable rows.
	if rows, err := table.Select("staged", nil); err == nil && len(rows) > 0 {
		t.Fatalf("rolled-back table left %d durable rows", len(rows))
	}
}

// transactional DDL: DROP TABLE is undone by ROLLBACK, rows included.
func TestTransactionalDDLDropTableRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE ledger (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO ledger VALUES (1, 'a'), (2, 'b')")

	execMust(t, e, "BEGIN")
	execMust(t, e, "DROP TABLE ledger")
	if _, err := execSQL(e, "SELECT * FROM ledger"); err == nil {
		t.Fatal("dropped table is still visible to the transaction")
	}
	execMust(t, e, "ROLLBACK")

	res := execMust(t, e, "SELECT id, v FROM ledger ORDER BY id")
	if res.RowCount != 2 {
		t.Fatalf("rows after rollback = %v", res.Rows)
	}
	// The catalog and the rows must both be intact after the rollback.
	if !schema.TableExists("ledger") {
		t.Fatal("durable catalog lost the table")
	}
	row, err := table.GetByPK("ledger", "1")
	if err != nil || row["v"] != "a" {
		t.Fatalf("durable row after rollback: %v %v", row, err)
	}
}

// transactional DDL: ALTER TABLE variants are undone by ROLLBACK.
func TestTransactionalDDLAlterRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE people (id INTEGER PRIMARY KEY, name TEXT)")
	execMust(t, e, "INSERT INTO people VALUES (1, 'ada')")

	execMust(t, e, "BEGIN")
	execMust(t, e, "ALTER TABLE people ADD COLUMN age INTEGER DEFAULT 30")
	// Read-your-writes: the new column exists for this session and carries the
	// backfilled default.
	res := execMust(t, e, "SELECT age FROM people WHERE id = 1")
	if res.RowCount != 1 || res.Rows[0][0] != int64(30) {
		t.Fatalf("staged ALTER ADD COLUMN read = %v", res.Rows)
	}
	execMust(t, e, "ALTER TABLE people RENAME COLUMN name TO full_name")
	if res := execMust(t, e, "SELECT full_name FROM people"); res.Rows[0][0] != "ada" {
		t.Fatalf("staged RENAME COLUMN read = %v", res.Rows)
	}
	execMust(t, e, "ALTER TABLE people RENAME TO humans")
	if res := execMust(t, e, "SELECT full_name FROM humans"); res.Rows[0][0] != "ada" {
		t.Fatalf("staged RENAME TABLE read = %v", res.Rows)
	}
	execMust(t, e, "ROLLBACK")

	res = execMust(t, e, "SELECT id, name FROM people")
	if res.RowCount != 1 || res.Rows[0][1] != "ada" {
		t.Fatalf("rows after rollback = %v", res.Rows)
	}
	if _, err := execSQL(e, "SELECT age FROM people"); err == nil {
		t.Fatal("rolled-back ADD COLUMN is still visible")
	}
	if _, err := execSQL(e, "SELECT * FROM humans"); err == nil {
		t.Fatal("rolled-back RENAME TABLE is still visible")
	}
	if _, err := execSQL(e, "SELECT * FROM people"); err != nil {
		t.Fatalf("original table name lost by ROLLBACK: %v", err)
	}
}

// transactional DDL: CREATE INDEX is transaction-scoped.
func TestTransactionalDDLCreateIndexRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE items (id INTEGER PRIMARY KEY, sku TEXT)")
	execMust(t, e, "INSERT INTO items VALUES (1, 'a'), (2, 'b')")

	execMust(t, e, "BEGIN")
	execMust(t, e, "CREATE UNIQUE INDEX idx_items_sku ON items (sku)")
	if !e.session.IndexExists("idx_items_sku") {
		t.Fatal("staged index missing from the transaction's catalog")
	}
	if schema.IndexExists("idx_items_sku") {
		t.Fatal("staged index leaked into the committed catalog")
	}
	execMust(t, e, "ROLLBACK")

	if schema.IndexExists("idx_items_sku") {
		t.Fatal("rolled-back index survived in the durable catalog")
	}
	// The index name is reusable, which proves the catalog write was discarded.
	execMust(t, e, "CREATE INDEX idx_items_sku ON items (sku)")
}

// transactional DDL: another session keeps reading the committed catalog until
// the DDL commit publishes it.
func TestTransactionalDDLTwoSessionVisibility(t *testing.T) {
	_, schema, table := newTestDB(t)
	writer := newExec(schema, table)
	observer := newExec(schema, table)

	execMust(t, writer, "CREATE TABLE base (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, observer, "SELECT * FROM base")
	execMust(t, writer, "INSERT INTO base VALUES (1, 'committed')")

	execMust(t, writer, "BEGIN")
	execMust(t, writer, "CREATE TABLE extra (id INTEGER PRIMARY KEY)")
	execMust(t, writer, "ALTER TABLE base ADD COLUMN note TEXT DEFAULT 'new'")
	execMust(t, writer, "CREATE INDEX idx_base_note ON base (note)")

	// The observer sees neither the new table nor the new column, and it still
	// reads the committed rows.
	if _, err := execSQL(observer, "SELECT * FROM extra"); err == nil {
		t.Fatal("observer saw staged CREATE TABLE before COMMIT")
	}
	if _, err := execSQL(observer, "SELECT note FROM base"); err == nil {
		t.Fatal("observer saw a staged column before COMMIT")
	}
	if res := execMust(t, observer, "SELECT v FROM base"); res.RowCount != 1 || res.Rows[0][0] != "committed" {
		t.Fatalf("observer read = %v", res.Rows)
	}

	execMust(t, writer, "COMMIT")

	if _, err := execSQL(observer, "SELECT * FROM extra"); err != nil {
		t.Fatalf("observer did not see committed CREATE TABLE: %v", err)
	}
	// The observer's executor resynchronizes its analyzer catalog from the
	// bumped catalog version.
	res := execMust(t, observer, "SELECT note FROM base")
	if res.RowCount != 1 || res.Rows[0][0] != "new" {
		t.Fatalf("observer row after COMMIT = %v", res.Rows)
	}
	if !schema.IndexExists("idx_base_note") {
		t.Fatal("committed index missing from the durable catalog")
	}
}

// transactional DDL: COMMIT publishes every staged statement atomically and the
// result reloads from durable storage.
func TestTransactionalDDLCommitPublishesAndReloads(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE orders (id INTEGER PRIMARY KEY, total INTEGER)")
	execMust(t, e, "INSERT INTO orders VALUES (1, 10)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "ALTER TABLE orders ADD COLUMN region TEXT DEFAULT 'eu'")
	execMust(t, e, "CREATE TABLE shipments (id INTEGER PRIMARY KEY, order_id INTEGER)")
	execMust(t, e, "INSERT INTO shipments VALUES (100, 1)")
	execMust(t, e, "CREATE INDEX idx_orders_region ON orders (region)")
	execMust(t, e, "COMMIT")

	// Reload the catalog from durable state with fresh managers over the same KV
	// server: the commit must have written schema, index, and rows together.
	reloadedSchemas := storage.NewSchemaManager(pool, "testdb")
	reloadedTables := storage.NewTableManager(pool, reloadedSchemas, "testdb")

	orders, err := reloadedSchemas.GetSchema("orders")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := orders.GetColumn("region"); !ok {
		t.Fatal("committed ADD COLUMN missing after reload")
	}
	if !reloadedSchemas.IndexExists("idx_orders_region") {
		t.Fatal("committed index missing after reload")
	}
	if !reloadedSchemas.TableExists("shipments") {
		t.Fatal("committed CREATE TABLE missing after reload")
	}
	orderRow, err := reloadedTables.GetByPK("orders", "1")
	if err != nil {
		t.Fatal(err)
	}
	if orderRow["region"] != "eu" {
		t.Fatalf("backfilled default after reload = %v", orderRow)
	}
	shipment, err := reloadedTables.GetByPK("shipments", "100")
	if err != nil || shipment["order_id"] != int64(1) {
		t.Fatalf("committed insert after reload: %v %v", shipment, err)
	}
}

// transactional DDL: DML and DDL commit or roll back as one unit.
func TestTransactionalDDLWithDML(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE events (id INTEGER PRIMARY KEY, kind TEXT)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "CREATE TABLE archive (id INTEGER PRIMARY KEY, kind TEXT)")
	execMust(t, e, "INSERT INTO archive VALUES (1, 'old')")
	execMust(t, e, "INSERT INTO events VALUES (1, 'new')")
	execMust(t, e, "ALTER TABLE events ADD COLUMN archived INTEGER DEFAULT 0")
	execMust(t, e, "ROLLBACK")

	if _, err := execSQL(e, "SELECT * FROM archive"); err == nil {
		t.Fatal("rollback kept a table created with DML in the same transaction")
	}
	if res := execMust(t, e, "SELECT COUNT(*) FROM events"); res.Rows[0][0] != int64(0) {
		t.Fatalf("rollback kept a DML row: %v", res.Rows)
	}
	if _, err := execSQL(e, "SELECT archived FROM events"); err == nil {
		t.Fatal("rollback kept a staged column")
	}

	// The same sequence commits as one unit.
	execMust(t, e, "BEGIN")
	execMust(t, e, "CREATE TABLE archive (id INTEGER PRIMARY KEY, kind TEXT)")
	execMust(t, e, "INSERT INTO archive VALUES (1, 'old')")
	execMust(t, e, "INSERT INTO events VALUES (1, 'new')")
	execMust(t, e, "ALTER TABLE events ADD COLUMN archived INTEGER DEFAULT 0")
	execMust(t, e, "COMMIT")

	if res := execMust(t, e, "SELECT COUNT(*) FROM archive"); res.Rows[0][0] != int64(1) {
		t.Fatalf("committed CREATE TABLE rows = %v", res.Rows)
	}
	if res := execMust(t, e, "SELECT archived FROM events WHERE id = 1"); res.Rows[0][0] != int64(0) {
		t.Fatalf("committed ADD COLUMN default = %v", res.Rows)
	}
}

// Transactional DDL: a rejected statement leaves the transaction's prior schema
// intact, including DDL staged before it.
func TestTransactionalDDLFailureLeavesPriorSchema(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))")
	execMust(t, e, "INSERT INTO parent VALUES (1)")
	execMust(t, e, "INSERT INTO child VALUES (10, 1)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "CREATE TABLE scratch (id INTEGER PRIMARY KEY)")
	// Duplicate primary key inside the staged table fails the statement.
	if _, err := execSQL(e, "INSERT INTO scratch VALUES (1), (1)"); err == nil {
		t.Fatal("duplicate primary key accepted")
	}
	// Dropping a referenced parent is rejected.
	if _, err := execSQL(e, "DROP TABLE parent"); err == nil {
		t.Fatal("dropped a table another table references")
	}
	// Adding a NOT NULL column without a default to a populated table fails.
	if _, err := execSQL(e, "ALTER TABLE child ADD COLUMN required TEXT NOT NULL"); err == nil {
		t.Fatal("NOT NULL column without default accepted on a populated table")
	}

	// Earlier staged DDL is still visible and staged DML is intact.
	if res := execMust(t, e, "SELECT COUNT(*) FROM scratch"); res.Rows[0][0] != int64(0) {
		t.Fatalf("scratch rows = %v", res.Rows)
	}
	if res := execMust(t, e, "SELECT COUNT(*) FROM child"); res.Rows[0][0] != int64(1) {
		t.Fatalf("child rows after failed statements = %v", res.Rows)
	}
	execMust(t, e, "COMMIT")

	if !schema.TableExists("scratch") {
		t.Fatal("committed staged table missing after a failed statement")
	}
	if schema.TableExists("scratch") && schema.TableExists("nonexistent") {
		t.Fatal("unreachable")
	}
}

// Transactional DDL: a multi-statement migration rolls back as one unit.
func TestTransactionalDDLMultiStatementMigrationRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE accounts (id INTEGER PRIMARY KEY, balance INTEGER)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "CREATE TABLE audit (id INTEGER PRIMARY KEY, account_id INTEGER)")
	execMust(t, e, "ALTER TABLE accounts ADD COLUMN currency TEXT DEFAULT 'EUR'")
	execMust(t, e, "CREATE UNIQUE INDEX idx_accounts_currency ON accounts (currency)")
	execMust(t, e, "INSERT INTO accounts VALUES (1, 100, 'EUR')")
	execMust(t, e, "INSERT INTO audit VALUES (1, 1)")
	execMust(t, e, "ROLLBACK")

	if schema.TableExists("audit") || schema.IndexExists("idx_accounts_currency") {
		t.Fatal("migration objects survived ROLLBACK")
	}
	accounts, err := schema.GetSchema("accounts")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := accounts.GetColumn("currency"); ok {
		t.Fatal("migration column survived ROLLBACK")
	}
}

// Transactional DDL: a UNIQUE index staged by the transaction is enforced by
// the commit's validation, so duplicate values never become durable.
func TestTransactionalDDLStagedUniqueIndexValidation(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE accounts (id INTEGER PRIMARY KEY, balance INTEGER)")
	execMust(t, e, "INSERT INTO accounts VALUES (1, 100)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "ALTER TABLE accounts ADD COLUMN currency TEXT DEFAULT 'EUR'")
	execMust(t, e, "CREATE UNIQUE INDEX idx_accounts_currency ON accounts (currency)")
	execMust(t, e, "INSERT INTO accounts (id, balance) VALUES (2, 200)")
	// Both rows now carry the backfilled default 'EUR', so the staged UNIQUE
	// index must reject the transaction at commit.
	if _, err := execSQL(e, "COMMIT"); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("commit with duplicate staged UNIQUE values = %v", err)
	}
	if schema.IndexExists("idx_accounts_currency") {
		t.Fatal("failed commit published the staged index")
	}
	if res := execMust(t, e, "SELECT COUNT(*) FROM accounts"); res.Rows[0][0] != int64(1) {
		t.Fatalf("failed commit published staged rows: %v", res.Rows)
	}
}

// Transactional DDL: validations run against the staged catalog.
func TestTransactionalDDLValidationsUseStagedSchema(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	execMust(t, e, "BEGIN")
	execMust(t, e, "CREATE TABLE parents (a INTEGER, b INTEGER, PRIMARY KEY (a, b))")
	execMust(t, e, "CREATE TABLE kids (id INTEGER PRIMARY KEY, pa INTEGER, pb INTEGER, FOREIGN KEY (pa, pb) REFERENCES parents(a, b))")
	execMust(t, e, "INSERT INTO parents VALUES (1, 2)")
	execMust(t, e, "INSERT INTO kids VALUES (10, 1, 2)")
	// Composite primary key: the same pair collides, a different pair does not.
	if _, err := execSQL(e, "INSERT INTO parents VALUES (1, 2)"); err == nil {
		t.Fatal("duplicate composite primary key accepted")
	}
	execMust(t, e, "INSERT INTO parents VALUES (3, 4)")
	// CHECK added by a staged ALTER TABLE is enforced by staged writes.
	execMust(t, e, "ALTER TABLE kids ADD COLUMN age INTEGER CHECK (age >= 0)")
	execMust(t, e, "INSERT INTO kids VALUES (12, 3, 4, 5)")
	if _, err := execSQL(e, "INSERT INTO kids VALUES (13, 3, 4, -1)"); err == nil {
		t.Fatal("staged CHECK constraint not enforced")
	}
	// NOT NULL added by a staged ALTER TABLE is enforced by staged writes.
	execMust(t, e, "ALTER TABLE kids ADD COLUMN tag TEXT NOT NULL DEFAULT 't'")
	if _, err := execSQL(e, "INSERT INTO kids (id, pa, pb, age, tag) VALUES (14, 3, 4, 1, NULL)"); err == nil {
		t.Fatal("staged NOT NULL constraint not enforced")
	}
	execMust(t, e, "COMMIT")

	// The staged constraints are durable and still enforced.
	if _, err := execSQL(e, "INSERT INTO kids VALUES (15, 3, 4, -1, 'x')"); err == nil {
		t.Fatal("committed CHECK constraint not enforced")
	}

	// A composite foreign key declared by staged DDL is validated at commit
	// against the transaction's final state.
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO kids (id, pa, pb) VALUES (11, 1, 9)")
	if _, err := execSQL(e, "COMMIT"); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("commit with a dangling staged composite foreign key = %v", err)
	}
	if res := execMust(t, e, "SELECT COUNT(*) FROM kids WHERE id = 11"); res.Rows[0][0] != int64(0) {
		t.Fatalf("failed commit published the dangling row: %v", res.Rows)
	}
	if _, err := execSQL(e, "INSERT INTO kids (id, pa, pb) VALUES (16, 8, 9)"); err == nil {
		t.Fatal("committed FOREIGN KEY not enforced")
	}
}

// Transactional DDL: cascades and composite keys keep working across a commit.
func TestTransactionalDDLCascadesAcrossCommit(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	execMust(t, e, "BEGIN")
	execMust(t, e, "CREATE TABLE parents (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE kids (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parents(id) ON DELETE CASCADE)")
	execMust(t, e, "INSERT INTO parents VALUES (1)")
	execMust(t, e, "INSERT INTO kids VALUES (10, 1)")
	execMust(t, e, "COMMIT")

	if res := execMust(t, e, "SELECT COUNT(*) FROM kids"); res.Rows[0][0] != int64(1) {
		t.Fatalf("kids = %v", res.Rows)
	}
	execMust(t, e, "DELETE FROM parents WHERE id = 1")
	if res := execMust(t, e, "SELECT COUNT(*) FROM kids"); res.Rows[0][0] != int64(0) {
		t.Fatalf("cascade did not delete the child row: %v", res.Rows)
	}
}

// Transactional DDL: a concurrent committed DDL that conflicts with the staged
// change fails the commit with a serialization error instead of corrupting the
// catalog.
func TestTransactionalDDLConcurrentConflict(t *testing.T) {
	_, schema, table := newTestDB(t)
	first := newExec(schema, table)
	second := newExec(schema, table)

	execMust(t, first, "BEGIN")
	execMust(t, first, "CREATE TABLE races (id INTEGER PRIMARY KEY)")

	// A second session creates the same table and commits first.
	execMust(t, second, "CREATE TABLE races (id INTEGER PRIMARY KEY, v TEXT)")

	if _, err := execSQL(first, "COMMIT"); err == nil {
		t.Fatal("conflicting DDL commit succeeded")
	} else if !strings.Contains(err.Error(), "serialization") {
		t.Fatalf("conflicting DDL commit error = %v, want a serialization error", err)
	}

	// The committed definition wins and stays intact.
	stored, err := schema.GetSchema("races")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.GetColumn("v"); !ok {
		t.Fatalf("concurrent committed schema was overwritten: %+v", stored.Columns)
	}
}

// Transactional DDL: a savepoint rollback also reverts staged DDL.
func TestTransactionalDDLSavepointRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "CREATE TABLE before_sp (id INTEGER PRIMARY KEY)")
	execMust(t, e, "SAVEPOINT sp1")
	execMust(t, e, "CREATE TABLE after_sp (id INTEGER PRIMARY KEY)")
	execMust(t, e, "ALTER TABLE t ADD COLUMN v TEXT DEFAULT 'x'")
	execMust(t, e, "ROLLBACK TO SAVEPOINT sp1")

	if _, err := execSQL(e, "SELECT * FROM after_sp"); err == nil {
		t.Fatal("DDL after the savepoint survived ROLLBACK TO SAVEPOINT")
	}
	if _, err := execSQL(e, "SELECT v FROM t"); err == nil {
		t.Fatal("ALTER after the savepoint survived ROLLBACK TO SAVEPOINT")
	}
	if _, err := execSQL(e, "SELECT * FROM before_sp"); err != nil {
		t.Fatalf("DDL before the savepoint was lost: %v", err)
	}
	execMust(t, e, "COMMIT")

	if !schema.TableExists("before_sp") || schema.TableExists("after_sp") {
		t.Fatal("committed savepoint result is wrong")
	}
}

// transactional DDL: DROP INDEX is undone by ROLLBACK and stays invisible to
// other sessions until COMMIT.
func TestTransactionalDDLDropIndexRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	writer := newExec(schema, table)
	observer := newExec(schema, table)
	execMust(t, writer, "CREATE TABLE docs (id INTEGER PRIMARY KEY, slug TEXT)")
	execMust(t, writer, "CREATE UNIQUE INDEX idx_docs_slug ON docs (slug)")
	execMust(t, writer, "INSERT INTO docs VALUES (1, 'a')")

	execMust(t, writer, "BEGIN")
	execMust(t, writer, "DROP INDEX idx_docs_slug")
	if writer.session.IndexExists("idx_docs_slug") {
		t.Fatal("staged DROP INDEX is still visible to the transaction")
	}
	if !observer.session.IndexExists("idx_docs_slug") {
		t.Fatal("staged DROP INDEX became visible to another session before COMMIT")
	}
	execMust(t, writer, "ROLLBACK")
	if !schema.IndexExists("idx_docs_slug") {
		t.Fatal("rollback did not restore the dropped index")
	}

	// The duplicate still fails, proving the unique index survived intact.
	if _, err := execSQL(writer, "INSERT INTO docs VALUES (2, 'a')"); err == nil {
		t.Fatal("unique index lost its enforcement after a rolled-back DROP INDEX")
	}

	execMust(t, writer, "BEGIN")
	execMust(t, writer, "DROP INDEX idx_docs_slug")
	execMust(t, writer, "COMMIT")
	if schema.IndexExists("idx_docs_slug") {
		t.Fatal("committed DROP INDEX did not reach the catalog")
	}
	execMust(t, observer, "INSERT INTO docs VALUES (3, 'a')")
}

// transactional DDL: a renamed table stays writable inside the transaction and
// the staged rows move with it at commit.
func TestTransactionalDDLRenameTableReadYourWrites(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE old_name (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO old_name VALUES (1, 'kept')")

	execMust(t, e, "BEGIN")
	execMust(t, e, "ALTER TABLE old_name RENAME TO new_name")
	execMust(t, e, "INSERT INTO new_name VALUES (2, 'staged')")
	if res := execMust(t, e, "SELECT v FROM new_name ORDER BY id"); res.RowCount != 2 {
		t.Fatalf("rows after staged rename = %v", res.Rows)
	}
	execMust(t, e, "COMMIT")

	if schema.TableExists("old_name") {
		t.Fatal("renamed table is still addressable under its old name")
	}
	res := execMust(t, e, "SELECT id, v FROM new_name ORDER BY id")
	if res.RowCount != 2 || res.Rows[0][1] != "kept" || res.Rows[1][1] != "staged" {
		t.Fatalf("rows after committed rename = %v", res.Rows)
	}
	// The durable rows moved rather than being copied.
	if rows, err := table.Select("old_name", nil); err == nil && len(rows) > 0 {
		t.Fatalf("old data prefix still holds %d rows", len(rows))
	}
}

// transactional DDL: another session keeps reading a dropped table's rows until
// the drop commits.
func TestTransactionalDDLDropTableTwoSessions(t *testing.T) {
	_, schema, table := newTestDB(t)
	writer := newExec(schema, table)
	observer := newExec(schema, table)
	execMust(t, writer, "CREATE TABLE doomed (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, writer, "INSERT INTO doomed VALUES (1, 'still here')")

	execMust(t, writer, "BEGIN")
	execMust(t, writer, "DROP TABLE doomed")
	if res := execMust(t, observer, "SELECT v FROM doomed"); res.RowCount != 1 || res.Rows[0][0] != "still here" {
		t.Fatalf("observer rows while the drop is uncommitted = %v", res.Rows)
	}
	execMust(t, writer, "COMMIT")

	if _, err := execSQL(observer, "SELECT * FROM doomed"); err == nil {
		t.Fatal("observer still reads a dropped table after COMMIT")
	}
	if rows, err := table.Select("doomed", nil); err == nil && len(rows) > 0 {
		t.Fatalf("dropped table left %d durable rows", len(rows))
	}
}

// transactional DDL: a write that commits after the DDL statement but before the
// DDL commit makes the commit fail with a serialization error rather than
// silently discarding the concurrent write.
func TestTransactionalDDLConcurrentWriteConflicts(t *testing.T) {
	_, schema, table := newTestDB(t)
	writer := newExec(schema, table)
	other := newExec(schema, table)
	execMust(t, writer, "CREATE TABLE shared (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, writer, "INSERT INTO shared VALUES (1, 'a')")

	execMust(t, writer, "BEGIN")
	execMust(t, writer, "ALTER TABLE shared ADD COLUMN note TEXT DEFAULT 'x'")

	// Another session writes the table between the DDL statement and the commit.
	execMust(t, other, "INSERT INTO shared VALUES (2, 'b')")

	if _, err := execSQL(writer, "COMMIT"); err == nil {
		t.Fatal("commit over a concurrent write succeeded")
	} else if !strings.Contains(err.Error(), "serialization") {
		t.Fatalf("commit error = %v, want a serialization error", err)
	}
	// The concurrent row is intact and the column was never published.
	if res := execMust(t, other, "SELECT COUNT(*) FROM shared"); res.Rows[0][0] != int64(2) {
		t.Fatalf("concurrent rows after failed commit = %v", res.Rows)
	}
	if _, err := execSQL(other, "SELECT note FROM shared"); err == nil {
		t.Fatal("failed commit published the staged column")
	}
}

// transactional DDL: a rolled-back DROP INDEX and a committed re-creation both
// leave the derived index usable, not stuck on a stale disabled marker.
func TestTransactionalDDLIndexCachesSurviveRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE docs (id INTEGER PRIMARY KEY, slug TEXT)")
	execMust(t, e, "CREATE INDEX idx_docs_slug ON docs (slug)")
	execMust(t, e, "INSERT INTO docs VALUES (1, 'a'), (2, 'b')")

	execMust(t, e, "BEGIN")
	execMust(t, e, "DROP INDEX idx_docs_slug")
	execMust(t, e, "ROLLBACK")
	if _, err := e.session.SelectByIndex("docs", "idx_docs_slug", "b"); err != nil {
		t.Fatalf("index unusable after rolling back DROP INDEX: %v", err)
	}
	// The rollback must not leave the shared derived caches describing the
	// staged drop: the index still resolves, still has its entries, and is not
	// stuck on the disabled marker ClearIndex sets.
	index, err := e.session.GetIndex("idx_docs_slug")
	if err != nil {
		t.Fatalf("index missing after rolling back DROP INDEX: %v", err)
	}
	if rows, err := e.session.SelectByIndex("docs", index.Name, "a"); err != nil || len(rows) != 1 {
		t.Fatalf("index entries after rolling back DROP INDEX: rows=%v err=%v", rows, err)
	}

	// A rolled-back CREATE INDEX must not leave derived entries or a cleared
	// disabled marker behind either.
	execMust(t, e, "BEGIN")
	execMust(t, e, "CREATE INDEX idx_docs_slug_rollback ON docs (slug)")
	if !e.session.IndexExists("idx_docs_slug_rollback") {
		t.Fatal("staged index missing from the transaction catalog")
	}
	execMust(t, e, "ROLLBACK")
	if _, err := e.session.GetIndex("idx_docs_slug_rollback"); err == nil {
		t.Fatal("rolled-back index definition survived")
	}
	if _, err := e.session.SelectByIndex("docs", "idx_docs_slug_rollback", "b"); err == nil {
		t.Fatal("rolled-back index left usable derived entries")
	}

	// A committed drop followed by a transactional re-creation must rebuild the
	// index rather than inherit the disabled marker.
	execMust(t, e, "DROP INDEX idx_docs_slug")
	execMust(t, e, "BEGIN")
	execMust(t, e, "CREATE INDEX idx_docs_slug ON docs (slug)")
	execMust(t, e, "COMMIT")
	if _, err := e.session.SelectByIndex("docs", "idx_docs_slug", "b"); err != nil {
		t.Fatalf("index unusable after committing a re-created index: %v", err)
	}
}

// Transactional DDL: a RENAME followed by an UPDATE must still delete the
// committed row under the old prefix. The updated image lives in the session
// overlay under the new prefix, so the commit has to move-or-delete every
// durable old key, not only the rows whose image it rewrites.
func TestTransactionalDDLRenameThenUpdateKeepsNoOrphanRows(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO a VALUES (1, 'one'), (2, 'two')")

	execMust(t, e, "BEGIN")
	execMust(t, e, "ALTER TABLE a RENAME TO b")
	execMust(t, e, "UPDATE b SET v = 'changed' WHERE id = 1")
	execMust(t, e, "COMMIT")

	if schema.TableExists("a") {
		t.Fatal("renamed table is still addressable under its old name")
	}
	if res := execMust(t, e, "SELECT id, v FROM b ORDER BY id"); res.RowCount != 2 || res.Rows[0][1] != "changed" || res.Rows[1][1] != "two" {
		t.Fatalf("rows after committed rename+update = %v", res.Rows)
	}
	// The decisive check: a table re-created with the old name must start empty.
	// Any durable row left under _data:a: would resurface here as a ghost.
	execMust(t, e, "CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)")
	if res := execMust(t, e, "SELECT COUNT(*) FROM a"); res.Rows[0][0] != int64(0) {
		t.Fatalf("orphaned rows resurfaced under the re-created old name: %v", res.Rows)
	}
	if rows, err := table.Select("b", nil); err != nil || len(rows) != 2 {
		t.Fatalf("durable rows after rename+update = %v err=%v", rows, err)
	}
}

// Transactional DDL: the same guarantee for a staged DELETE after a RENAME.
func TestTransactionalDDLRenameThenDeleteKeepsNoOrphanRows(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO a VALUES (1, 'one'), (2, 'two')")

	execMust(t, e, "BEGIN")
	execMust(t, e, "ALTER TABLE a RENAME TO b")
	execMust(t, e, "DELETE FROM b WHERE id = 2")
	execMust(t, e, "COMMIT")

	if res := execMust(t, e, "SELECT id FROM b"); res.RowCount != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("rows after committed rename+delete = %v", res.Rows)
	}
	execMust(t, e, "CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)")
	if res := execMust(t, e, "SELECT COUNT(*) FROM a"); res.Rows[0][0] != int64(0) {
		t.Fatalf("deleted row or its sibling resurfaced under the re-created old name: %v", res.Rows)
	}
	if rows, err := table.Select("b", nil); err != nil || len(rows) != 1 {
		t.Fatalf("durable rows after rename+delete = %v err=%v", rows, err)
	}
}

// Transactional DDL: renaming a table away and back inside one transaction must
// not publish a put+delete for the same key; all rows and the schema survive.
func TestTransactionalDDLRenameAwayAndBack(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO a VALUES (1, 'one'), (2, 'two')")

	execMust(t, e, "BEGIN")
	execMust(t, e, "ALTER TABLE a RENAME TO b")
	execMust(t, e, "ALTER TABLE b RENAME TO a")
	execMust(t, e, "INSERT INTO a VALUES (3, 'three')")
	execMust(t, e, "COMMIT")

	if schema.TableExists("b") {
		t.Fatal("intermediate rename name survived the commit")
	}
	if res := execMust(t, e, "SELECT id, v FROM a ORDER BY id"); res.RowCount != 3 {
		t.Fatalf("rows after rename away and back = %v", res.Rows)
	}
	if rows, err := table.Select("a", nil); err != nil || len(rows) != 3 {
		t.Fatalf("durable rows after rename away and back = %v err=%v", rows, err)
	}
}

// Transactional DDL: ROLLBACK TO SAVEPOINT can be repeated. The savepoint is
// retained, so DDL and rows staged after the first rollback must not leak into
// the state a second rollback installs.
func TestTransactionalDDLRepeatedRollbackToSavepoint(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "SAVEPOINT s")
	execMust(t, e, "CREATE TABLE x (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t VALUES (1)")
	execMust(t, e, "ROLLBACK TO SAVEPOINT s")

	execMust(t, e, "CREATE TABLE y (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO t VALUES (2)")
	execMust(t, e, "ROLLBACK TO SAVEPOINT s")
	execMust(t, e, "COMMIT")

	if schema.TableExists("x") || schema.TableExists("y") {
		t.Fatalf("DDL staged between rollbacks survived: x=%v y=%v", schema.TableExists("x"), schema.TableExists("y"))
	}
	if res := execMust(t, e, "SELECT COUNT(*) FROM t"); res.Rows[0][0] != int64(0) {
		t.Fatalf("rows staged between rollbacks survived: %v", res.Rows)
	}
}

// Transactional DDL: committing DDL on one table must not reset another table's
// ROWID allocator. A concurrent transaction that staged a TEXT-keyed insert
// holds a ROWID that is not durable yet; if the allocator is wiped, a later
// insert derives that same value again and both rows become durable with the
// same _rowid_, mis-associating every rowid-keyed index cache entry.
func TestTransactionalDDLCommitKeepsUnrelatedRowIDAllocators(t *testing.T) {
	_, schema, table := newTestDB(t)
	stager := newExec(schema, table)
	ddl := newExec(schema, table)
	other := newExec(schema, table)

	execMust(t, stager, "CREATE TABLE events (name TEXT PRIMARY KEY, v TEXT)")

	execMust(t, stager, "BEGIN")
	execMust(t, stager, "INSERT INTO events VALUES ('staged', 'a')")

	// An unrelated transactional DDL commit must not touch the events allocator.
	execMust(t, ddl, "BEGIN")
	execMust(t, ddl, "CREATE TABLE side (id INTEGER PRIMARY KEY)")
	execMust(t, ddl, "COMMIT")

	// Another session allocates its own ROWID while the first insert is staged.
	execMust(t, other, "INSERT INTO events VALUES ('later', 'b')")
	execMust(t, stager, "COMMIT")

	rows, err := table.Select("events", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("events rows = %d, want 2", len(rows))
	}
	seen := make(map[int64]bool, len(rows))
	for _, row := range rows {
		rowid, ok := row["_rowid_"].(int64)
		if !ok {
			t.Fatalf("row without a rowid: %v", row)
		}
		if seen[rowid] {
			t.Fatalf("duplicate durable _rowid_ %d after an unrelated DDL commit: %v", rowid, rows)
		}
		seen[rowid] = true
	}
}

// Transactional DDL: a RENAME whose row move cannot fit in the commit's single
// storage transaction fails at COMMIT with an explicit limit error instead of
// buffering the whole table and failing inside the storage protocol, and the
// failed commit publishes nothing.
func TestTransactionalDDLRenameExceedingBatchCeilingFails(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE big (id INTEGER PRIMARY KEY, v TEXT)")

	// A rename publishes a put and a delete per row, so just over half of the
	// store's 65,535-operation ceiling is enough to exceed it.
	const rows = 33000
	const chunk = 5000
	for start := 0; start < rows; start += chunk {
		batch := make([]storage.Row, 0, chunk)
		for i := start; i < start+chunk && i < rows; i++ {
			batch = append(batch, storage.Row{"id": int64(i + 1), "v": "x"})
		}
		if _, err := table.InsertBulk("big", batch); err != nil {
			t.Fatalf("seed rows %d-%d: %v", start, start+len(batch), err)
		}
	}

	execMust(t, e, "BEGIN")
	execMust(t, e, "ALTER TABLE big RENAME TO big_renamed")
	_, err := execSQL(e, "COMMIT")
	if err == nil {
		t.Fatal("commit of an oversized rename succeeded")
	}
	if !strings.Contains(err.Error(), "storage batch limit") {
		t.Fatalf("commit error = %v, want the storage batch limit", err)
	}
	// Nothing was published: the old name, its schema, and every row survive.
	if !schema.TableExists("big") || schema.TableExists("big_renamed") {
		t.Fatalf("failed commit published the rename: big=%v big_renamed=%v", schema.TableExists("big"), schema.TableExists("big_renamed"))
	}
	if count, err := table.CountFast("big"); err != nil || count != rows {
		t.Fatalf("rows after failed commit = %d err=%v, want %d", count, err, rows)
	}
}
