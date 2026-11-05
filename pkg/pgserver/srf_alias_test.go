package pgserver

import "testing"

// PostgreSQL's `AS alias(col, ...)` list renames a set-returning function's
// output columns in the projection and in WHERE.
func TestSetReturningFunctionColumnAliases(t *testing.T) {
	c, conn := arrayConnection(t)

	conn.reset()
	messages := runSimple(t, c, conn, `SELECT unnest FROM unnest(ARRAY[1,2,3]) AS t(unnest)`)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("unnest alias list failed: %s", errorCode(errMsg))
	}
	columns := rowDescriptionColumns(t, findRowDescription(t, messages).Data)
	if len(columns) != 1 || columns[0] != "unnest" {
		t.Fatalf("unnest alias columns = %v, want [unnest]", columns)
	}
	if rows := countMessageType(messages, MsgDataRow); rows != 3 {
		t.Fatalf("unnest alias rows = %d, want 3", rows)
	}

	conn.reset()
	messages = runSimple(t, c, conn, `SELECT n FROM unnest(ARRAY[10,20,30]) AS t(n) WHERE n > 10 ORDER BY n`)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("unnest renamed column failed: %s", errorCode(errMsg))
	}
	if columns := rowDescriptionColumns(t, findRowDescription(t, messages).Data); len(columns) != 1 || columns[0] != "n" {
		t.Fatalf("unnest renamed columns = %v, want [n]", columns)
	}
	if row := firstDataRow(t, messages); len(row) != 1 || row[0] != "20" {
		t.Fatalf("unnest renamed first row = %v, want [20]", row)
	}

	conn.reset()
	messages = runSimple(t, c, conn, `SELECT k, v FROM jsonb_each('{"b":2,"a":1}') AS e(k, v) ORDER BY k`)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("jsonb_each renamed columns failed: %s", errorCode(errMsg))
	}
	if columns := rowDescriptionColumns(t, findRowDescription(t, messages).Data); len(columns) != 2 || columns[0] != "k" || columns[1] != "v" {
		t.Fatalf("jsonb_each renamed columns = %v, want [k v]", columns)
	}
	if row := firstDataRow(t, messages); len(row) != 2 || row[0] != "a" || row[1] != "1" {
		t.Fatalf("jsonb_each renamed first row = %v, want [a 1]", row)
	}

	// An alias without a column list keeps the default names.
	conn.reset()
	messages = runSimple(t, c, conn, `SELECT key, value FROM jsonb_each('{"a":1}') AS e`)
	if errMsg := findError(messages); errMsg != nil {
		t.Fatalf("alias-only jsonb_each failed: %s", errorCode(errMsg))
	}
	if columns := rowDescriptionColumns(t, findRowDescription(t, messages).Data); len(columns) != 2 || columns[0] != "key" || columns[1] != "value" {
		t.Fatalf("alias-only columns = %v, want [key value]", columns)
	}
}

// An alias list with the wrong arity reports invalid_column_reference (42P10).
func TestSetReturningFunctionColumnAliasArity(t *testing.T) {
	c, conn := arrayConnection(t)
	for _, sql := range []string{
		`SELECT * FROM unnest(ARRAY[1]) AS t(a, b)`,
		`SELECT * FROM jsonb_each('{"a":1}') AS e(k)`,
	} {
		conn.reset()
		messages := runSimple(t, c, conn, sql)
		errMsg := findError(messages)
		if errMsg == nil || errorCode(errMsg) != ErrCodeInvalidColumnReference {
			t.Fatalf("%s error = %v, want 42P10", sql, messages)
		}
	}
	conn.reset()
	messages := runExtendedParseBindExecute(t, c, conn, `SELECT * FROM unnest($1) AS t(a, b)`, 1007, `{1}`)
	if errMsg := findError(messages); errMsg == nil || errorCode(errMsg) != ErrCodeInvalidColumnReference {
		t.Fatalf("extended alias arity error = %v, want 42P10", messages)
	}
}
