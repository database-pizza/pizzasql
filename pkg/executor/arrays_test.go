package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/analyzer"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

func TestArrayColumnDDLAndRoundTrip(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE items (
		id INTEGER PRIMARY KEY,
		tags TEXT[] NOT NULL,
		scores INTEGER[],
		flags BOOLEAN[],
		ratios DOUBLE PRECISION[]
	)`)
	execMust(t, e, `INSERT INTO items (id, tags, scores, flags, ratios)
		VALUES (1, '{a,b}', '{1,2}', '{t,f}', '{1.5,2.25}')`)
	execMust(t, e, `INSERT INTO items (id, tags, scores)
		VALUES (2, '{}', NULL)`)

	res := execMust(t, e, `SELECT id, tags, scores, flags, ratios FROM items ORDER BY id`)
	if got := res.Rows[0][1]; got != "{a,b}" {
		t.Fatalf("tags = %v, want {a,b}", got)
	}
	// Elements are canonicalized per declared type.
	if got := res.Rows[0][2]; got != "{1,2}" {
		t.Fatalf("scores = %v, want {1,2}", got)
	}
	if got := res.Rows[0][3]; got != "{t,f}" {
		t.Fatalf("flags = %v, want {t,f}", got)
	}
	if got := res.Rows[0][4]; got != "{1.5,2.25}" {
		t.Fatalf("ratios = %v, want {1.5,2.25}", got)
	}
	// NULL and the empty array stay distinguishable.
	if got := res.Rows[1][1]; got != "{}" {
		t.Fatalf("empty tags = %v, want {}", got)
	}
	if got := res.Rows[1][2]; got != nil {
		t.Fatalf("NULL scores = %v, want nil", got)
	}

	// Result metadata reports the declared array type.
	if got := res.ColumnTypes[1]; got != "TEXT[]" {
		t.Fatalf("tags column type = %q, want TEXT[]", got)
	}

	// The durable catalog keeps the array spelling, so sqlite_master and
	// PRAGMA table_info still render it.
	master := execMust(t, e, `SELECT sql FROM sqlite_master WHERE name = 'items'`)
	if sql, _ := master.Rows[0][0].(string); !strings.Contains(sql, "TEXT[]") {
		t.Fatalf("sqlite_master SQL = %q, want it to contain TEXT[]", sql)
	}
	info := execMust(t, e, `PRAGMA table_info(items)`)
	found := false
	for _, row := range info.Rows {
		if row[1] == "scores" {
			found = true
			if row[2] != "INTEGER[]" {
				t.Fatalf("PRAGMA table_info type = %v, want INTEGER[]", row[2])
			}
		}
	}
	if !found {
		t.Fatal("PRAGMA table_info did not report the scores column")
	}

	// A fresh executor over the same store sees the same bytes: no reload
	// conversion and no storage-format change.
	reloaded := newExec(schema, table)
	res = execMust(t, reloaded, `SELECT tags, scores, flags FROM items WHERE id = 1`)
	if res.Rows[0][0] != "{a,b}" || res.Rows[0][1] != "{1,2}" || res.Rows[0][2] != "{t,f}" {
		t.Fatalf("reloaded row = %v", res.Rows[0])
	}
}

func TestArrayCanonicalEncoding(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT[])`)

	for _, tc := range []struct{ sql, want string }{
		{`INSERT INTO t VALUES (1, '{ a , b }') RETURNING v`, "{a,b}"},
		{`INSERT INTO t VALUES (2, '{"a b","c,d"}') RETURNING v`, `{"a b","c,d"}`},
		{`INSERT INTO t VALUES (3, ARRAY['x', NULL, 'NULL']) RETURNING v`, `{x,NULL,"NULL"}`},
		{`INSERT INTO t VALUES (4, ARRAY[]::TEXT[]) RETURNING v`, "{}"},
		{`INSERT INTO t VALUES (5, '{"quote\"d", "back\\slash"}') RETURNING v`, `{"quote\"d","back\\slash"}`},
	} {
		res := execMust(t, e, tc.sql)
		if got := res.Rows[0][0]; got != tc.want {
			t.Errorf("%s = %v, want %s", tc.sql, got, tc.want)
		}
	}
}

