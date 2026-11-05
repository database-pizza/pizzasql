package executor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/analyzer"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// jsonbExec parses and executes one statement, returning the value at (0, 0).
func jsonbValue(t *testing.T, e *Executor, sql string) interface{} {
	t.Helper()
	res := execMust(t, e, sql)
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		t.Fatalf("%s: no value", sql)
	}
	return res.Rows[0][0]
}

func jsonbErrorCode(t *testing.T, err error) string {
	t.Helper()
	switch {
	case err == nil:
		return ""
	case errors.Is(err, pgerror.ErrInvalidText):
		return pgerror.CodeInvalidText
	case errors.Is(err, pgerror.ErrInvalidParameter):
		return pgerror.CodeInvalidParameter
	case errors.Is(err, pgerror.ErrFeatureNotSupported):
		return pgerror.CodeFeatureNotSupported
	case errors.Is(err, pgerror.ErrDatatypeMismatch):
		return pgerror.CodeDatatypeMismatch
	}
	var analysisErr *analyzer.AnalysisError
	if errors.As(err, &analysisErr) && analysisErr.Type == analyzer.ErrTypeMismatch {
		return pgerror.CodeDatatypeMismatch
	}
	return "other:" + err.Error()
}

func TestJSONBCanonicalization(t *testing.T) {
	pool, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE docs (id INTEGER PRIMARY KEY, doc JSONB)")
	execMust(t, e, `INSERT INTO docs VALUES (1, '{"b": 1, "a": 2, "b": 3}')`)
	execMust(t, e, `INSERT INTO docs VALUES (2, '{"z": 1.50, "y": 1e2, "x": -0.0, "w": 1.0}')`)
	execMust(t, e, `INSERT INTO docs VALUES (3, ' [ 1 , { "b" : 2 , "a" : 1 } ] ')`)

	want := map[int64]string{
		1: `{"a":2,"b":3}`,
		2: `{"w":1,"x":0,"y":100,"z":1.5}`,
		3: `[1,{"a":1,"b":2}]`,
	}
	res := execMust(t, e, "SELECT id, doc FROM docs ORDER BY id")
	for _, row := range res.Rows {
		id := row[0].(int64)
		if row[1] != want[id] {
			t.Fatalf("row %d doc = %v, want %s", id, row[1], want[id])
		}
	}

	// A JSONB column rejects malformed documents and stores nothing.
	if _, err := execSQL(e, `INSERT INTO docs VALUES (4, 'not json')`); jsonbErrorCode(t, err) != pgerror.CodeInvalidText {
		t.Fatalf("malformed JSONB insert error = %v", err)
	}
	if res := execMust(t, e, "SELECT COUNT(*) FROM docs WHERE id = 4"); res.Rows[0][0] != int64(0) {
		t.Fatalf("malformed JSONB insert stored a row")
	}

	// Non-text input is not silently stringified, matching array columns.
	if _, err := execSQL(e, `INSERT INTO docs VALUES (5, 5)`); jsonbErrorCode(t, err) != pgerror.CodeInvalidText {
		t.Fatalf("integer JSONB insert error = %v", err)
	}

	// Canonicalization is idempotent, and a fresh executor over the same store
	// reads back the same bytes (no hidden normalization at read time).
	reloadedSchema := storage.NewSchemaManager(pool, "testdb")
	reloadedTable := storage.NewTableManager(pool, reloadedSchema, "testdb")
	reloaded := newExec(reloadedSchema, reloadedTable)
	res = execMust(t, reloaded, "SELECT id, doc FROM docs ORDER BY id")
	for _, row := range res.Rows {
		id := row[0].(int64)
		if row[1] != want[id] {
			t.Fatalf("reloaded row %d doc = %v, want %s", id, row[1], want[id])
		}
		canonical, err := CanonicalizeJSONB(row[1].(string))
		if err != nil || canonical != row[1] {
			t.Fatalf("row %d is not a canonical fixed point: %q (%v)", id, row[1], err)
		}
	}
}

func TestJSONBCastsAndToJSONB(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT '{"b":2,"a":1}'::jsonb`, `{"a":1,"b":2}`},
		{`SELECT CAST(' [1, 2] ' AS JSONB)`, `[1,2]`},
		{`SELECT ('{"a":1}'::jsonb)::jsonb`, `{"a":1}`},
		{`SELECT to_jsonb(5)`, `5`},
		{`SELECT to_jsonb('x')`, `"x"`},
		{`SELECT to_jsonb(TRUE)`, `true`},
		{`SELECT to_jsonb(NULL)`, nil},
		{`SELECT to_jsonb(ARRAY[1,2])`, `[1,2]`},
		{`SELECT to_jsonb(ARRAY['a','b'])`, `["a","b"]`},
		{`SELECT to_jsonb('{"a":1}'::jsonb)`, `{"a":1}`},
		{`SELECT CAST('{"a":1}'::jsonb AS TEXT)`, `{"a":1}`},
	}
	for _, tc := range cases {
		if got := jsonbValue(t, e, tc.sql); got != tc.want {
			t.Errorf("%s = %v (%T), want %v", tc.sql, got, got, tc.want)
		}
	}

	for _, sql := range []string{
		`SELECT 'not json'::jsonb`,
		`SELECT '{"a":}'::jsonb`,
		`SELECT '{"a":1} trailing'::jsonb`,
		`SELECT 5::jsonb`,
		`SELECT '{"a":1e999999}'::jsonb`,
	} {
		if _, err := execSQL(e, sql); jsonbErrorCode(t, err) != pgerror.CodeInvalidText {
			t.Errorf("%s error = %v, want 22P02", sql, err)
		}
	}

	// The JSON1 normalization function and its json() sibling keep working.
	if got := jsonbValue(t, e, `SELECT jsonb('{"a":1}')`); got != `{"a":1}` {
		t.Fatalf("jsonb() = %v", got)
	}
	if got := jsonbValue(t, e, `SELECT jsonb_valid('{"a":1}')`); got != int64(1) {
		t.Fatalf("jsonb_valid() = %v", got)
	}
}

func TestJSONBArrowOperators(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE docs (id INTEGER PRIMARY KEY, doc JSONB)")
	execMust(t, e, `INSERT INTO docs VALUES (1, '{"a":1,"s":"x","n":null,"arr":[10,{"b":2}]}')`)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT doc -> 'a' FROM docs`, `1`},
		{`SELECT doc -> 's' FROM docs`, `"x"`},
		{`SELECT doc ->> 's' FROM docs`, `x`},
		{`SELECT doc -> 'n' FROM docs`, `null`},
		{`SELECT doc ->> 'n' FROM docs`, nil},
		{`SELECT doc -> 'missing' FROM docs`, nil},
		{`SELECT doc ->> 'missing' FROM docs`, nil},
		{`SELECT doc -> 'arr' -> 0 FROM docs`, `10`},
		{`SELECT doc -> 'arr' -> -1 ->> 'b' FROM docs`, `2`},
		{`SELECT doc -> 'arr' -> 3 FROM docs`, nil},
		{`SELECT doc -> '$."arr"[1]' ->> 'b' FROM docs`, `2`},
		{`SELECT NULL::jsonb -> 'a'`, nil},
		{`SELECT doc -> NULL FROM docs`, nil},
	}
	for _, tc := range cases {
		if got := jsonbValue(t, e, tc.sql); got != tc.want {
			t.Errorf("%s = %v (%T), want %v", tc.sql, got, got, tc.want)
		}
	}

	// The JSON text surface is unchanged: a text-backed document still returns
	// JSON1 coercions for ->>.
	if got := jsonbValue(t, e, `SELECT '{"a":true}' ->> 'a'`); got != int64(1) {
		t.Fatalf("JSON1 ->> boolean = %v (%T), want int64 1", got, got)
	}
}

