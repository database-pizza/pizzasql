package executor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

// setupPGCatalogFixture creates a schema that exercises every catalog-backed
// object shape: single and composite primary keys, UNIQUE, FOREIGN KEY, CHECK,
// explicit indexes, arrays, JSONB, defaults, and a view.
func setupPGCatalogFixture(t *testing.T) *Executor {
	t.Helper()
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	for _, sql := range []string{
		"CREATE TABLE accounts (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE)",
		"CREATE TABLE orders (id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, note TEXT DEFAULT 'new', tags TEXT[], meta JSONB, CHECK (id > 0), CONSTRAINT fk_orders_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE)",
		"CREATE INDEX idx_orders_note ON orders (note)",
		"CREATE TABLE pairs (a TEXT, b TEXT, v TEXT, PRIMARY KEY (a, b))",
		"CREATE VIEW active_orders AS SELECT id, note FROM orders",
	} {
		execMust(t, e, sql)
	}
	return e
}

func catalogCell(t *testing.T, res *Result, row, col int) string {
	t.Helper()
	if row >= len(res.Rows) || col >= len(res.Rows[row]) {
		t.Fatalf("row %d column %d out of range: %v", row, col, res.Rows)
	}
	value := res.Rows[row][col]
	if value == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v", value)
}

func catalogRows(t *testing.T, e *Executor, query string) [][]string {
	t.Helper()
	res, err := execSQL(e, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	rows := make([][]string, len(res.Rows))
	for i := range res.Rows {
		rows[i] = make([]string, len(res.Rows[i]))
		for j, value := range res.Rows[i] {
			if value == nil {
				rows[i][j] = "<nil>"
				continue
			}
			rows[i][j] = fmt.Sprintf("%v", value)
		}
	}
	return rows
}

// A PostgreSQL-style column listing joins pg_class, pg_namespace, and
// pg_attribute; the synthetic OIDs must line up across all three.
func TestPGCatalogJoinListsColumns(t *testing.T) {
	e := setupPGCatalogFixture(t)
	rows := catalogRows(t, e, `
		SELECT n.nspname, c.relname, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
		WHERE c.relname = 'orders' AND n.nspname = 'public' AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`)
	want := [][]string{
		{"public", "orders", "id", "integer", "true"},
		{"public", "orders", "account_id", "integer", "true"},
		{"public", "orders", "note", "text", "false"},
		{"public", "orders", "tags", "text[]", "false"},
		{"public", "orders", "meta", "jsonb", "false"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("columns = %v, want %v", rows, want)
	}
}

func TestPGCatalogInformationSchemaColumns(t *testing.T) {
	e := setupPGCatalogFixture(t)
	rows := catalogRows(t, e, `
		SELECT table_name, column_name, data_type, is_nullable, udt_name, ordinal_position
		FROM information_schema.columns
		WHERE table_name = 'orders'
		ORDER BY ordinal_position`)
	want := [][]string{
		{"orders", "id", "integer", "NO", "int4", "1"},
		{"orders", "account_id", "integer", "NO", "int4", "2"},
		{"orders", "note", "text", "YES", "text", "3"},
		{"orders", "tags", "ARRAY", "YES", "_text", "4"},
		{"orders", "meta", "jsonb", "YES", "jsonb", "5"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("information_schema.columns = %v, want %v", rows, want)
	}

	// Schema names are case-insensitive, as in PostgreSQL.
	res := execMust(t, e, `SELECT count(*) FROM INFORMATION_SCHEMA.TABLES WHERE table_name = 'orders'`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("uppercase information schema lookup = %v, want 1", res.Rows[0][0])
	}

	res = execMust(t, e, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if res.Rows[0][0] != int64(3) {
		t.Fatalf("base table count = %v, want 3", res.Rows[0][0])
	}
	res = execMust(t, e, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'active_orders' AND table_type = 'VIEW'`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("view count = %v, want 1", res.Rows[0][0])
	}
	res = execMust(t, e, `SELECT count(*) FROM information_schema.sequences`)
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("sequence count = %v, want 0", res.Rows[0][0])
	}
}

func TestPGCatalogIndexRelations(t *testing.T) {
	e := setupPGCatalogFixture(t)
	rows := catalogRows(t, e, `
		SELECT c.relname, i.indisunique, i.indisprimary, i.indkey
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
		WHERE t.relname = 'orders'
		ORDER BY c.relname`)
	want := [][]string{
		{"idx_orders_note", "false", "false", "{3}"},
		{"orders_pkey", "true", "true", "{1}"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("pg_index = %v, want %v", rows, want)
	}

	rows = catalogRows(t, e, `SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'orders' ORDER BY indexname`)
	if len(rows) != 2 {
		t.Fatalf("pg_indexes rows = %v, want 2", rows)
	}
	if !strings.Contains(rows[0][1], "CREATE INDEX idx_orders_note ON public.orders USING btree (note)") {
		t.Fatalf("indexdef = %q", rows[0][1])
	}
	if !strings.Contains(rows[1][1], "CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)") {
		t.Fatalf("indexdef = %q", rows[1][1])
	}

	// Composite keys keep declaration order in pg_index.indkey.
	rows = catalogRows(t, e, `
		SELECT i.indkey FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'pairs_pkey'`)
	if len(rows) != 1 || rows[0][0] != "{1,2}" {
		t.Fatalf("composite indkey = %v, want {1,2}", rows)
	}
}

func TestPGCatalogConstraints(t *testing.T) {
	e := setupPGCatalogFixture(t)
	rows := catalogRows(t, e, `
		SELECT conname, contype, conkey, confkey, pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'orders'::regclass
		ORDER BY conname`)
	want := [][]string{
		{"fk_orders_account", "f", "{2}", "{1}", "FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE"},
		{"orders_check", "c", "<nil>", "<nil>", "CHECK (((id > 0)))"},
		{"orders_pkey", "p", "{1}", "<nil>", "PRIMARY KEY (id)"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("pg_constraint = %v, want %v", rows, want)
	}

	rows = catalogRows(t, e, `SELECT conname, contype FROM pg_constraint WHERE conrelid = 'accounts'::regclass ORDER BY conname`)
	want = [][]string{
		{"accounts_pkey", "p"},
		{"uniq_accounts_email", "u"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("account constraints = %v, want %v", rows, want)
	}

	rows = catalogRows(t, e, `
		SELECT constraint_name, constraint_type FROM information_schema.table_constraints
		WHERE table_name = 'orders' ORDER BY constraint_name`)
	want = [][]string{
		{"fk_orders_account", "FOREIGN KEY"},
		{"orders_check", "CHECK"},
		{"orders_pkey", "PRIMARY KEY"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("table_constraints = %v, want %v", rows, want)
	}

	rows = catalogRows(t, e, `
		SELECT constraint_name, column_name, ordinal_position, position_in_unique_constraint
		FROM information_schema.key_column_usage
		WHERE table_name = 'orders' ORDER BY constraint_name, ordinal_position`)
	want = [][]string{
		{"fk_orders_account", "account_id", "1", "1"},
		{"orders_pkey", "id", "1", "<nil>"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("key_column_usage = %v, want %v", rows, want)
	}

	rows = catalogRows(t, e, `
		SELECT constraint_name, unique_constraint_name, delete_rule, update_rule
		FROM information_schema.referential_constraints ORDER BY constraint_name`)
	want = [][]string{{"fk_orders_account", "accounts_pkey", "CASCADE", "NO ACTION"}}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("referential_constraints = %v, want %v", rows, want)
	}
}

// regclass is symbolic in PostgreSQL: a name literal resolves to the same OID
// pg_class reports, and rendering the OID yields the name again.
func TestPGCatalogRegclassRoundTrip(t *testing.T) {
	e := setupPGCatalogFixture(t)
	rows := catalogRows(t, e, `
		SELECT c.relname, c.oid, 'orders'::regclass, c.oid::regclass
		FROM pg_class c WHERE c.relname = 'orders'`)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0][0] != "orders" || rows[0][2] != "orders" || rows[0][3] != "orders" {
		t.Fatalf("regclass rendering = %v", rows)
	}

	// Text output of a regclass value is the relation name, as in PostgreSQL;
	// ::oid exposes the numeric OID for comparisons.
	rows = catalogRows(t, e, `
		SELECT 'orders'::regclass::text,
		       'orders'::regclass::oid = (SELECT oid FROM pg_class WHERE relname = 'orders'),
		       (SELECT oid FROM pg_class WHERE relname = 'orders') = 'orders'::regclass`)
	if rows[0][0] != "orders" || rows[0][1] != "true" || rows[0][2] != "true" {
		t.Fatalf("regclass text/comparison = %v", rows)
	}

	// regtype resolves the built-in type table.
	rows = catalogRows(t, e, `SELECT 'integer'::regtype, 'integer'::regtype = 'int4'::regtype`)
	if rows[0][0] != "integer" || rows[0][1] != "true" {
		t.Fatalf("regtype = %v", rows)
	}

	if _, err := execSQL(e, `SELECT 'missing_relation'::regclass`); err == nil {
		t.Fatal("unknown relation cast must fail")
	}
}

func TestPGCatalogIntrospectionFunctions(t *testing.T) {
	e := setupPGCatalogFixture(t)
	rows := catalogRows(t, e, `
		SELECT current_schema(), current_database(),
		       pg_table_is_visible('orders'::regclass),
		       has_table_privilege('orders', 'SELECT'),
		       has_table_privilege('missing_table', 'SELECT')`)
	if rows[0][0] != "public" || rows[0][1] != "testdb" || rows[0][2] != "true" || rows[0][3] != "true" || rows[0][4] != "false" {
		t.Fatalf("identity/privilege functions = %v", rows)
	}

	rows = catalogRows(t, e, `
		SELECT pg_get_serial_sequence('orders', 'id'),
		       obj_description('orders'::regclass, 'pg_class'),
		       col_description('orders'::regclass, 1)`)
	if len(rows) != 1 || rows[0][0] != "<nil>" || rows[0][1] != "<nil>" || rows[0][2] != "<nil>" {
		t.Fatalf("description functions = %v, want NULLs", rows)
	}

	rows = catalogRows(t, e, `
		SELECT pg_get_expr(d.adbin, d.adrelid)
		FROM pg_attrdef d WHERE d.adrelid = 'orders'::regclass`)
	if len(rows) != 1 || rows[0][0] != "'new'" {
		t.Fatalf("pg_get_expr = %v, want the stored default text", rows)
	}

	rows = catalogRows(t, e, `SELECT pg_catalog.version(), current_setting('search_path')`)
	if !strings.Contains(rows[0][0], "PostgreSQL") || rows[0][1] != "public" {
		t.Fatalf("version/current_setting = %v", rows)
	}

	rows = catalogRows(t, e, `SELECT format_type(23, -1), format_type(1043, 14), format_type(999999, -1)`)
	if rows[0][0] != "integer" || rows[0][1] != "character varying(10)" || rows[0][2] != "???" {
		t.Fatalf("format_type = %v", rows)
	}
}

// A psql-style column listing combines the catalog relations with LEFT JOINs;
// it must return one row per live attribute and mark the primary key.
func TestPGCatalogPsqlStyleDescribe(t *testing.T) {
	e := setupPGCatalogFixture(t)
	rows := catalogRows(t, e, `
		SELECT a.attname,
		       pg_catalog.format_type(a.atttypid, a.atttypmod) AS type,
		       a.attnotnull AS notnull,
		       pg_catalog.pg_get_expr(d.adbin, d.adrelid) AS dflt,
		       i.indisprimary AS is_pk
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
		LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		LEFT JOIN pg_catalog.pg_index i ON i.indrelid = c.oid AND a.attnum = ANY(i.indkey)
		WHERE c.relname = 'orders' AND c.relkind = 'r' AND n.nspname = 'public'
		  AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`)
	want := [][]string{
		{"id", "integer", "true", "<nil>", "true"},
		{"account_id", "integer", "true", "<nil>", "<nil>"},
		{"note", "text", "false", "'new'", "false"},
		{"tags", "text[]", "false", "<nil>", "<nil>"},
		{"meta", "jsonb", "false", "<nil>", "<nil>"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("psql-style describe = %v, want %v", rows, want)
	}
}

// Catalog rows reflect DDL immediately, and transactional DDL is visible only
// to the connection that staged it.
func TestPGCatalogTransactionalDDLVisibility(t *testing.T) {
	_, schema, table := newTestDB(t)
	writer := newExec(schema, table)
	observer := newExec(schema, table)

	execMust(t, writer, "CREATE TABLE base (id INTEGER PRIMARY KEY)")
	execMust(t, writer, "BEGIN")
	execMust(t, writer, "CREATE TABLE staged (id INTEGER PRIMARY KEY)")
	execMust(t, writer, "ALTER TABLE base ADD COLUMN note TEXT")
	execMust(t, writer, "CREATE INDEX idx_base_note ON base (note)")

	res := execMust(t, writer, `SELECT table_name FROM information_schema.tables WHERE table_name = 'staged'`)
	if len(res.Rows) != 1 {
		t.Fatalf("writer does not see staged table: %v", res.Rows)
	}
	res = execMust(t, writer, `
		SELECT column_name FROM information_schema.columns
		WHERE table_name = 'base' ORDER BY ordinal_position`)
	if len(res.Rows) != 2 || res.Rows[1][0] != "note" {
		t.Fatalf("writer does not see staged column: %v", res.Rows)
	}
	res = execMust(t, writer, `SELECT count(*) FROM pg_indexes WHERE tablename = 'base' AND indexname = 'idx_base_note'`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("writer does not see staged index: %v", res.Rows)
	}

	res = execMust(t, observer, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'staged'`)
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("observer sees staged table: %v", res.Rows)
	}
	res = execMust(t, observer, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'base'`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("observer sees staged column: %v", res.Rows)
	}

	execMust(t, writer, "ROLLBACK")
	res = execMust(t, writer, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'staged'`)
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("rolled-back table still visible: %v", res.Rows)
	}
	res = execMust(t, writer, `SELECT count(*) FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid WHERE c.relname = 'base'`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("rolled-back column still visible: %v", res.Rows)
	}
}

// OIDs are deterministic per database, so two connections and repeated
// statements agree.
func TestPGCatalogOIDsAreDeterministic(t *testing.T) {
	_, schema, table := newTestDB(t)
	first := newExec(schema, table)
	execMust(t, first, "CREATE TABLE deterministic (id INTEGER PRIMARY KEY, name TEXT)")
	query := `SELECT oid, relname FROM pg_class WHERE relname = 'deterministic'`
	one := catalogRows(t, first, query)
	two := catalogRows(t, first, query)
	if fmt.Sprint(one) != fmt.Sprint(two) {
		t.Fatalf("OID changed between statements: %v vs %v", one, two)
	}
	second := newExec(schema, table)
	three := catalogRows(t, second, query)
	if fmt.Sprint(one) != fmt.Sprint(three) {
		t.Fatalf("OID differs across connections: %v vs %v", one, three)
	}
	if len(one) != 1 || one[0][1] != "deterministic" {
		t.Fatalf("unexpected relation row: %v", one)
	}
}

func TestPGCatalogRejectsWrites(t *testing.T) {
	e := setupPGCatalogFixture(t)
	for _, sql := range []string{
		`INSERT INTO pg_catalog.pg_class VALUES (1)`,
		`INSERT INTO information_schema.tables VALUES ('x')`,
		`UPDATE pg_catalog.pg_type SET typname = 'x'`,
		`DELETE FROM pg_catalog.pg_attribute`,
		`CREATE INDEX idx_pg_class_name ON pg_class (relname)`,
		`ALTER TABLE pg_class ADD COLUMN nope TEXT`,
		`DROP TABLE pg_catalog.pg_class`,
		`CREATE VIEW pg_class AS SELECT 1`,
	} {
		_, err := execSQL(e, sql)
		if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Fatalf("%s: err = %v, want 0A000 feature_not_supported", sql, err)
		}
	}
}

func TestPGSearchPathSettings(t *testing.T) {
	e := setupPGCatalogFixture(t)
	for _, sql := range []string{
		"SET search_path TO public",
		"SET search_path = public",
		"SET client_encoding TO 'UTF8'",
		"SET TIME ZONE 'UTC'",
		"SET statement_timeout = 0",
		"SET NAMES 'utf8'",
		"SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED",
	} {
		if _, err := execSQL(e, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	res := execMust(t, e, "SHOW search_path")
	if len(res.Rows) != 1 || res.Rows[0][0] != "public" {
		t.Fatalf("SHOW search_path = %v", res.Rows)
	}
	res = execMust(t, e, "SHOW server_version")
	if len(res.Rows) != 1 || res.Rows[0][0] == "" {
		t.Fatalf("SHOW server_version = %v", res.Rows)
	}
	if _, err := execSQL(e, "SHOW definitely_not_a_setting"); err == nil {
		t.Fatal("unknown SHOW setting must fail")
	}
}

// The SQLite introspection surface keeps working alongside the PostgreSQL
// catalogs; sqlite_master must not list the virtual relations.
func TestPGCatalogDoesNotChangeSQLiteIntrospection(t *testing.T) {
	e := setupPGCatalogFixture(t)
	res := execMust(t, e, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	names := make([]string, len(res.Rows))
	for i, row := range res.Rows {
		names[i] = fmt.Sprintf("%v", row[0])
	}
	if strings.Join(names, ",") != "accounts,orders,pairs" {
		t.Fatalf("sqlite_master tables = %v", names)
	}
	res = execMust(t, e, `SELECT name FROM sqlite_master WHERE type = 'index' ORDER BY name`)
	if len(res.Rows) != 2 {
		t.Fatalf("sqlite_master indexes = %v", res.Rows)
	}
	res = execMust(t, e, `PRAGMA table_info(orders)`)
	if len(res.Rows) != 5 {
		t.Fatalf("PRAGMA table_info rows = %v", res.Rows)
	}
}

// An index may share its table's name. pg_class then holds two rows with the
// same relname, so relkind is part of the relation identity: the rows keep
// distinct OIDs and pg_index/pg_attribute stay consistent with each of them.
func TestPGCatalogIndexSharingTableName(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE shared (id INTEGER PRIMARY KEY, label TEXT)")
	execMust(t, e, "CREATE INDEX shared ON shared (label)")

	rows := catalogRows(t, e, `SELECT relkind, oid FROM pg_class WHERE relname = 'shared' ORDER BY relkind`)
	if len(rows) != 2 {
		t.Fatalf("pg_class rows for shared = %v, want a table and an index", rows)
	}
	if rows[0][0] != "i" || rows[1][0] != "r" {
		t.Fatalf("pg_class row kinds = %v, want i then r", rows)
	}
	if rows[0][1] == rows[1][1] {
		t.Fatalf("table and index share OID %v", rows[0][1])
	}

	// pg_index must point the index row at the table row, not at itself.
	rows = catalogRows(t, e, `
		SELECT c.relname, i.indexrelid, i.indrelid, i.indrelid = t.oid
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid
		WHERE c.relname = 'shared'`)
	if len(rows) != 1 || rows[0][0] != "shared" || rows[0][1] == rows[0][2] || rows[0][3] != "true" {
		t.Fatalf("pg_index row = %v, want indexrelid != indrelid = t.oid", rows)
	}

	// The table's attributes belong to exactly one relation, so the index row
	// must not double them.
	res := execMust(t, e, `
		SELECT count(*) FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relname = 'shared' AND c.relkind = 'r' AND a.attnum > 0`)
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("pg_attribute rows for the shared table = %v, want 2", res.Rows[0][0])
	}

	// regclass resolution prefers the table when the names collide.
	res = execMust(t, e, `SELECT 'shared'::regclass::oid = (SELECT oid FROM pg_class WHERE relname = 'shared' AND relkind = 'r')`)
	if res.Rows[0][0] != true {
		t.Fatalf("regclass did not resolve to the table: %v", res.Rows)
	}
}

// A durable table named <x>_pkey occupies the synthetic primary-key index name,
// so the synthesized index must pick a free name instead of colliding with it.
func TestPGCatalogSyntheticPrimaryKeyNameCollision(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE owner (id INTEGER PRIMARY KEY)")
	execMust(t, e, "CREATE TABLE owner_pkey (id INTEGER PRIMARY KEY)")

	rows := catalogRows(t, e, `SELECT relname, relkind FROM pg_class WHERE relname LIKE 'owner%' ORDER BY relname, relkind`)
	want := [][]string{
		{"owner", "r"},
		{"owner_pkey", "r"},
		{"owner_pkey2", "i"},
		{"owner_pkey_pkey", "i"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("pg_class rows = %v, want %v", rows, want)
	}

	// The primary key constraint still points at the renamed synthetic index.
	rows = catalogRows(t, e, `
		SELECT con.conname, c.relname
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conindid
		WHERE con.conrelid = 'owner'::regclass AND con.contype = 'p'`)
	if len(rows) != 1 || rows[0][1] != "owner_pkey2" {
		t.Fatalf("primary key index = %v, want owner_pkey2", rows)
	}

	// No name is reported twice with the same relkind.
	res := execMust(t, e, `
		SELECT count(*) FROM (
			SELECT relname, relkind, count(*) AS n FROM pg_class GROUP BY relname, relkind
		) d WHERE d.n > 1`)
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("pg_class contains duplicate relname/relkind rows: %v", res.Rows)
	}
}

// An unknown regclass or regtype reports undefined_table (42P01) instead of an
// internal error, and it fails the statement even inside a WHERE clause.
// to_regclass/to_regtype are the NULL-returning variants.
func TestPGCatalogUndefinedRelationError(t *testing.T) {
	e := setupPGCatalogFixture(t)
	for _, sql := range []string{
		`SELECT 'missing_relation'::regclass`,
		`SELECT 'missing_type'::regtype`,
		`SELECT count(*) FROM pg_class WHERE oid = 'missing_relation'::regclass`,
		`SELECT count(*) FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid WHERE a.attrelid = 'missing_relation'::regclass`,
		// A constant predicate that fails is a statement error even when the
		// table has no rows to evaluate it against.
		`SELECT count(*) FROM orders WHERE 'missing_relation'::regclass IS NOT NULL`,
	} {
		_, err := execSQL(e, sql)
		if !errors.Is(err, pgerror.ErrUndefinedTable) {
			t.Fatalf("%s: err = %v, want an undefined_table error", sql, err)
		}
	}

	// A failing predicate in a join is a statement error too, not a filter that
	// silently returns 0 rows.
	execMust(t, e, "INSERT INTO pairs (a, b, v) VALUES ('x', 'y', 'z')")
	for _, sql := range []string{
		`SELECT * FROM pairs p1 JOIN pairs p2 ON true WHERE 'missing_relation'::regclass IS NOT NULL`,
		`SELECT * FROM pairs p1, pairs p2 WHERE 'missing_relation'::regclass IS NOT NULL`,
	} {
		_, err := execSQL(e, sql)
		if !errors.Is(err, pgerror.ErrUndefinedTable) {
			t.Fatalf("%s: err = %v, want an undefined_table error", sql, err)
		}
	}

	rows := catalogRows(t, e, `SELECT to_regclass('orders'), to_regclass('missing_relation'), to_regtype('missing_type')`)
	if len(rows) != 1 || rows[0][0] != "orders" || rows[0][1] != "<nil>" || rows[0][2] != "<nil>" {
		t.Fatalf("to_regclass/to_regtype = %v", rows)
	}
	res := execMust(t, e, `
		SELECT to_regclass('orders')::oid = (SELECT oid FROM pg_class WHERE relname = 'orders' AND relkind = 'r')`)
	if res.Rows[0][0] != true {
		t.Fatalf("to_regclass OID = %v, want the pg_class OID", res.Rows)
	}
	res = execMust(t, e, `SELECT to_regclass('orders')`)
	if fmt.Sprint(res.ColumnTypes) != "[REGCLASS]" {
		t.Fatalf("to_regclass result type = %v, want REGCLASS", res.ColumnTypes)
	}
}

// A CTE shadows a virtual catalog relation of the same name, exactly as a CTE
// shadows a table.
func TestPGCatalogCTEShadowsVirtualRelation(t *testing.T) {
	e := setupPGCatalogFixture(t)
	rows := catalogRows(t, e, `WITH pg_attribute AS (SELECT 7 AS attnum) SELECT * FROM pg_attribute`)
	if len(rows) != 1 || rows[0][0] != "7" {
		t.Fatalf("non-recursive CTE did not shadow pg_attribute: %v", rows)
	}

	rows = catalogRows(t, e, `
		WITH RECURSIVE pg_class(oid) AS (SELECT 42 UNION ALL SELECT oid + 1 FROM pg_class WHERE oid < 44)
		SELECT * FROM pg_class`)
	if len(rows) != 3 || rows[0][0] != "42" || rows[2][0] != "44" {
		t.Fatalf("recursive CTE did not shadow pg_class: %v", rows)
	}

	res := execMust(t, e, `
		WITH pg_attribute(attname) AS (SELECT 'shadowed')
		SELECT count(*) FROM pg_catalog.pg_attribute`)
	if res.Rows[0][0] == int64(1) {
		t.Fatalf("CTE shadowed an explicitly qualified catalog relation")
	}
}

// A schema qualifier only resolves inside the schema it names: public.pg_class
// is not pg_catalog.pg_class, so it is an undefined table.
func TestPGCatalogQualifiedNameStaysInItsSchema(t *testing.T) {
	e := setupPGCatalogFixture(t)
	for _, sql := range []string{
		`SELECT count(*) FROM public.pg_class`,
		`SELECT count(*) FROM public.pg_attribute`,
	} {
		_, err := execSQL(e, sql)
		if !errors.Is(err, pgerror.ErrUndefinedTable) {
			t.Fatalf("%s: err = %v, want an undefined_table error", sql, err)
		}
	}

	// A real public table still resolves through the qualifier.
	res := execMust(t, e, `SELECT count(*) FROM public.orders`)
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("public.orders rows = %v", res.Rows)
	}
	if res := execMust(t, e, `SELECT count(*) FROM pg_catalog.pg_class`); res.Rows[0][0] == int64(0) {
		t.Fatal("pg_catalog.pg_class is empty")
	}
}

// A view listed by information_schema is listed everywhere else with the same
// shape: pg_class relkind 'v', its pg_attribute rows, and its columns.
func TestPGCatalogViewShape(t *testing.T) {
	e := setupPGCatalogFixture(t)

	rows := catalogRows(t, e, `SELECT relname, relkind, relnatts FROM pg_class WHERE relname = 'active_orders'`)
	if len(rows) != 1 || rows[0][1] != "v" || rows[0][2] != "2" {
		t.Fatalf("pg_class view row = %v", rows)
	}

	rows = catalogRows(t, e, `
		SELECT a.attnum, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relname = 'active_orders' AND c.relkind = 'v' AND a.attnum > 0
		ORDER BY a.attnum`)
	want := [][]string{{"1", "id", "integer"}, {"2", "note", "text"}}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("view attributes = %v, want %v", rows, want)
	}

	rows = catalogRows(t, e, `
		SELECT table_type, is_insertable_into FROM information_schema.tables WHERE table_name = 'active_orders'`)
	if len(rows) != 1 || rows[0][0] != "VIEW" || rows[0][1] != "NO" {
		t.Fatalf("information_schema.tables view row = %v", rows)
	}

	rows = catalogRows(t, e, `
		SELECT column_name, data_type, ordinal_position
		FROM information_schema.columns WHERE table_name = 'active_orders'
		ORDER BY ordinal_position`)
	want = [][]string{{"id", "integer", "1"}, {"note", "text", "2"}}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("information_schema.columns view rows = %v, want %v", rows, want)
	}

	res := execMust(t, e, `SELECT count(*) FROM information_schema.views WHERE table_name = 'active_orders'`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("information_schema.views rows = %v, want 1", res.Rows[0][0])
	}

	// Every view information_schema lists has the same number of columns in
	// pg_attribute and information_schema.columns.
	res = execMust(t, e, `
		SELECT count(*) FROM information_schema.tables t
		WHERE t.table_type = 'VIEW' AND t.table_schema = 'public'
		  AND (SELECT count(*) FROM information_schema.columns c
		       WHERE c.table_name = t.table_name)
		      <> (SELECT relnatts FROM pg_class c
		          WHERE c.relname = t.table_name AND c.relkind = 'v')`)
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("a public view has an inconsistent catalog shape: %v", res.Rows[0][0])
	}
}