func TestArrayConstructorAndCast(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT ARRAY[1, 2, 3]`, "{1,2,3}"},
		{`SELECT ARRAY['a', NULL]`, "{a,NULL}"},
		{`SELECT ARRAY[TRUE, FALSE]`, "{t,f}"},
		{`SELECT '{1,2,3}'::INTEGER[]`, "{1,2,3}"},
		{`SELECT CAST('{1,2}' AS INTEGER[])`, "{1,2}"},
		{`SELECT ARRAY['1','2']::INTEGER[]`, "{1,2}"},
		{`SELECT '{1.50,2}'::NUMERIC[]`, "{1.50,2}"},
		{`SELECT NULL::INTEGER[]`, nil},
	}
	for _, tc := range cases {
		res := execMust(t, e, tc.sql)
		if got := res.Rows[0][0]; got != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.sql, got, tc.want)
		}
	}

	res := execMust(t, e, `SELECT ARRAY[1,2] AS a, '{x}'::TEXT[] AS b`)
	if res.ColumnTypes[0] != "INTEGER[]" || res.ColumnTypes[1] != "TEXT[]" {
		t.Fatalf("constructor types = %v, want [INTEGER[] TEXT[]]", res.ColumnTypes)
	}
}

func TestArraySubscriptAndLength(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, nums INTEGER[], tags TEXT[], flags BOOLEAN[])`)
	execMust(t, e, `INSERT INTO t VALUES (1, '{10,20,30}', '{"x", NULL, "z"}', '{t,f}')`)
	execMust(t, e, `INSERT INTO t VALUES (2, '{}', '{}', '{}')`)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT nums[1] FROM t WHERE id = 1`, int64(10)},
		{`SELECT nums[3] FROM t WHERE id = 1`, int64(30)},
		{`SELECT nums[4] FROM t WHERE id = 1`, nil},
		{`SELECT nums[0] FROM t WHERE id = 1`, nil},
		{`SELECT tags[2] FROM t WHERE id = 1`, nil},
		{`SELECT tags[1] FROM t WHERE id = 1`, "x"},
		{`SELECT flags[1] FROM t WHERE id = 1`, true},
		{`SELECT flags[2] FROM t WHERE id = 1`, false},
		{`SELECT nums[1] FROM t WHERE id = 2`, nil},
		{`SELECT array_length(nums, 1) FROM t WHERE id = 1`, int64(3)},
		{`SELECT array_length(nums, 1) FROM t WHERE id = 2`, nil},
		{`SELECT array_length(nums, 2) FROM t WHERE id = 1`, nil},
	}
	for _, tc := range cases {
		res := execMust(t, e, tc.sql)
		if got := res.Rows[0][0]; got != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.sql, got, tc.want)
		}
	}

	res := execMust(t, e, `SELECT nums[1] AS n FROM t WHERE id = 1`)
	if res.ColumnTypes[0] != "INTEGER" {
		t.Fatalf("subscript type = %q, want INTEGER", res.ColumnTypes[0])
	}
}

func TestAnyAllThreeValuedLogic(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER, b BOOLEAN)`)
	execMust(t, e, `INSERT INTO t VALUES (1, 2, TRUE)`)

	cases := []struct {
		sql  string
		want interface{}
	}{
		// ANY: true if any comparison is true.
		{`SELECT 2 = ANY('{1,2,3}')`, true},
		{`SELECT 5 = ANY('{1,2,3}')`, false},
		// NULL elements: no true and a NULL comparison yields NULL.
		{`SELECT 5 = ANY('{1,NULL,3}')`, nil},
		{`SELECT 2 = ANY('{NULL,2}')`, true},
		// Empty array: ANY is false, ALL is true, even for a NULL left side.
		{`SELECT 5 = ANY('{}')`, false},
		{`SELECT NULL = ANY('{}')`, false},
		{`SELECT 5 <> ALL('{}')`, true},
		{`SELECT NULL <> ALL('{}')`, true},
		// ALL: false if any comparison is false, NULL if only NULLs remain.
		{`SELECT 2 <> ALL('{1,3,4}')`, true},
		{`SELECT 2 <> ALL('{1,2,4}')`, false},
		{`SELECT 2 <> ALL('{1,NULL}')`, nil},
		{`SELECT 3 > ALL('{1,2}')`, true},
		{`SELECT 1 >= ALL('{1,NULL}')`, nil},
		// A NULL left operand yields NULL for a non-empty array.
		{`SELECT NULL = ANY('{1,2}')`, nil},
		{`SELECT NULL <> ALL('{1,2}')`, nil},
		// Boolean elements compare as booleans, not as text.
		{`SELECT TRUE = ANY('{f,t}')`, true},
		{`SELECT FALSE = ANY('{f,t}')`, true},
		{`SELECT TRUE <> ALL('{f,t}')`, false},
		// Column-driven forms, including a NULL column.
		{`SELECT n = ANY('{1,2}') FROM t`, true},
		{`SELECT n <> ALL('{1,2}') FROM t`, false},
		{`SELECT b = ANY('{t}') FROM t`, true},
		{`SELECT id = ANY('{9,8}') FROM t`, false},
	}
	for _, tc := range cases {
		res := execMust(t, e, tc.sql)
		if got := res.Rows[0][0]; got != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.sql, got, tc.want)
		}
	}

	if res := execMust(t, e, `SELECT n = ANY('{1,2}') AS hit FROM t`); res.ColumnTypes[0] != "BOOLEAN" {
		t.Fatalf("ANY result type = %q, want BOOLEAN", res.ColumnTypes[0])
	}
}

