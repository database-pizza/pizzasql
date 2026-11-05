package executor

import (
	"strings"
	"testing"
)

func TestJSONArrowObjectAndArrayAccess(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	tests := []struct {
		name string
		sql  string
		want interface{}
	}{
		{"object member as JSON text", `SELECT '{"a":1,"s":"x"}' -> 'a'`, "1"},
		{"string member keeps JSON quotes", `SELECT '{"a":1,"s":"x"}' -> 's'`, `"x"`},
		{"string member as SQL text", `SELECT '{"a":1,"s":"x"}' ->> 's'`, "x"},
		{"object member as JSON text", `SELECT '{"b":[10,20]}' -> 'b'`, "[10,20]"},
		{"array element as JSON text", `SELECT '[10,20,30]' -> 1`, "20"},
		{"array element as SQL number", `SELECT '[10,20,30]' ->> 1`, int64(20)},
		{"array element as JSON text", `SELECT '["a","b"]' -> 0`, `"a"`},
		{"negative array index", `SELECT '[10,20,30]' ->> -1`, int64(30)},
		{"nested chaining", `SELECT '{"a":{"b":[{"c":7}]}}' -> 'a' -> 'b' -> 0 ->> 'c'`, int64(7)},
		{"numeric key selects an array index", `SELECT '[10,20]' -> 1`, "20"},
		{"string key names an object member", `SELECT '{"1":"one"}' -> '1'`, `"one"`},
		{"numeric key on an object is missing", `SELECT '{"1":"one"}' -> 1`, nil},
		{"json null as JSON text", `SELECT '{"a":null}' -> 'a'`, "null"},
		{"json null as SQL NULL", `SELECT '{"a":null}' ->> 'a'`, nil},
		{"json true as JSON text", `SELECT '{"a":true}' -> 'a'`, "true"},
		{"json true as SQL integer", `SELECT '{"a":true}' ->> 'a'`, int64(1)},
		{"json false as SQL integer", `SELECT '{"a":false}' ->> 'a'`, int64(0)},
		{"path-style key", `SELECT '{"a":{"b":2}}' -> '$.a' ->> 'b'`, int64(2)},
		{"whole document path", `SELECT '{"a":1}' ->> '$'`, `{"a":1}`},
		{"missing object key", `SELECT '{"a":1}' -> 'b'`, nil},
		{"missing object key as SQL", `SELECT '{"a":1}' ->> 'b'`, nil},
		{"array index past the end", `SELECT '[1,2]' -> 5`, nil},
		{"NULL document", `SELECT NULL -> 'a'`, nil},
		{"NULL document as SQL", `SELECT NULL ->> 'a'`, nil},
		{"NULL key", `SELECT '{"a":1}' -> NULL`, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := execMust(t, e, tt.sql)
			got := res.Rows[0][0]
			if got != tt.want {
				t.Fatalf("%s = %v (%T), want %v (%T)", tt.sql, got, got, tt.want, tt.want)
			}
		})
	}
}

func TestJSONArrowMalformedInputIsAnError(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	for _, sql := range []string{
		`SELECT 'not json' -> 'a'`,
		`SELECT 'not json' ->> 'a'`,
		`SELECT '{"a":1}' -> '$.['`,
	} {
		if _, err := execSQL(e, sql); err == nil {
			t.Fatalf("%s: expected an error", sql)
		} else if !strings.Contains(err.Error(), "JSON") {
			t.Fatalf("%s: unexpected error: %v", sql, err)
		}
	}
}