func TestJSONBPathOperators(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT '{"a":{"b":2}}'::jsonb #> '{a,b}'`, `2`},
		{`SELECT '{"a":{"b":2}}'::jsonb #>> '{a,b}'`, `2`},
		{`SELECT '{"a":[1,2,3]}'::jsonb #> '{a,-1}'`, `3`},
		{`SELECT '{"a":{"b":2}}'::jsonb #> '$."a"."b"'`, `2`},
		{`SELECT '{"a":{"b":2}}'::jsonb #> ARRAY['a','b']`, `2`},
		{`SELECT '{"a":1}'::jsonb #> '{missing}'`, nil},
		{`SELECT '{"a":1}'::jsonb #- '{a}'`, `{}`},
		{`SELECT '{"a":{"b":1},"c":2}'::jsonb #- '{a,b}'`, `{"a":{},"c":2}`},
		{`SELECT '[1,2,3]'::jsonb #- '{1}'`, `[1,3]`},
		{`SELECT '{"a":1}'::jsonb #- '{missing}'`, `{"a":1}`},
		{`SELECT NULL::jsonb #> '{a}'`, nil},
	}
	for _, tc := range cases {
		if got := jsonbValue(t, e, tc.sql); got != tc.want {
			t.Errorf("%s = %v (%T), want %v", tc.sql, got, got, tc.want)
		}
	}

	// Wildcards, slices, and unbounded expansion are rejected, not approximated.
	for _, sql := range []string{
		`SELECT '{"a":1}'::jsonb #> '$[*]'`,
		`SELECT '{"a":1}'::jsonb #> '$.**'`,
		`SELECT '{"a":1}'::jsonb -> '$[*]'`,
		`SELECT '{"a":1}'::jsonb #- '$[0:1]'`,
	} {
		if _, err := execSQL(e, sql); jsonbErrorCode(t, err) != pgerror.CodeFeatureNotSupported {
			t.Errorf("%s error = %v, want 0A000", sql, err)
		}
	}
}