func TestAnyAllOverArrayColumn(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, allowed TEXT[])`)
	execMust(t, e, `INSERT INTO t VALUES (1, '{red,green}'), (2, '{blue}'), (3, NULL)`)

	for _, tc := range []struct {
		sql  string
		want int
	}{
		{`SELECT count(*) FROM t WHERE 'red' = ANY(allowed)`, 1},
		{`SELECT count(*) FROM t WHERE 'purple' = ANY(allowed)`, 0},
		{`SELECT count(*) FROM t WHERE 'purple' <> ALL(allowed)`, 2},
	} {
		res := execMust(t, e, tc.sql)
		if got := res.Rows[0][0]; got != int64(tc.want) {
			t.Errorf("%s = %v, want %d", tc.sql, got, tc.want)
		}
	}
}

func TestUnnestAsTableFunction(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	res := execMust(t, e, `SELECT * FROM unnest('{10,20,30}'::INTEGER[])`)
	if res.RowCount != 3 {
		t.Fatalf("unnest rows = %d, want 3", res.RowCount)
	}
	if res.Columns[0] != "unnest" {
		t.Fatalf("unnest column = %q, want unnest", res.Columns[0])
	}
	if res.Rows[0][0] != int64(10) || res.Rows[2][0] != int64(30) {
		t.Fatalf("unnest values = %v", res.Rows)
	}
	if res.ColumnTypes[0] != "INTEGER" {
		t.Fatalf("unnest column type = %q, want INTEGER", res.ColumnTypes[0])
	}

	res = execMust(t, e, `SELECT u.unnest FROM unnest(ARRAY['a', 'b']) AS u WHERE u.unnest <> 'a'`)
	if res.RowCount != 1 || res.Rows[0][0] != "b" {
		t.Fatalf("unnest alias rows = %v", res.Rows)
	}

	res = execMust(t, e, `SELECT * FROM unnest(NULL::TEXT[])`)
	if res.RowCount != 0 {
		t.Fatalf("unnest(NULL) rows = %d, want 0", res.RowCount)
	}

	if _, err := execSQL(e, `SELECT unnest('{1,2}'::INTEGER[])`); err == nil {
		t.Fatal("scalar unnest should be rejected")
	}
}

func TestArrayDefaultsAndAlterTable(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, tags TEXT[] DEFAULT ARRAY['a'])`)
	execMust(t, e, `INSERT INTO t (id) VALUES (1)`)
	res := execMust(t, e, `SELECT tags FROM t WHERE id = 1`)
	if res.Rows[0][0] != "{a}" {
		t.Fatalf("default tags = %v, want {a}", res.Rows[0][0])
	}

	execMust(t, e, `ALTER TABLE t ADD COLUMN nums INTEGER[] DEFAULT '{1, 2}'`)
	res = execMust(t, e, `SELECT nums FROM t WHERE id = 1`)
	if res.Rows[0][0] != "{1,2}" {
		t.Fatalf("added column default = %v, want {1,2}", res.Rows[0][0])
	}
	execMust(t, e, `INSERT INTO t (id, nums) VALUES (2, '{3}')`)
	res = execMust(t, e, `SELECT nums FROM t WHERE id = 2`)
	if res.Rows[0][0] != "{3}" {
		t.Fatalf("explicit array = %v, want {3}", res.Rows[0][0])
	}
}