func TestJSONArrowInWhereOrderByAndUpdate(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE docs (id INTEGER PRIMARY KEY, doc TEXT, v TEXT)")
	execMust(t, e, `INSERT INTO docs VALUES (1, '{"kind":"a","n":1}', NULL)`)
	execMust(t, e, `INSERT INTO docs VALUES (2, '{"kind":"b","n":2}', NULL)`)
	execMust(t, e, `INSERT INTO docs VALUES (3, '{"kind":"c","n":3}', NULL)`)

	res := execMust(t, e, `SELECT id FROM docs WHERE doc ->> 'kind' = 'b'`)
	if res.RowCount != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("WHERE doc ->> 'kind' = 'b' rows = %v, want id 2", res.Rows)
	}

	res = execMust(t, e, `SELECT doc ->> 'kind' AS kind FROM docs ORDER BY doc -> 'n' DESC`)
	if res.RowCount != 3 {
		t.Fatalf("ORDER BY rows = %d, want 3", res.RowCount)
	}
	if res.Rows[0][0] != "c" || res.Rows[1][0] != "b" || res.Rows[2][0] != "a" {
		t.Fatalf("ORDER BY doc -> 'n' DESC = %v, want c, b, a", res.Rows)
	}

	execMust(t, e, `UPDATE docs SET v = doc ->> 'kind'`)
	res = execMust(t, e, `SELECT id, v FROM docs ORDER BY id`)
	for i, want := range []string{"a", "b", "c"} {
		if res.Rows[i][1] != want {
			t.Fatalf("row %d v = %v, want %q", i, res.Rows[i][1], want)
		}
	}
}

// The -> operator returns JSON text (with the engine's internal JSON subtype),
// not a plain string. Numeric, boolean, and comparison coercions must still
// read the text form, exactly as they do for SQL TEXT; otherwise arithmetic
// silently treats every extracted number as 0.
func TestJSONArrowOperatorValueCoercions(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE metrics (id INTEGER PRIMARY KEY, doc TEXT)")
	execMust(t, e, `INSERT INTO metrics VALUES (1, '{"n":2,"big":10,"flag":1}')`)
	execMust(t, e, `INSERT INTO metrics VALUES (2, '{"n":10,"big":20,"flag":0}')`)

	// Arithmetic uses the text numeric value. Like '2' + 1 elsewhere in the
	// engine, the result is REAL rather than INTEGER for JSON text.
	res := execMust(t, e, `SELECT (doc -> 'n') + 1 FROM metrics WHERE id = 1`)
	if got := res.Rows[0][0]; got != float64(3) {
		t.Fatalf("(doc -> 'n') + 1 = %v (%T), want 3", got, got)
	}

	// Bitwise operators coerce through toInt64.
	res = execMust(t, e, `SELECT (doc -> 'n') & 3 FROM metrics WHERE id = 1`)
	if got := res.Rows[0][0]; got != int64(2) {
		t.Fatalf("(doc -> 'n') & 3 = %v (%T), want int64 2", got, got)
	}

	// Numeric comparison must not fall back to string comparison, where
	// "10" < "9" and multi-digit values order incorrectly.
	res = execMust(t, e, `SELECT id FROM metrics WHERE doc -> 'big' > 9 ORDER BY id`)
	if res.RowCount != 2 || res.Rows[0][0] != int64(1) || res.Rows[1][0] != int64(2) {
		t.Fatalf("WHERE doc -> 'big' > 9 rows = %v, want ids 1, 2", res.Rows)
	}
	res = execMust(t, e, `SELECT id FROM metrics WHERE doc -> 'big' = 10`)
	if res.RowCount != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("WHERE doc -> 'big' = 10 rows = %v, want id 1", res.Rows)
	}

	// CAST goes through toFloat.
	res = execMust(t, e, `SELECT CAST(doc -> 'big' AS INTEGER) FROM metrics WHERE id = 1`)
	if got := res.Rows[0][0]; got != int64(10) {
		t.Fatalf("CAST(doc -> 'big' AS INTEGER) = %v (%T), want int64 10", got, got)
	}

	// WHERE truthiness goes through toBool.
	res = execMust(t, e, `SELECT id FROM metrics WHERE doc -> 'flag'`)
	if res.RowCount != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("WHERE doc -> 'flag' rows = %v, want id 1", res.Rows)
	}

	// ORDER BY compares the extracted values numerically.
	res = execMust(t, e, `SELECT id FROM metrics ORDER BY doc -> 'n'`)
	if res.RowCount != 2 || res.Rows[0][0] != int64(1) || res.Rows[1][0] != int64(2) {
		t.Fatalf("ORDER BY doc -> 'n' rows = %v, want ids 1, 2", res.Rows)
	}

	// The JSON subtype must survive -> so JSON functions embed the extracted
	// value as JSON rather than as a quoted string.
	res = execMust(t, e, `SELECT json_array(doc -> 'n') FROM metrics WHERE id = 1`)
	if got := res.Rows[0][0]; got != "[2]" {
		t.Fatalf("json_array(doc -> 'n') = %v, want [2]", got)
	}
}
