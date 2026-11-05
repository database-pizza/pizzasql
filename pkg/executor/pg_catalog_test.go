package executor

import (
	"fmt"
	"strings"
	"testing"
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
		`CREATE INDEX idx_pg_class_name ON pg_catalog.pg_class (relname)`,
		`ALTER TABLE pg_catalog.pg_class ADD COLUMN nope TEXT`,
		`DROP TABLE pg_catalog.pg_class`,
		`CREATE VIEW pg_catalog.pg_class AS SELECT 1`,
	} {
		if _, err := execSQL(e, sql); err == nil {
			t.Fatalf("%s: catalog write must be rejected", sql)
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