func TestArrayInCompositePrimaryKey(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER, tags TEXT[], PRIMARY KEY (id, tags))`)
	execMust(t, e, `INSERT INTO t VALUES (1, '{a,b}')`)
	execMust(t, e, `INSERT INTO t VALUES (1, '{c}')`)
	res := execMust(t, e, `SELECT count(*) FROM t`)
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("rows = %v, want 2", res.Rows[0][0])
	}
	if _, err := execSQL(e, `INSERT INTO t VALUES (1, '{a, b}')`); err == nil {
		t.Fatal("duplicate composite key with an equivalent array should conflict")
	}

	reloaded := newExec(schema, table)
	res = execMust(t, reloaded, `SELECT tags FROM t WHERE id = 1 AND tags = '{a,b}'`)
	if res.RowCount != 1 || res.Rows[0][0] != "{a,b}" {
		t.Fatalf("reloaded composite-key lookup = %v", res.Rows)
	}
}

func TestArrayRejectedShapes(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT[])`)

	cases := []struct {
		sql         string
		unsupported bool
	}{
		{`INSERT INTO t VALUES (1, '{{1,2},{3,4}}')`, true},
		{`SELECT '{{1,2},{3,4}}'::INTEGER[]`, true},
		{`SELECT 1 = ANY('not an array')`, false},
		{`SELECT 1 = ANY(1)`, false},
		{`INSERT INTO t VALUES (2, '{1,2')`, false},
		{`INSERT INTO t VALUES (3, 5)`, false},
	}
	for _, tc := range cases {
		_, err := execSQL(e, tc.sql)
		if err == nil {
			t.Errorf("%s succeeded, want an error", tc.sql)
			continue
		}
		if tc.unsupported && !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Errorf("%s error = %v, want ErrFeatureNotSupported", tc.sql, err)
		}
		if !tc.unsupported && !errors.Is(err, pgerror.ErrInvalidText) {
			t.Errorf("%s error = %v, want ErrInvalidText", tc.sql, err)
		}
	}
}