func TestJSONBContainmentAndExistence(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT '{"a":1,"b":{"c":2}}'::jsonb @> '{"a":1}'::jsonb`, true},
		{`SELECT '{"a":1,"b":{"c":2}}'::jsonb @> '{"b":{"c":2}}'::jsonb`, true},
		{`SELECT '{"a":1,"b":{"c":2}}'::jsonb @> '{"b":{}}'::jsonb`, true},
		{`SELECT '{"a":1}'::jsonb @> '{"b":1}'::jsonb`, false},
		{`SELECT '{"a":1}'::jsonb @> '{"a":1,"b":2}'::jsonb`, false},
		{`SELECT '[1,2,3]'::jsonb @> '[3,1]'::jsonb`, true},
		{`SELECT '[1,2,3]'::jsonb @> '[4]'::jsonb`, false},
		{`SELECT '[1,2]'::jsonb @> '1'::jsonb`, true},
		{`SELECT '["a"]'::jsonb @> '"a"'::jsonb`, true},
		{`SELECT '[true]'::jsonb @> 'true'::jsonb`, true},
		{`SELECT '[null]'::jsonb @> 'null'::jsonb`, true},
		{`SELECT '[1]'::jsonb @> '2'::jsonb`, false},
		{`SELECT '{"a":[1,2]}'::jsonb @> '{"a":[2]}'::jsonb`, true},
		{`SELECT '{"a":null}'::jsonb @> '{"a":null}'::jsonb`, true},
		{`SELECT '{"a":null}'::jsonb @> '{"a":1}'::jsonb`, false},
		{`SELECT '{"a":1.0}'::jsonb @> '{"a":1}'::jsonb`, true},
		{`SELECT '{"a":1}'::jsonb <@ '{"a":1,"b":2}'::jsonb`, true},
		{`SELECT '{"a":1}'::jsonb @> '{"a":1}'`, true},
		{`SELECT NULL::jsonb @> '{}'::jsonb`, nil},
		{`SELECT '{"a":1}'::jsonb @> NULL::jsonb`, nil},
		{`SELECT '{"a":1}'::jsonb ? 'a'`, true},
		{`SELECT '{"a":1}'::jsonb ? 'b'`, false},
		{`SELECT '[1,"x"]'::jsonb ? 'x'`, true},
		{`SELECT '[1,"x"]'::jsonb ? '1'`, false},
		{`SELECT '"x"'::jsonb ? 'x'`, true},
		{`SELECT 'null'::jsonb ? 'x'`, false},
		{`SELECT '{"a":1,"b":2}'::jsonb ?| '{b,c}'`, true},
		{`SELECT '{"a":1,"b":2}'::jsonb ?| '{c,d}'`, false},
		{`SELECT '{"a":1,"b":2}'::jsonb ?& '{a,b}'`, true},
		{`SELECT '{"a":1,"b":2}'::jsonb ?& '{a,c}'`, false},
		{`SELECT '{"1":true}'::jsonb ? '1'`, true},
		{`SELECT '{"1":true}'::jsonb ?| '{1,2}'`, true},
		{`SELECT '[1,"x"]'::jsonb ?| '{x}'`, true},
		{`SELECT NULL::jsonb ? 'a'`, nil},
	}
	for _, tc := range cases {
		if got := jsonbValue(t, e, tc.sql); got != tc.want {
			t.Errorf("%s = %v (%T), want %v", tc.sql, got, got, tc.want)
		}
	}

	// Containment needs a jsonb operand on at least one side.
	if _, err := execSQL(e, `SELECT '{"a":1}' @> '{"a":1}'`); jsonbErrorCode(t, err) != pgerror.CodeDatatypeMismatch {
		t.Errorf("text @> text error = %v, want 42804", err)
	}
}

func TestJSONBConcatAndDelete(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT '{"a":1}'::jsonb || '{"b":2,"a":3}'::jsonb`, `{"a":3,"b":2}`},
		{`SELECT '[1]'::jsonb || '[2,3]'::jsonb`, `[1,2,3]`},
		{`SELECT '[1]'::jsonb || '2'::jsonb`, `[1,2]`},
		{`SELECT '{"a":1}'::jsonb || '[1]'::jsonb`, `[{"a":1},1]`},
		{`SELECT '1'::jsonb || '2'::jsonb`, `[1,2]`},
		{`SELECT NULL::jsonb || '{}'::jsonb`, nil},
		{`SELECT '{"a":1,"b":2}'::jsonb - 'a'`, `{"b":2}`},
		{`SELECT '{"a":1,"b":2}'::jsonb - '{a,b}'::text[]`, `{}`},
		{`SELECT '[1,2,3]'::jsonb - 1`, `[1,3]`},
		{`SELECT '[1,2,3]'::jsonb - -1`, `[1,2]`},
		{`SELECT '[1,2,3]'::jsonb - 9`, `[1,2,3]`},
		{`SELECT '["a","b","a"]'::jsonb - 'a'`, `["b"]`},
		{`SELECT '"x"'::jsonb - 'a'`, `"x"`},
		{`SELECT NULL::jsonb - 'a'`, nil},
	}
	for _, tc := range cases {
		if got := jsonbValue(t, e, tc.sql); got != tc.want {
			t.Errorf("%s = %v (%T), want %v", tc.sql, got, got, tc.want)
		}
	}

	// Text concatenation is unchanged when neither side is jsonb.
	if got := jsonbValue(t, e, `SELECT 'a' || 'b'`); got != "ab" {
		t.Fatalf("text concatenation = %v", got)
	}
	if _, err := execSQL(e, `SELECT '{"a":1}'::jsonb - 1`); jsonbErrorCode(t, err) != pgerror.CodeInvalidParameter {
		t.Errorf("jsonb - integer on object error = %v, want 22023", err)
	}
}

