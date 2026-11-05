package pgserver

import "testing"

// Array @> and <@ compare one-dimensional arrays element-wise over the wire
// instead of falling into the jsonb path.
func TestArrayContainmentOverTheWire(t *testing.T) {
	c, conn := arrayConnection(t)

	conn.reset()
	messages := runSimple(t, c, conn, `SELECT ARRAY[1,2,3] @> ARRAY[1], ARRAY[1] <@ ARRAY[1,2], ARRAY[1] @> ARRAY[2], NULL::INTEGER[] @> ARRAY[1], ARRAY[1] @> ARRAY[]::INTEGER[]`)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("array containment failed: %s %s", errorCode(errMsg), string(errMsg.Data))
	}
	row := firstDataRow(t, messages)
	want := []string{"true", "true", "false", "<null>", "true"}
	if len(row) != len(want) {
		t.Fatalf("array containment row = %v, want %v", row, want)
	}
	for i := range want {
		if row[i] != want[i] {
			t.Fatalf("array containment row = %v, want %v", row, want)
		}
	}

	conn.reset()
	if errMsg := findError(runSimple(t, c, conn, `CREATE TABLE items (id INTEGER PRIMARY KEY, nums INTEGER[])`)); errMsg != nil {
		t.Fatalf("create items: %s", errorCode(errMsg))
	}
	conn.reset()
	if errMsg := findError(runSimple(t, c, conn, `INSERT INTO items VALUES (1, '{1,2,3}'), (2, '{4}'), (3, NULL)`)); errMsg != nil {
		t.Fatalf("insert items: %s", errorCode(errMsg))
	}
	conn.reset()
	messages = runSimple(t, c, conn, `SELECT id FROM items WHERE nums @> ARRAY[2] AND nums <@ ARRAY[1,2,3]`)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("filtered containment: %s", errorCode(errMsg))
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "1" {
		t.Fatalf("filtered containment row = %v, want [1]", row)
	}

	// A scalar operand is still a type mismatch, not an empty result.
	conn.reset()
	messages = runSimple(t, c, conn, `SELECT ARRAY[1] @> 1`)
	if errMsg := findError(messages); errMsg == nil || errorCode(errMsg) != ErrCodeDatatypeMismatch {
		t.Fatalf("scalar containment error = %v, want 42804", messages)
	}
}

// A parameter on the right of an array containment operator is inferred as the
// column's array type, so the extended protocol binds an array rather than text.
func TestArrayContainmentParameterBinding(t *testing.T) {
	c, conn := arrayConnection(t)
	cases := []struct {
		query string
		oid   int32
		value string
	}{
		{`SELECT id FROM items WHERE nums @> $1`, 1007, `{2}`},
		{`SELECT id FROM items WHERE $1 <@ nums`, 1007, `{1,2}`},
	}
	for _, tc := range cases {
		conn.reset()
		if errMsg := findError(runSimple(t, c, conn, `CREATE TABLE items (id INTEGER PRIMARY KEY, nums INTEGER[])`)); errMsg != nil {
			if errorCode(errMsg) != "42P07" {
				t.Fatalf("create items: %s", errorCode(errMsg))
			}
		}
		conn.reset()
		if errMsg := findError(runSimple(t, c, conn, `INSERT INTO items VALUES (1, '{1,2,3}'), (2, '{4}')`)); errMsg != nil {
			t.Fatalf("insert items: %s", errorCode(errMsg))
		}

		conn.reset()
		messages := runExtendedParseBindExecute(t, c, conn, tc.query, tc.oid, tc.value)
		if errMsg := findError(messages); errMsg != nil {
			t.Fatalf("%s: %s", tc.query, errorCode(errMsg))
		}
		if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "1" {
			t.Fatalf("%s row = %v, want [1]", tc.query, row)
		}

		conn.reset()
		if errMsg := findError(runSimple(t, c, conn, `DROP TABLE items`)); errMsg != nil {
			t.Fatalf("drop items: %s", errorCode(errMsg))
		}
	}
}