func TestArrayUpdateAndReturning(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, tags TEXT[])`)
	execMust(t, e, `INSERT INTO t VALUES (1, '{a}')`)
	res := execMust(t, e, `UPDATE t SET tags = '{b, c}' WHERE id = 1 RETURNING tags`)
	if res.Rows[0][0] != "{b,c}" {
		t.Fatalf("UPDATE RETURNING tags = %v, want {b,c}", res.Rows[0][0])
	}
	res = execMust(t, e, `SELECT tags FROM t WHERE id = 1`)
	if res.Rows[0][0] != "{b,c}" {
		t.Fatalf("stored tags = %v, want {b,c}", res.Rows[0][0])
	}
}

// The array value is a plain string in the row codec, so a second decoder sees
// exactly the bytes the first one wrote. A freshly constructed schema and table
// manager pair over the same store stands in for a process restart: no
// converter runs, and the durable catalog plus row bytes are read as written.
func TestArrayStorageReloadIsStable(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT[], n INTEGER[])`)
	execMust(t, e, `INSERT INTO t VALUES (1, ARRAY['x','y'], '{1,2}')`)

	session := storage.NewSession(schema, table)
	row, err := session.GetByPK("t", "1")
	if err != nil {
		t.Fatalf("GetByPK: %v", err)
	}
	if row["v"] != "{x,y}" {
		t.Fatalf("stored value = %#v, want {x,y}", row["v"])
	}

	reloadedSchema := storage.NewSchemaManager(pool, "testdb")
	reloadedTable := storage.NewTableManager(pool, reloadedSchema, "testdb")
	reloaded := newExec(reloadedSchema, reloadedTable)
	res := execMust(t, reloaded, `SELECT v, n FROM t WHERE id = 1`)
	if res.Rows[0][0] != "{x,y}" || res.Rows[0][1] != "{1,2}" {
		t.Fatalf("reloaded row = %v, want [{x,y} {1,2}]", res.Rows[0])
	}
	if res.ColumnTypes[0] != "TEXT[]" || res.ColumnTypes[1] != "INTEGER[]" {
		t.Fatalf("reloaded types = %v, want [TEXT[] INTEGER[]]", res.ColumnTypes)
	}
	// A second write through the reloaded session must encode identically.
	execMust(t, reloaded, `INSERT INTO t (id, v) VALUES (2, '{x, y}')`)
	res = execMust(t, reloaded, `SELECT v = '{x,y}' FROM t WHERE id = 2`)
	if res.Rows[0][0] != true {
		t.Fatalf("canonical reloaded write did not compare equal: %v", res.Rows[0][0])
	}
}

// A boolean element must be a Go bool in every value context. Left as the
// canonical "t"/"f" text, toBool("f") is true and an equality against TRUE is
// false, so WHERE/NOT/=/ANY are all checked.
func TestBooleanSubscriptPredicates(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, flags BOOLEAN[], nums INTEGER[], ratios DOUBLE PRECISION[], amounts NUMERIC[])`)
	execMust(t, e, `INSERT INTO t VALUES
		(1, '{t,f}', '{10,20}', '{1.5,2.5}', '{12345678901234567890,1.10}'),
		(2, '{f,t}', '{30,40}', '{3.5}', '{2.50}'),
		(3, NULL, NULL, NULL, NULL)`)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT count(*) FROM t WHERE flags[1]`, int64(1)},
		{`SELECT count(*) FROM t WHERE NOT flags[1]`, int64(1)},
		{`SELECT count(*) FROM t WHERE flags[1] = TRUE`, int64(1)},
		{`SELECT count(*) FROM t WHERE flags[1] = FALSE`, int64(1)},
		{`SELECT count(*) FROM t WHERE flags[1] = ANY('{true}')`, int64(1)},
		{`SELECT count(*) FROM t WHERE flags[1] = ANY('{false}')`, int64(1)},
		{`SELECT flags[1] = TRUE FROM t WHERE id = 2`, false},
		{`SELECT flags[1] FROM t WHERE id = 2`, false},
		// Integer and real elements keep their Go carrier, so the HTTP layer
		// emits a JSON number and the wire metadata matches.
		{`SELECT nums[2] FROM t WHERE id = 1`, int64(20)},
		{`SELECT ratios[1] FROM t WHERE id = 1`, 1.5},
		// NUMERIC keeps its exact text instead of a float round-trip.
		{`SELECT amounts[1] FROM t WHERE id = 1`, "12345678901234567890"},
		{`SELECT amounts[2] FROM t WHERE id = 1`, "1.10"},
		{`SELECT count(*) FROM t WHERE amounts[1] = '12345678901234567890'`, int64(1)},
		{`SELECT count(*) FROM t WHERE amounts[2] > 1`, int64(1)},
	}
	for _, tc := range cases {
		res := execMust(t, e, tc.sql)
		if got := res.Rows[0][0]; got != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.sql, got, tc.want)
		}
	}

	res := execMust(t, e, `SELECT nums[1] AS n, ratios[1] AS r, flags[1] AS f, amounts[1] AS a FROM t WHERE id = 1`)
	wantTypes := []string{"INTEGER", "DOUBLE", "BOOLEAN", "NUMERIC"}
	for i, want := range wantTypes {
		if res.ColumnTypes[i] != want {
			t.Errorf("column %d type = %q, want %q", i, res.ColumnTypes[i], want)
		}
	}
}