func TestJSONBScalarFunctions(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT jsonb_typeof('{"a":1}')`, "object"},
		{`SELECT jsonb_typeof('[1]')`, "array"},
		{`SELECT jsonb_typeof('"x"')`, "string"},
		{`SELECT jsonb_typeof('1.50')`, "number"},
		{`SELECT jsonb_typeof('true')`, "boolean"},
		{`SELECT jsonb_typeof('null')`, "null"},
		{`SELECT jsonb_typeof(NULL)`, nil},
		{`SELECT jsonb_array_length('[1,2,3]')`, int64(3)},
		{`SELECT jsonb_array_length('[]')`, int64(0)},
		{`SELECT jsonb_array_length(NULL)`, nil},
		{`SELECT jsonb_pretty('{"b":2,"a":[1]}')`, "{\n    \"a\": [\n        1\n    ],\n    \"b\": 2\n}"},
		{`SELECT jsonb_strip_nulls('{"a":null,"b":{"c":null,"d":1},"e":[null,1]}')`, `{"b":{"d":1},"e":[null,1]}`},
		{`SELECT jsonb_build_object('b', 1, 'a', 'x')`, `{"a":"x","b":1}`},
		{`SELECT jsonb_build_object('a', NULL)`, `{"a":null}`},
		{`SELECT jsonb_build_object('a', '{"b":1}'::jsonb)`, `{"a":{"b":1}}`},
		{`SELECT jsonb_build_array(1, 'x', NULL, TRUE)`, `[1,"x",null,true]`},
		{`SELECT jsonb_build_array()`, `[]`},
		{`SELECT jsonb_build_object()`, `{}`},
		{`SELECT jsonb_set('{"a":1}', '{a}', '2')`, `{"a":2}`},
		{`SELECT jsonb_set('{"a":1}', '{b}', '2')`, `{"a":1,"b":2}`},
		{`SELECT jsonb_set('{"a":1}', '{b}', '2', false)`, `{"a":1}`},
		{`SELECT jsonb_set('{"a":{"b":1}}', '{a,b}', '2')`, `{"a":{"b":2}}`},
		{`SELECT jsonb_set('{"a":{"b":1}}', '{x,b}', '2')`, `{"a":{"b":1}}`},
		{`SELECT jsonb_set('[1,2]', '{2}', '3')`, `[1,2,3]`},
		{`SELECT jsonb_set('[1,2]', '{5}', '3')`, `[1,2,3]`},
		{`SELECT jsonb_set('[1,2]', '{5}', '3', false)`, `[1,2]`},
		{`SELECT jsonb_set('[1,2]', '{-3}', '9')`, `[9,1,2]`},
		{`SELECT jsonb_set('[1,2]', '{-3}', '9', false)`, `[1,2]`},
		{`SELECT jsonb_set('{"a":1}', '{}', '2')`, `{"a":1}`},
		{`SELECT jsonb_insert('{"a":1}', '{}', '2')`, `{"a":1}`},
		{`SELECT jsonb_set('{"a":1}', '{a,b}', '2')`, `{"a":1}`},
		{`SELECT jsonb_insert('{"a":1}', '{a,b}', '2')`, `{"a":1}`},
		{`SELECT jsonb_set('{"a":1}', NULL, '2')`, nil},
		{`SELECT jsonb_insert('{"a":1}', NULL, '2')`, nil},
		{`SELECT jsonb_insert('{"a":[0,1,2]}', '{a,1}', '"n"')`, `{"a":[0,"n",1,2]}`},
		{`SELECT jsonb_insert('{"a":[0,1,2]}', '{a,1}', '"n"', true)`, `{"a":[0,1,"n",2]}`},
		{`SELECT jsonb_insert('{"a":1}', '{b}', '2')`, `{"a":1,"b":2}`},
		{`SELECT jsonb_insert('[0,1,2]', '{5}', '9')`, `[0,1,2,9]`},
		{`SELECT jsonb_insert('[0,1,2]', '{-3}', '9')`, `[9,0,1,2]`},
		{`SELECT jsonb_insert('[0,1,2]', '{-1}', '9', true)`, `[0,1,2,9]`},
		{`SELECT jsonb_insert('[0,1,2]', '{-2}', '9')`, `[0,9,1,2]`},
		{`SELECT jsonb_insert('[0,1,2]', '{-2}', '9', true)`, `[0,1,9,2]`},
		{`SELECT jsonb_set('{"a":1}', '{a}', NULL)`, nil},
		{`SELECT jsonb_insert('[0,1]', '{-1}', '9')`, `[0,9,1]`},
		// The JSON1 $ path form keeps its previous value semantics: a plain SQL
		// string becomes a JSON string, while a jsonb-typed value is embedded.
		{`SELECT jsonb_set('{"a":1}', '$.b', '2')`, `{"a":1,"b":"2"}`},
		{`SELECT jsonb_set('{"a":1}', '$.a', '2')`, `{"a":"2"}`},
		{`SELECT jsonb_set('{"a":1}', '$.b', '{"x":1}'::jsonb)`, `{"a":1,"b":{"x":1}}`},
		{`SELECT jsonb_insert('{"a":1}', '$.b', '2')`, `{"a":1,"b":"2"}`},
		{`SELECT jsonb_replace('{"a":1}', '$.a', '2')`, `{"a":"2"}`},
		{`SELECT jsonb_remove('{"a":1,"b":2}', '$.a')`, `{"b":2}`},
	}
	for _, tc := range cases {
		if got := jsonbValue(t, e, tc.sql); got != tc.want {
			t.Errorf("%s = %v (%T), want %v", tc.sql, got, got, tc.want)
		}
	}

	// JSON1 scalar functions keep their text surface.
	if got := jsonbValue(t, e, `SELECT json_type('{"a":1.5}', '$.a')`); got != "real" {
		t.Fatalf("json_type text surface = %v", got)
	}
	if got := jsonbValue(t, e, `SELECT json_set('{"a":1}', '$.b', '2')`); got != `{"a":1,"b":"2"}` {
		t.Fatalf("json_set text surface = %v", got)
	}

	for _, tc := range []struct {
		sql  string
		code string
	}{
		{`SELECT jsonb_array_length('{"a":1}')`, pgerror.CodeInvalidParameter},
		{`SELECT jsonb_typeof('not json')`, pgerror.CodeInvalidText},
		{`SELECT jsonb_build_object('a')`, pgerror.CodeInvalidParameter},
		{`SELECT jsonb_build_object(NULL, 1)`, pgerror.CodeInvalidParameter},
		{`SELECT jsonb_set('[1,2]', '{a}', '2')`, pgerror.CodeInvalidText},
		{`SELECT jsonb_insert('[1,2]', '{a}', '2')`, pgerror.CodeInvalidText},
		{`SELECT jsonb_insert('{"a":1}', '{a}', '2')`, pgerror.CodeInvalidParameter},
		{`SELECT jsonb_set('1', '{a}', '2')`, pgerror.CodeInvalidParameter},
		{`SELECT jsonb_insert('1', '{a}', '2')`, pgerror.CodeInvalidParameter},
	} {
		if _, err := execSQL(e, tc.sql); jsonbErrorCode(t, err) != tc.code {
			t.Errorf("%s error = %v, want %s", tc.sql, err, tc.code)
		}
	}
}

func TestJSONBSetReturningFunctionsInFrom(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	res := execMust(t, e, `SELECT * FROM jsonb_each('{"b":2,"a":{"c":1}}')`)
	if len(res.Columns) != 2 || res.Columns[0] != "key" || res.Columns[1] != "value" {
		t.Fatalf("jsonb_each columns = %v", res.Columns)
	}
	if res.RowCount != 2 || res.Rows[0][0] != "a" || res.Rows[0][1] != `{"c":1}` || res.Rows[1][0] != "b" || res.Rows[1][1] != `2` {
		t.Fatalf("jsonb_each rows = %v", res.Rows)
	}
	if len(res.ColumnTypes) != 2 || res.ColumnTypes[1] != "JSONB" {
		t.Fatalf("jsonb_each column types = %v", res.ColumnTypes)
	}

	res = execMust(t, e, `SELECT e.key, e.value FROM jsonb_each('{"a":1}') AS e`)
	if res.RowCount != 1 || res.Rows[0][0] != "a" || res.Rows[0][1] != `1` {
		t.Fatalf("aliased jsonb_each rows = %v", res.Rows)
	}

	res = execMust(t, e, `SELECT key, value FROM jsonb_each_text('{"a":null,"b":true}')`)
	if res.RowCount != 2 || res.Rows[0][0] != "a" || res.Rows[0][1] != nil || res.Rows[1][1] != "true" {
		t.Fatalf("jsonb_each_text rows = %v", res.Rows)
	}

	res = execMust(t, e, `SELECT value FROM jsonb_array_elements('[1,{"a":2}]')`)
	if res.RowCount != 2 || res.Rows[0][0] != `1` || res.Rows[1][0] != `{"a":2}` {
		t.Fatalf("jsonb_array_elements rows = %v", res.Rows)
	}
	if res.ColumnTypes[0] != "JSONB" {
		t.Fatalf("jsonb_array_elements type = %v", res.ColumnTypes)
	}

	res = execMust(t, e, `SELECT value FROM jsonb_array_elements_text('[1,null,"x"]')`)
	if res.RowCount != 3 || res.Rows[0][0] != "1" || res.Rows[1][0] != nil || res.Rows[2][0] != "x" {
		t.Fatalf("jsonb_array_elements_text rows = %v", res.Rows)
	}

	res = execMust(t, e, `SELECT jsonb_object_keys FROM jsonb_object_keys('{"b":1,"a":2}')`)
	if res.RowCount != 2 || res.Rows[0][0] != "a" || res.Rows[1][0] != "b" {
		t.Fatalf("jsonb_object_keys rows = %v", res.Rows)
	}

	// A NULL document produces no rows.
	res = execMust(t, e, `SELECT * FROM jsonb_array_elements(NULL)`)
	if res.RowCount != 0 {
		t.Fatalf("jsonb_array_elements(NULL) rows = %v", res.Rows)
	}

	// Wrong document types and projections of set-returning functions are
	// rejected clearly instead of executing as scalar calls.
	for _, tc := range []struct {
		sql  string
		code string
	}{
		{`SELECT * FROM jsonb_array_elements('{"a":1}')`, pgerror.CodeInvalidParameter},
		{`SELECT * FROM jsonb_each('[1]')`, pgerror.CodeInvalidParameter},
		{`SELECT * FROM jsonb_object_keys('[1]')`, pgerror.CodeInvalidParameter},
		{`SELECT * FROM jsonb_each('null')`, pgerror.CodeInvalidParameter},
		{`SELECT * FROM jsonb_array_elements('null')`, pgerror.CodeInvalidParameter},
		{`SELECT jsonb_each('{"a":1}')`, pgerror.CodeFeatureNotSupported},
		{`SELECT jsonb_array_elements('[1]')`, pgerror.CodeFeatureNotSupported},
		{`SELECT * FROM jsonb_each('{"a":1}') AS e JOIN docs d ON TRUE`, pgerror.CodeFeatureNotSupported},
	} {
		if _, err := execSQL(e, tc.sql); jsonbErrorCode(t, err) != tc.code {
			t.Errorf("%s error = %v, want %s", tc.sql, err, tc.code)
		}
	}
}

func TestJSONBEqualityOrderingAndDistinct(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE docs (id INTEGER PRIMARY KEY, doc JSONB)")
	execMust(t, e, `INSERT INTO docs VALUES (1, '{"b":1,"a":2}'), (2, '{"a":2,"b":1}'), (3, '[1,2]')`)

	if got := jsonbValue(t, e, `SELECT '{"b":1,"a":2}'::jsonb = '{"a":2, "b": 1}'::jsonb`); got != true {
		t.Fatalf("jsonb equality = %v, want true", got)
	}
	if got := jsonbValue(t, e, `SELECT '{"a":1}'::jsonb = '[1]'::jsonb`); got != false {
		t.Fatalf("jsonb inequality = %v, want false", got)
	}
	if got := jsonbValue(t, e, `SELECT doc IS NOT DISTINCT FROM '{"a":2,"b":1}'::jsonb FROM docs WHERE id = 2`); got != true {
		t.Fatalf("IS NOT DISTINCT FROM = %v", got)
	}
	if got := jsonbValue(t, e, `SELECT '{"b":1,"a":2}'::jsonb IS DISTINCT FROM '{"a":2,"b":1.0}'::jsonb`); got != false {
		t.Fatalf("IS DISTINCT FROM key-order/number-spelling = %v, want false", got)
	}

	res := execMust(t, e, `SELECT COUNT(*) FROM docs WHERE doc = '{"a":2,"b":1}'::jsonb`)
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("key-order-independent equality matched %v rows, want 2", res.Rows[0][0])
	}

	res = execMust(t, e, `SELECT DISTINCT doc FROM docs ORDER BY doc`)
	if res.RowCount != 2 {
		t.Fatalf("DISTINCT jsonb rows = %v, want 2", res.Rows)
	}

	// Canonical numeric scalars order numerically, and non-scalars order by
	// canonical text.
	execMust(t, e, "CREATE TABLE scalars (id INTEGER PRIMARY KEY, v JSONB)")
	execMust(t, e, `INSERT INTO scalars VALUES (1, '10'), (2, '2')`)
	res = execMust(t, e, `SELECT v FROM scalars ORDER BY v`)
	if res.RowCount != 2 || res.Rows[0][0] != `2` || res.Rows[1][0] != `10` {
		t.Fatalf("jsonb scalar ordering = %v", res.Rows)
	}
	res = execMust(t, e, `SELECT doc FROM docs WHERE id IN (1,3) ORDER BY doc`)
	if res.RowCount != 2 || res.Rows[0][0] != `[1,2]` || res.Rows[1][0] != `{"a":2,"b":1}` {
		t.Fatalf("jsonb text ordering = %v", res.Rows)
	}

	// A jsonb column cannot be compared with a number.
	if _, err := execSQL(e, `SELECT id FROM docs WHERE doc = 5`); err == nil {
		t.Fatal("jsonb = integer was accepted")
	}
}

func TestJSONBRejectedShapes(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE docs (id INTEGER PRIMARY KEY, doc JSONB)")

	for _, sql := range []string{
		`SELECT jsonb_path_exists('{"a":1}', '$.a')`,
		`SELECT jsonb_path_query('{"a":1}', '$.a')`,
		`CREATE INDEX docs_gin ON docs USING GIN (doc)`,
		`CREATE INDEX docs_expr ON docs ((doc -> 'a'))`,
		`CREATE TABLE bad_arrays (doc JSONB[])`,
	} {
		if _, err := execSQL(e, sql); jsonbErrorCode(t, err) != pgerror.CodeFeatureNotSupported {
			t.Errorf("%s error = %v, want 0A000", sql, err)
		}
	}

	// A scalar extraction index over jsonb is a text expression and stays legal.
	execMust(t, e, `CREATE INDEX docs_kind ON docs ((doc ->> 'kind'))`)
}

func TestJSONBColumnMetadataAndDefaults(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE docs (id INTEGER PRIMARY KEY, doc JSONB DEFAULT '{"b":2,"a":1}')`)
	execMust(t, e, `INSERT INTO docs (id) VALUES (1)`)
	res := execMust(t, e, `SELECT doc FROM docs`)
	if res.Rows[0][0] != `{"a":1,"b":2}` {
		t.Fatalf("canonical default = %v", res.Rows[0][0])
	}
	if len(res.ColumnTypes) != 1 || res.ColumnTypes[0] != "JSONB" {
		t.Fatalf("column types = %v, want [JSONB]", res.ColumnTypes)
	}

	// ALTER TABLE backfills the canonical default into existing rows.
	execMust(t, e, "CREATE TABLE raw (id INTEGER PRIMARY KEY)")
	execMust(t, e, "INSERT INTO raw VALUES (1)")
	execMust(t, e, `ALTER TABLE raw ADD COLUMN doc JSONB DEFAULT '{"b":2,"a":1}'`)
	res = execMust(t, e, `SELECT doc FROM raw`)
	if res.Rows[0][0] != `{"a":1,"b":2}` {
		t.Fatalf("backfilled default = %v", res.Rows[0][0])
	}

	// UPDATE canonicalizes too.
	execMust(t, e, `UPDATE raw SET doc = '{"z": 1.50}'`)
	res = execMust(t, e, `SELECT doc FROM raw`)
	if res.Rows[0][0] != `{"z":1.5}` {
		t.Fatalf("updated doc = %v", res.Rows[0][0])
	}
}