// pg_type's function attributes are regproc columns holding 0, not the text "-".
func TestPGCatalogTypeRegprocAttributes(t *testing.T) {
	e := setupPGCatalogFixture(t)
	rows := catalogRows(t, e, `
		SELECT typname, typinput, typoutput, typreceive, typsend, typmodin, typanalyze
		FROM pg_type WHERE typname = 'int4'`)
	want := [][]string{{"int4", "0", "0", "0", "0", "0", "0"}}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("pg_type regproc attributes = %v, want %v", rows, want)
	}

	res := execMust(t, e, `SELECT typinput, typoutput FROM pg_type WHERE typname = 'text'`)
	if fmt.Sprint(res.ColumnTypes) != "[REGPROC REGPROC]" {
		t.Fatalf("pg_type regproc column types = %v", res.ColumnTypes)
	}
}

// search_path follows PostgreSQL's transaction rules: SET LOCAL expires at
// COMMIT (and on ROLLBACK TO SAVEPOINT), and a session SET inside an aborted
// transaction is undone by ROLLBACK.
func TestPGSearchPathTransactionScope(t *testing.T) {
	e := setupPGCatalogFixture(t)
	showPath := func() string {
		t.Helper()
		res := execMust(t, e, "SHOW search_path")
		if len(res.Rows) != 1 {
			t.Fatalf("SHOW search_path = %v", res.Rows)
		}
		return fmt.Sprintf("%v", res.Rows[0][0])
	}

	// SET LOCAL outside a transaction block has no effect.
	execMust(t, e, "SET LOCAL search_path TO pg_catalog")
	if got := showPath(); got != "public" {
		t.Fatalf("search_path after SET LOCAL outside a transaction = %q", got)
	}

	execMust(t, e, "BEGIN")
	execMust(t, e, "SET LOCAL search_path TO pg_catalog")
	if got := showPath(); got != "pg_catalog" {
		t.Fatalf("search_path inside the transaction = %q", got)
	}
	if res := execMust(t, e, "SELECT current_schema()"); res.Rows[0][0] != "pg_catalog" {
		t.Fatalf("current_schema() inside the transaction = %v", res.Rows[0][0])
	}
	execMust(t, e, "COMMIT")
	if got := showPath(); got != "public" {
		t.Fatalf("search_path after COMMIT = %q, want SET LOCAL to expire", got)
	}

	// A session SET is transactional: ROLLBACK restores the previous value,
	// COMMIT keeps it.
	execMust(t, e, "BEGIN")
	execMust(t, e, "SET search_path TO pg_catalog")
	execMust(t, e, "ROLLBACK")
	if got := showPath(); got != "public" {
		t.Fatalf("search_path after ROLLBACK = %q, want the pre-transaction value", got)
	}
	execMust(t, e, "BEGIN")
	execMust(t, e, "SET search_path TO pg_catalog")
	execMust(t, e, "COMMIT")
	if got := showPath(); got != "pg_catalog" {
		t.Fatalf("search_path after COMMIT = %q, want the session SET to persist", got)
	}
	execMust(t, e, "SET search_path TO public")

	// ROLLBACK TO SAVEPOINT undoes a SET LOCAL issued after the savepoint.
	execMust(t, e, "BEGIN")
	execMust(t, e, "SAVEPOINT sp")
	execMust(t, e, "SET LOCAL search_path TO pg_catalog")
	execMust(t, e, "ROLLBACK TO SAVEPOINT sp")
	if got := showPath(); got != "public" {
		t.Fatalf("search_path after ROLLBACK TO SAVEPOINT = %q", got)
	}
	execMust(t, e, "COMMIT")
}