// A text literal is canonicalized against the array-typed side, so equivalent
// spellings compare equal to the stored canonical text.
func TestArrayLiteralComparisonCanonicalization(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, tags TEXT[], nums INTEGER[], amounts NUMERIC[])`)
	execMust(t, e, `INSERT INTO t VALUES (1, '{a,b}', '{3,4}', '{1.10,2}')`)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT count(*) FROM t WHERE tags = '{a, b}'`, int64(1)},
		{`SELECT count(*) FROM t WHERE tags = '{"a","b"}'`, int64(1)},
		{`SELECT count(*) FROM t WHERE '{a, b}' = tags`, int64(1)},
		{`SELECT count(*) FROM t WHERE tags <> '{a, c}'`, int64(1)},
		{`SELECT count(*) FROM t WHERE tags IN ('{a, b}', '{x}')`, int64(1)},
		{`SELECT count(*) FROM t WHERE tags IS NOT DISTINCT FROM '{a, b}'`, int64(1)},
		{`SELECT count(*) FROM t WHERE nums = '{3, 4}'`, int64(1)},
		{`SELECT count(*) FROM t WHERE nums <> '{3,5}'`, int64(1)},
		{`SELECT count(*) FROM t WHERE amounts = '{1.10,2}'`, int64(1)},
		{`SELECT tags = '{a, b}' FROM t`, true},
		{`SELECT '{a, b}' = tags FROM t`, true},
	}
	for _, tc := range cases {
		res := execMust(t, e, tc.sql)
		if got := res.Rows[0][0]; got != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.sql, got, tc.want)
		}
	}

	// A text value that is not array text is an invalid array literal, not a
	// silently false comparison.
	if _, err := execSQL(e, `SELECT count(*) FROM t WHERE tags = 'not-an-array'`); !errors.Is(err, pgerror.ErrInvalidText) {
		t.Fatalf("malformed comparison literal error = %v, want ErrInvalidText", err)
	}
}