func TestCanonicalJSONBNumber(t *testing.T) {
	cases := map[string]string{
		"0":                              "0",
		"-0":                             "0",
		"-0.0":                           "0",
		"1":                              "1",
		"1.0":                            "1",
		"1.50":                           "1.5",
		"1e2":                            "100",
		"1E+2":                           "100",
		"1e-2":                           "0.01",
		"-12.340":                        "-12.34",
		"123456789012345678901234567890": "123456789012345678901234567890",
	}
	for input, want := range cases {
		got, err := canonicalJSONBNumber(json.Number(input))
		if err != nil || got != want {
			t.Errorf("canonicalJSONBNumber(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"1e999999", "1e-999999"} {
		if _, err := canonicalJSONBNumber(json.Number(input)); err == nil {
			t.Errorf("canonicalJSONBNumber(%q) accepted an unbounded expansion", input)
		}
	}
}

// JSON operations must keep working from a derived table and a CTE, where the
// column type travels through Result column metadata.
func TestJSONBTypePropagation(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE docs (id INTEGER PRIMARY KEY, doc JSONB)")
	execMust(t, e, `INSERT INTO docs VALUES (1, '{"a":1}')`)

	if got := jsonbValue(t, e, `SELECT doc -> 'a' FROM (SELECT doc FROM docs) AS d`); got != `1` {
		t.Fatalf("derived jsonb -> = %v", got)
	}
	if got := jsonbValue(t, e, `SELECT doc ? 'a' FROM (SELECT doc FROM docs) AS d`); got != true {
		t.Fatalf("derived jsonb ? = %v", got)
	}
	if got := jsonbValue(t, e, `WITH c AS (SELECT doc FROM docs) SELECT doc @> '{"a":1}' FROM c`); got != true {
		t.Fatalf("CTE jsonb @> = %v", got)
	}
	if got := jsonbValue(t, e, `SELECT COALESCE(doc, '{}'::jsonb) ->> 'a' FROM docs`); got != "1" {
		t.Fatalf("COALESCE jsonb ->> = %v", got)
	}

	// A jsonb column keeps its type when it is the left side of a join and when
	// it is referenced through an alias.
	res := execMust(t, e, `SELECT a.id FROM docs AS a JOIN docs AS b ON a.id = b.id WHERE a.doc @> '{"a":1}'`)
	if res.RowCount != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("joined jsonb containment rows = %v", res.Rows)
	}
	if got := jsonbValue(t, e, `SELECT a.doc -> 'a' FROM docs AS a`); got != `1` {
		t.Fatalf("aliased jsonb -> = %v", got)
	}
	if got := jsonbValue(t, e, `SELECT MAX(jsonb_build_object('a', doc)) FROM docs`); got != `{"a":{"a":1}}` {
		t.Fatalf("jsonb build function inside an aggregate = %v", got)
	}
}

func TestJSONBRejectsUnboundedCanonicalExpansion(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	if _, err := execSQL(e, `SELECT '{"a":1e999999}'::jsonb`); !strings.Contains(err.Error(), "invalid input syntax") {
		t.Fatalf("unbounded number error = %v", err)
	}
}

// A PostgreSQL path element is resolved against the container it reaches:
// numeric-looking text is an object key in an object and an integer index in an
// array. This must hold for #>, #>>, #-, jsonb_set, and jsonb_insert.
func TestJSONBNumericLookingObjectKeys(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT '{"1":{"2":3}}'::jsonb #> '{1,2}'`, `3`},
		{`SELECT '{"1":{"2":3}}'::jsonb #>> '{1,2}'`, `3`},
		{`SELECT '{"1":{"2":3}}'::jsonb #- '{1,2}'`, `{"1":{}}`},
		{`SELECT jsonb_set('{"1":{"2":3}}', '{1,2}', '4')`, `{"1":{"2":4}}`},
		{`SELECT jsonb_insert('{"1":{"2":3}}', '{1,3}', '4')`, `{"1":{"2":3,"3":4}}`},
		{`SELECT '{"01":"x"}'::jsonb #> '{01}'`, `"x"`},
		{`SELECT '{"-1":"x"}'::jsonb #> '{-1}'`, `"x"`},
		{`SELECT '{"1.5":"x"}'::jsonb #> '{1.5}'`, `"x"`},
		{`SELECT jsonb_set('{"01":"x"}', '{01}', '"y"')`, `{"01":"y"}`},
		{`SELECT jsonb_insert('{"01":"x"}', '{02}', '"y"')`, `{"01":"x","02":"y"}`},
		{`SELECT jsonb_set('{"-1":"x"}', '{-1}', '"y"')`, `{"-1":"y"}`},
		{`SELECT jsonb_insert('{"-1":"x"}', '{-2}', '"y"')`, `{"-1":"x","-2":"y"}`},
		{`SELECT '{"1.5":"x"}'::jsonb #- '{1.5}'`, `{}`},
		{`SELECT jsonb_set('{"1.5":"x"}', '{1.5}', '"y"')`, `{"1.5":"y"}`},
		{`SELECT '[10,20]'::jsonb #> '{01}'`, `20`},
		{`SELECT '[10,20]'::jsonb #> '{-1}'`, `20`},
		{`SELECT '[10,20]'::jsonb #> '{a}'`, nil},
	}
	for _, tc := range cases {
		if got := jsonbValue(t, e, tc.sql); got != tc.want {
			t.Errorf("%s = %v (%T), want %v", tc.sql, got, got, tc.want)
		}
	}

	// A non-integer path element on an array is an error for #-, matching
	// PostgreSQL's path mutation and delete behavior.
	if _, err := execSQL(e, `SELECT '[10,20]'::jsonb #- '{a}'`); jsonbErrorCode(t, err) != pgerror.CodeInvalidText {
		t.Errorf("#- array non-integer element error = %v, want 22P02", err)
	}
}

