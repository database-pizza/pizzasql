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