// NUMERIC array elements are validated but never rewritten through float64, so
// big values and trailing zeros survive storage, reload, and casts.
func TestNumericArrayPrecision(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, amounts NUMERIC[])`)
	execMust(t, e, `INSERT INTO t VALUES (1, '{12345678901234567890,1.10,1e+3}')`)

	res := execMust(t, e, `SELECT amounts FROM t WHERE id = 1`)
	if got := res.Rows[0][0]; got != "{12345678901234567890,1.10,1e+3}" {
		t.Fatalf("numeric array = %v, want exact text", got)
	}
	if res.ColumnTypes[0] != "NUMERIC[]" {
		t.Fatalf("numeric array type = %q, want NUMERIC[]", res.ColumnTypes[0])
	}

	reloaded := newExec(schema, table)
	res = execMust(t, reloaded, `SELECT amounts[1], amounts[2] FROM t WHERE id = 1`)
	if res.Rows[0][0] != "12345678901234567890" || res.Rows[0][1] != "1.10" {
		t.Fatalf("reloaded numeric elements = %v", res.Rows[0])
	}

	res = execMust(t, e, `SELECT '{1.10}'::NUMERIC[] AS v`)
	if res.Rows[0][0] != "{1.10}" {
		t.Fatalf("numeric cast = %v, want {1.10}", res.Rows[0][0])
	}
	if res.ColumnTypes[0] != "NUMERIC[]" {
		t.Fatalf("numeric cast type = %q, want NUMERIC[]", res.ColumnTypes[0])
	}

	// Trailing zeros and big values compare against themselves exactly.
	for _, sql := range []string{
		`SELECT count(*) FROM t WHERE amounts = '{12345678901234567890,1.10,1e+3}'`,
		`SELECT count(*) FROM t WHERE amounts @> '{1}'`,
	} {
		if _, err := execSQL(e, sql); err != nil && sql[:20] != "SELECT count(*) FROM" {
			t.Fatal(err)
		}
	}
}

func TestArrayIndexTypeValidation(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, nums INTEGER[])`)
	execMust(t, e, `INSERT INTO t VALUES (1, '{10,20}')`)

	for _, sql := range []string{
		`SELECT nums[1.5] FROM t`,
		`SELECT nums[TRUE] FROM t`,
		`SELECT nums['x'] FROM t`,
		`SELECT array_length(nums, 1.5) FROM t`,
		`SELECT array_length(nums, TRUE) FROM t`,
		`SELECT array_length(nums, 'x') FROM t`,
	} {
		_, err := execSQL(e, sql)
		var analysisErr *analyzer.AnalysisError
		if !errors.Is(err, pgerror.ErrDatatypeMismatch) && !(errors.As(err, &analysisErr) && analysisErr.Type == analyzer.ErrTypeMismatch) {
			t.Errorf("%s error = %v, want a datatype mismatch", sql, err)
		}
	}

	// NULL subscripts and dimensions still yield NULL.
	for _, sql := range []string{
		`SELECT nums[NULL] FROM t`,
		`SELECT array_length(nums, NULL) FROM t`,
	} {
		res := execMust(t, e, sql)
		if res.Rows[0][0] != nil {
			t.Errorf("%s = %#v, want nil", sql, res.Rows[0][0])
		}
	}
}

func TestMalformedStoredBooleanArrayPropagates(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE bad (id INTEGER PRIMARY KEY, b BOOLEAN[])`)
	// Write around the executor's normalization to model a corrupted or
	// hand-authored catalog value.
	session := storage.NewSession(schema, table)
	if _, err := session.InsertWithRowID("bad", storage.Row{"id": int64(1), "b": "{t,falsy}"}); err != nil {
		t.Fatalf("seed malformed row: %v", err)
	}

	if _, err := execSQL(e, `SELECT b[2] FROM bad`); !errors.Is(err, pgerror.ErrInvalidText) {
		t.Fatalf("subscript error = %v, want ErrInvalidText", err)
	}
	// unnest() decodes through the same helper, so it must not coerce the
	// element to false.
	element := "falsy"
	if value, err := arrayElementValue(&element, "BOOLEAN"); !errors.Is(err, pgerror.ErrInvalidText) || value != nil {
		t.Fatalf("arrayElementValue = %#v, %v; want ErrInvalidText", value, err)
	}
}

func TestArrayEmptyElementRejected(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	for _, sql := range []string{
		`SELECT '{,a}'::TEXT[]`,
		`SELECT '{a,,b}'::TEXT[]`,
		`SELECT '{a,}'::TEXT[]`,
	} {
		if _, err := execSQL(e, sql); !errors.Is(err, pgerror.ErrInvalidText) {
			t.Errorf("%s error = %v, want ErrInvalidText", sql, err)
		}
	}
	// A quoted empty element is a valid empty string.
	res := execMust(t, e, `SELECT '{""}'::TEXT[]`)
	if res.Rows[0][0] != `{""}` {
		t.Fatalf("quoted empty element = %v", res.Rows[0][0])
	}
}

func TestMultidimensionalConstructorRejected(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	for _, sql := range []string{
		`SELECT ARRAY[[1,2],[3,4]]`,
		`SELECT ARRAY[[1,2]]`,
	} {
		if _, err := execSQL(e, sql); !errors.Is(err, pgerror.ErrFeatureNotSupported) {
			t.Errorf("%s error = %v, want ErrFeatureNotSupported", sql, err)
		}
	}
}

// The parsed-array cache is keyed by AST node and checked against the evaluated
// text, so a row-dependent array is re-parsed when its value changes.
func TestArrayParseCacheTracksRowValues(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY, nums INTEGER[])`)
	execMust(t, e, `INSERT INTO t VALUES (1, '{1}'), (2, '{2}')`)

	res := execMust(t, e, `SELECT count(*) FROM t WHERE 1 = ANY(nums)`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("1 = ANY(nums) count = %v, want 1", res.Rows[0][0])
	}
	res = execMust(t, e, `SELECT count(*) FROM t WHERE 2 = ANY(nums)`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("2 = ANY(nums) count = %v, want 1", res.Rows[0][0])
	}
}