// #- must produce jsonb metadata and keep jsonb semantics for chained
// operators, not fall back to text.
func TestJSONBPathDeleteKeepsJSONBType(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	res := execMust(t, e, `SELECT '{"t":true}'::jsonb #- '{x}'`)
	if len(res.ColumnTypes) != 1 || res.ColumnTypes[0] != "JSONB" {
		t.Fatalf("#- column type = %v, want JSONB", res.ColumnTypes)
	}
	if got := jsonbValue(t, e, `SELECT ('{"t":true}'::jsonb #- '{x}') ->> 't'`); got != "true" {
		t.Fatalf("#- ->> = %v (%T), want true text", got, got)
	}
	if got := jsonbValue(t, e, `SELECT ('{"t":true}'::jsonb #- '{x}') = '{"t":true}'::jsonb`); got != true {
		t.Fatalf("#- jsonb equality = %v", got)
	}
	if got := jsonbValue(t, e, `SELECT ('{"t":true}'::jsonb #- '{x}') ? 't'`); got != true {
		t.Fatalf("#- ? = %v", got)
	}
}

// jsonb equality and ordering use exact canonical bytes, not float64. DISTINCT,
// GROUP BY, and containment must agree with =.
func TestJSONBExactEqualityAndAggregateAgreement(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	for _, sql := range []string{
		`SELECT '1'::jsonb = '1.0000000000000000001'::jsonb`,
		`SELECT '9007199254740993'::jsonb = '9007199254740992'::jsonb`,
	} {
		if got := jsonbValue(t, e, sql); got != false {
			t.Errorf("%s = %v, want false", sql, got)
		}
	}
	if got := jsonbValue(t, e, `SELECT '1'::jsonb = '1.0'::jsonb`); got != true {
		t.Fatalf("1 = 1.0 = %v, want true", got)
	}
	if got := jsonbValue(t, e, `SELECT '1'::jsonb < '1.0000000000000000001'::jsonb`); got != true {
		t.Fatalf("1 < 1.0000000000000000001 = %v, want true", got)
	}
	if got := jsonbValue(t, e, `SELECT '1'::jsonb IS DISTINCT FROM '1.0000000000000000001'::jsonb`); got != true {
		t.Fatalf("1 IS DISTINCT FROM 1.0000000000000000001 = %v, want true", got)
	}
	if got := jsonbValue(t, e, `SELECT CASE '1'::jsonb WHEN '1.0000000000000000001'::jsonb THEN 'same' ELSE 'different' END`); got != "different" {
		t.Fatalf("simple CASE exact jsonb equality = %v, want different", got)
	}
	if got := jsonbValue(t, e, `SELECT '1'::jsonb IN ('1.0'::jsonb, '1.0000000000000000001'::jsonb)`); got != true {
		t.Fatalf("jsonb IN = %v, want true", got)
	}
	if got := jsonbValue(t, e, `SELECT '{"n":1}'::jsonb @> '{"n":1.0000000000000000001}'::jsonb`); got != false {
		t.Fatalf("containment rounded a number: %v, want false", got)
	}
	if got := jsonbValue(t, e, `SELECT '{"n":1}'::jsonb @> '{"n":1.0}'::jsonb`); got != true {
		t.Fatalf("containment of equal canonical numbers = %v, want true", got)
	}

	execMust(t, e, "CREATE TABLE exact (id INTEGER PRIMARY KEY, doc JSONB)")
	execMust(t, e, `INSERT INTO exact VALUES (1, '1'), (2, '1.0'), (3, '1.0000000000000000001'), (4, '9007199254740993'), (5, '9007199254740992')`)
	if res := execMust(t, e, `SELECT DISTINCT doc FROM exact`); res.RowCount != 4 {
		t.Fatalf("DISTINCT exact jsonb rows = %v, want 4", res.Rows)
	}
	res := execMust(t, e, `SELECT doc, COUNT(*) FROM exact GROUP BY doc ORDER BY doc`)
	if res.RowCount != 4 {
		t.Fatalf("GROUP BY exact jsonb rows = %v, want 4", res.Rows)
	}
	if res.Rows[0][1] != int64(2) {
		t.Fatalf("GROUP BY merged canonical numbers = %v, want two spellings of 1", res.Rows[0])
	}
	res = execMust(t, e, `SELECT COUNT(*) FROM exact WHERE doc = '1'::jsonb`)
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("equality count = %v, want 2", res.Rows[0][0])
	}
	res = execMust(t, e, `SELECT COUNT(*) FROM exact WHERE doc @> '1'::jsonb`)
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("containment count = %v, want 2", res.Rows[0][0])
	}
}

