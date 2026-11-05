package executor

import (
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

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

func TestSelectColumnTypesArithmeticExpression(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	// Integer arithmetic accumulates in int64, so it reports the wider
	// integer type rather than the schema type of its operands.
	res := execMust(t, e, "SELECT id + 1, id * 2 FROM t")
	if len(res.ColumnTypes) != 2 || res.ColumnTypes[0] != "BIGINT" || res.ColumnTypes[1] != "BIGINT" {
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

func TestSelectColumnTypesJoin(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE hits (hit_id INTEGER PRIMARY KEY, path_id INTEGER, created_at TIMESTAMP)`)
	execMust(t, e, `CREATE TABLE paths (path_id INTEGER PRIMARY KEY, path TEXT)`)
	execMust(t, e, `INSERT INTO paths VALUES (1, '/home')`)
	execMust(t, e, `INSERT INTO hits VALUES (1, 1, '2024-05-06 07:08:09')`)

	res := execMust(t, e, `SELECT paths.path, hits.created_at FROM hits JOIN paths USING (path_id)`)
	want := []string{"TEXT", "TIMESTAMP"}
	if len(res.ColumnTypes) != len(want) {
		t.Fatalf("column types = %v, want %v", res.ColumnTypes, want)
	}
	for i := range want {
		if res.ColumnTypes[i] != want[i] {
			t.Fatalf("column types = %v, want %v", res.ColumnTypes, want)
		}
	}
}

func TestSelectColumnTypesJoinUsesFirstUnqualifiedMatch(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE a (id INTEGER PRIMARY KEY, value TEXT)`)
	execMust(t, e, `CREATE TABLE b (id INTEGER PRIMARY KEY, value INTEGER)`)
	execMust(t, e, `INSERT INTO a VALUES (1, 'one')`)
	execMust(t, e, `INSERT INTO b VALUES (1, 2)`)

	res := execMust(t, e, `SELECT value FROM a JOIN b ON a.id = b.id`)
	if len(res.ColumnTypes) != 1 || res.ColumnTypes[0] != "TEXT" {
		t.Fatalf("column types = %v, want [TEXT]", res.ColumnTypes)
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

// TestInferExprType pins the expression type inference behind result metadata.
func TestInferExprType(t *testing.T) {
	schema := &storage.Schema{Name: "t", Columns: []storage.Column{
		{Name: "id", Type: "INTEGER"},
		{Name: "big", Type: "BIGINT"},
		{Name: "small", Type: "SMALLINT"},
		{Name: "price", Type: "NUMERIC"},
		{Name: "score", Type: "DOUBLE"},
		{Name: "flag", Type: "BOOLEAN"},
		{Name: "name", Type: "TEXT"},
		{Name: "data", Type: "BLOB"},
		{Name: "created_at", Type: "TIMESTAMP"},
		{Name: "day", Type: "DATE"},
	}}
	resolve := schemaTypeResolver(schema)

	cases := []struct {
		expr string
		want string
	}{
		// Direct column references keep the declared schema type.
		{"id", "INTEGER"},
		{"big", "BIGINT"},
		{"small", "SMALLINT"},
		{"price", "NUMERIC"},
		{"score", "DOUBLE"},
		{"flag", "BOOLEAN"},
		{"name", "TEXT"},
		{"data", "BLOB"},
		{"created_at", "TIMESTAMP"},
		{"day", "DATE"},
		{"rowid", "BIGINT"},
		{"missing", "TEXT"},

		// Literals.
		{"1", "INTEGER"},
		{"2147483647", "INTEGER"},
		{"2147483648", "BIGINT"},
		{"1.5", "DOUBLE"},
		{"'text'", "TEXT"},
		{"TRUE", "BOOLEAN"},
		{"FALSE", "BOOLEAN"},
		{"X'00'", "BLOB"},
		{"NULL", "TEXT"},

		// Comparisons and boolean expressions.
		{"id = 1", "BOOLEAN"},
		{"id <> 1", "BOOLEAN"},
		{"id > 0 AND flag", "BOOLEAN"},
		{"flag OR id < 0", "BOOLEAN"},
		{"NOT flag", "BOOLEAN"},
		{"name LIKE 'a%'", "BOOLEAN"},
		{"id BETWEEN 1 AND 3", "BOOLEAN"},
		{"id IN (1, 2)", "BOOLEAN"},
		{"name IS NULL", "BOOLEAN"},
		{"id IS DISTINCT FROM 1", "BOOLEAN"},

		// Aggregates.
		{"count(*)", "BIGINT"},
		{"count(id)", "BIGINT"},
		{"sum(id)", "BIGINT"},
		{"sum(price)", "DOUBLE"},
		{"sum(score)", "DOUBLE"},
		{"avg(id)", "DOUBLE"},
		{"avg(score)", "DOUBLE"},
		{"total(id)", "DOUBLE"},
		{"min(created_at)", "TIMESTAMP"},
		{"max(name)", "TEXT"},

		// Arithmetic, concatenation, and bitwise operators.
		{"id + 1", "BIGINT"},
		{"score + 1", "DOUBLE"},
		{"price + 1", "DOUBLE"},
		{"name || 'x'", "TEXT"},
		{"id & 3", "BIGINT"},
		{"-id", "INTEGER"},
		{"-1.5", "DOUBLE"},
		{"(id)", "INTEGER"},

		// Casts report the target type the executor converts to.
		{"CAST(id AS BIGINT)", "BIGINT"},
		{"CAST(id AS SMALLINT)", "BIGINT"},
		{"CAST(id AS TEXT)", "TEXT"},
		{"CAST(name AS BLOB)", "BLOB"},
		{"CAST(name AS DOUBLE)", "DOUBLE"},
		{"CAST(id AS REAL)", "DOUBLE"},
		{"CAST(id AS INTEGER)", "BIGINT"},
		{"CAST(id AS TINYINT)", "BIGINT"},
		{"CAST(id AS DATE)", "TEXT"},
		{"CAST(id AS NUMERIC)", "TEXT"},

		// NUMERIC/DECIMAL have no exact decimal carrier: their values are
		// float64, so expressions over them report DOUBLE.
		{"price + price", "DOUBLE"},
		{"-price", "DOUBLE"},

		// CASE keeps a type only when every branch agrees.
		{"CASE WHEN flag THEN id ELSE 0 END", "INTEGER"},
		{"CASE WHEN flag THEN id ELSE name END", "TEXT"},

		// Functions whose result type is not modeled stay TEXT.
		{"lower(name)", "TEXT"},
	}

	for _, tc := range cases {
		stmt := parse(t, "SELECT "+tc.expr).(*parser.SelectStmt)
		if got := inferExprType(stmt.Columns[0].Expr, resolve); got != tc.want {
			t.Errorf("inferExprType(%q) = %q, want %q", tc.expr, got, tc.want)
		}
	}
}

// TestSelectColumnTypesExpressions verifies that the executor attaches inferred
// types to non-column projections, including over an empty result set.
func TestSelectColumnTypesExpressions(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, created_at TIMESTAMP, price NUMERIC, small SMALLINT)")
	execMust(t, e, "INSERT INTO t VALUES (1, '2024-05-06 07:08:09', 19.5, 7)")

	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT count(*) FROM t", []string{"BIGINT"}},
		{"SELECT sum(id) FROM t", []string{"BIGINT"}},
		{"SELECT sum(price) FROM t", []string{"DOUBLE"}},
		{"SELECT avg(id) FROM t", []string{"DOUBLE"}},
		{"SELECT avg(price) FROM t", []string{"DOUBLE"}},
		{"SELECT small + 1 FROM t", []string{"BIGINT"}},
		{"SELECT price + 1 FROM t", []string{"DOUBLE"}},
		{"SELECT id = 1 FROM t", []string{"BOOLEAN"}},
		{"SELECT 'text' FROM t", []string{"TEXT"}},
		{"SELECT 42 FROM t", []string{"INTEGER"}},
		{"SELECT 1.5 FROM t", []string{"DOUBLE"}},
		{"SELECT CAST(id AS BIGINT) FROM t", []string{"BIGINT"}},
		{"SELECT created_at FROM t GROUP BY created_at", []string{"TIMESTAMP"}},
		{"SELECT id FROM t UNION SELECT id FROM t", []string{"INTEGER"}},
		// The empty result still reports projection metadata.
		{"SELECT id, created_at FROM t WHERE id = 999", []string{"INTEGER", "TIMESTAMP"}},
		{"SELECT id + 1 FROM t WHERE id = 999", []string{"BIGINT"}},
		{"SELECT count(*), max(created_at) FROM t WHERE id = 999", []string{"BIGINT", "TIMESTAMP"}},
	}

	for _, tc := range cases {
		res := execMust(t, e, tc.sql)
		if len(res.ColumnTypes) != len(tc.want) {
			t.Errorf("%s: column types = %v, want %v", tc.sql, res.ColumnTypes, tc.want)
			continue
		}
		for i := range tc.want {
			if res.ColumnTypes[i] != tc.want[i] {
				t.Errorf("%s: column types = %v, want %v", tc.sql, res.ColumnTypes, tc.want)
				break
			}
		}
	}
}

// TestSelectColumnTypesWithoutFrom covers projections with no schema at all.
func TestSelectColumnTypesWithoutFrom(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	res := execMust(t, e, "SELECT 1, 1.5, 'x', 1 = 1, CAST(1 AS SMALLINT)")
	want := []string{"INTEGER", "DOUBLE", "TEXT", "BOOLEAN", "BIGINT"}
	if len(res.ColumnTypes) != len(want) {
		t.Fatalf("column types = %v, want %v", res.ColumnTypes, want)
	}
	for i := range want {
		if res.ColumnTypes[i] != want[i] {
			t.Fatalf("column types = %v, want %v", res.ColumnTypes, want)
		}
	}
}