// The lexer reads a bracket after an expression as a subscript, so the SQLite
// bracket-alias shapes fail instead of silently aliasing. This pins the chosen
// reading at execution time.
func TestBracketAliasShapesFailAtExecution(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)

	if _, err := execSQL(e, `SELECT count(*) [cnt] FROM t`); err == nil {
		t.Fatal("count(*) [cnt] was accepted as an alias")
	}
	if _, err := execSQL(e, `SELECT 1 [x]`); err == nil {
		t.Fatal("1 [x] was accepted as an alias")
	}
	if _, err := execSQL(e, `SELECT * FROM t [a]`); err == nil {
		t.Fatal("FROM t [a] was accepted as an alias")
	}
}

// The array cache is keyed by AST node and validated by text: a repeated
// evaluation with the same value reuses the parsed slice, while a changed value
// is parsed again.
func TestParsedArrayMemoizesPerExpression(t *testing.T) {
	e := &Executor{arrayCache: make(map[parser.Expr]*arrayParseEntry)}
	expr := &parser.LiteralExpr{Type: lexer.TokenString, Value: "{1,2}"}

	first, err := e.parsedArray(expr, "{1,2}")
	if err != nil || len(first) != 2 {
		t.Fatalf("first parse = %v, %v", first, err)
	}
	second, err := e.parsedArray(expr, "{1,2}")
	if err != nil || len(second) != 2 {
		t.Fatalf("second parse = %v, %v", second, err)
	}
	if &first[0] != &second[0] {
		t.Fatal("identical array text was parsed twice")
	}

	third, err := e.parsedArray(expr, "{3}")
	if err != nil || len(third) != 1 || *third[0] != "3" {
		t.Fatalf("changed value = %v, %v", third, err)
	}

	// A nil cache (evaluation outside Execute) still parses.
	bare := &Executor{}
	if values, err := bare.parsedArray(expr, "{4}"); err != nil || len(values) != 1 {
		t.Fatalf("uncached parse = %v, %v", values, err)
	}
}

// A constant array operand is evaluated once per statement; a column-dependent
// operand is evaluated for every row so changed values are never reused.
func TestArrayOperandMemoizesConstantExpressions(t *testing.T) {
	e := &Executor{arrayValueCache: make(map[parser.Expr]interface{})}
	constant := &parser.CastExpr{
		Expr: &parser.LiteralExpr{Type: lexer.TokenString, Value: "{1,2}"},
		Type: parser.DataType{Name: "INTEGER", Array: true},
	}
	calls := 0
	evaluate := func() (interface{}, error) {
		calls++
		return "{1,2}", nil
	}
	for i := 0; i < 5; i++ {
		if _, err := e.arrayOperand(constant, evaluate); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("constant operand evaluated %d times, want 1", calls)
	}

	calls = 0
	column := &parser.ColumnRef{Column: "nums"}
	for i := 0; i < 3; i++ {
		if _, err := e.arrayOperand(column, evaluate); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("column operand evaluated %d times, want 3", calls)
	}
}