// A jsonb extraction result must not be silently compared with a SQL number.
func TestJSONBExtractionComparisonTypeError(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	for _, sql := range []string{
		`SELECT '{"a":1}'::jsonb -> 'a' = 1`,
		`SELECT '{"a":1}'::jsonb -> 'a' = TRUE`,
	} {
		if _, err := execSQL(e, sql); jsonbErrorCode(t, err) != pgerror.CodeDatatypeMismatch {
			t.Errorf("%s error = %v, want 42804", sql, err)
		}
	}
	if got := jsonbValue(t, e, `SELECT '{"a":1}'::jsonb -> 'a' = '1'::jsonb`); got != true {
		t.Fatalf("jsonb -> = jsonb = %v, want true", got)
	}
}

// PostgreSQL array containment is duplicate-insensitive: each candidate
// element only needs some matching container element, and duplicates may
// consume the same match.
func TestJSONBArrayContainmentDuplicates(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	cases := []struct {
		sql  string
		want interface{}
	}{
		{`SELECT '[1]'::jsonb @> '[1,1]'::jsonb`, true},
		{`SELECT '[1,1]'::jsonb @> '[1]'::jsonb`, true},
		{`SELECT '[1,2,2]'::jsonb @> '[2,2]'::jsonb`, true},
		{`SELECT '[1,2]'::jsonb @> '[2,2]'::jsonb`, true},
		{`SELECT '[1,2]'::jsonb @> '[2,3]'::jsonb`, false},
		{`SELECT '[1]'::jsonb @> '1'::jsonb`, true},
	}
	for _, tc := range cases {
		if got := jsonbValue(t, e, tc.sql); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.sql, got, tc.want)
		}
	}
}

// A JSONB column written before native JSONB may hold non-canonical text. Reads
// canonicalize it so projection, DISTINCT, GROUP BY, and ORDER BY all observe
// the canonical value.
func TestJSONBLegacyValuesCanonicalizeOnRead(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE legacy (id INTEGER PRIMARY KEY, doc JSONB)")

	legacy := []string{
		`{ "a" : 1 }`,
		`{"a":1}`,
		` { "a" :1 } `,
		`{"b": 2,"a": 1}`,
		`{"a":1,"b":2}`,
		` { "b" : 2, "a" : 1 } `,
	}
	for i, raw := range legacy {
		if err := table.Insert("legacy", storage.Row{"id": int64(i + 1), "doc": raw}); err != nil {
			t.Fatalf("legacy insert %d: %v", i, err)
		}
	}
	e = newExec(schema, table)

	res := execMust(t, e, `SELECT doc FROM legacy ORDER BY id`)
	if res.RowCount != len(legacy) {
		t.Fatalf("legacy rows = %d", res.RowCount)
	}
	for _, row := range res.Rows {
		if row[0] != `{"a":1}` && row[0] != `{"a":1,"b":2}` {
			t.Fatalf("legacy read = %v, want canonical text", row[0])
		}
	}
	res = execMust(t, e, `SELECT DISTINCT doc FROM legacy`)
	if res.RowCount != 2 {
		t.Fatalf("legacy DISTINCT rows = %v, want 2", res.Rows)
	}
	res = execMust(t, e, `SELECT DISTINCT * FROM legacy`)
	if res.RowCount != len(legacy) {
		t.Fatalf("legacy DISTINCT * rows = %d", res.RowCount)
	}
	res = execMust(t, e, `SELECT * FROM legacy ORDER BY id`)
	for _, row := range res.Rows {
		if row[1] != `{"a":1}` && row[1] != `{"a":1,"b":2}` {
			t.Fatalf("legacy SELECT * read = %v, want canonical text", row[1])
		}
	}
	if got := jsonbValue(t, e, `SELECT COUNT(DISTINCT doc) FROM legacy`); got != int64(2) {
		t.Fatalf("legacy COUNT(DISTINCT doc) = %v, want 2", got)
	}
	res = execMust(t, e, `SELECT doc, COUNT(*) FROM legacy GROUP BY doc`)
	if res.RowCount != 2 {
		t.Fatalf("legacy GROUP BY rows = %v, want 2", res.Rows)
	}
	res = execMust(t, e, `SELECT id FROM legacy ORDER BY doc, id`)
	if res.RowCount != len(legacy) {
		t.Fatalf("legacy ORDER BY rows = %d", res.RowCount)
	}
	if got := jsonbValue(t, e, `SELECT doc FROM legacy WHERE id = 1`); got != `{"a":1}` {
		t.Fatalf("legacy selected value = %v, want canonical", got)
	}
}

// Escapes that encoding/json would silently rewrite are invalid jsonb input.
func TestJSONBRejectsNULAndLoneSurrogates(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	for _, sql := range []string{
		`SELECT '{"a":"\u0000"}'::jsonb`,
		`SELECT '"\ud800"'::jsonb`,
		`SELECT '"\ud800x"'::jsonb`,
		`SELECT '"\udc00"'::jsonb`,
		`SELECT '{"\u0000":1}'::jsonb`,
	} {
		if _, err := execSQL(e, sql); jsonbErrorCode(t, err) != pgerror.CodeInvalidText {
			t.Errorf("%s error = %v, want 22P02", sql, err)
		}
	}
	if got := jsonbValue(t, e, `SELECT '"\u0041"'::jsonb`); got != `"A"` {
		t.Fatalf("escaped ASCII = %v", got)
	}
	if got := jsonbValue(t, e, `SELECT '"\ud83d\ude00"'::jsonb`); got != "\"\U0001F600\"" {
		t.Fatalf("surrogate pair = %v", got)
	}
}

// Zero-row text set-returning functions must still report TEXT for the value
// column.
func TestJSONBTextSetReturningTypesOnNull(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	res := execMust(t, e, `SELECT * FROM jsonb_each_text(NULL)`)
	if res.RowCount != 0 || len(res.ColumnTypes) != 2 || res.ColumnTypes[0] != "TEXT" || res.ColumnTypes[1] != "TEXT" {
		t.Fatalf("jsonb_each_text(NULL) = rows %d types %v, want 0 [TEXT TEXT]", res.RowCount, res.ColumnTypes)
	}
	res = execMust(t, e, `SELECT * FROM jsonb_array_elements_text(NULL)`)
	if res.RowCount != 0 || len(res.ColumnTypes) != 1 || res.ColumnTypes[0] != "TEXT" {
		t.Fatalf("jsonb_array_elements_text(NULL) = rows %d types %v, want 0 [TEXT]", res.RowCount, res.ColumnTypes)
	}
}
