package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

func TestInsertReturningGeneratedIDAndProjections(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL)")

	res := execMust(t, e, "INSERT INTO users (name) VALUES ('alice') RETURNING id, name, id + 1 AS next_id")
	if len(res.Columns) != 3 {
		t.Fatalf("expected 3 columns, got %v", res.Columns)
	}
	if res.Columns[0] != "id" || res.Columns[1] != "name" || res.Columns[2] != "next_id" {
		t.Fatalf("unexpected columns %v", res.Columns)
	}
	if res.RowCount != 1 {
		t.Fatalf("expected 1 row, got %d", res.RowCount)
	}
	if res.Rows[0][0] != int64(1) || res.Rows[0][1] != "alice" || res.Rows[0][2] != int64(2) {
		t.Fatalf("unexpected row %v", res.Rows[0])
	}
	if res.LastInsertID != 1 {
		t.Fatalf("expected LastInsertID 1, got %d", res.LastInsertID)
	}
}

func TestInsertReturningStar(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	res := execMust(t, e, "INSERT INTO t (v) VALUES ('x') RETURNING *")
	if res.RowCount != 1 || len(res.Columns) != 2 {
		t.Fatalf("unexpected result %v %v", res.Columns, res.Rows)
	}
	if res.Rows[0][1] != "x" {
		t.Fatalf("unexpected row %v", res.Rows[0])
	}
}

func TestUpdateReturning(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (1, 'a'), (2, 'b')")
	res := execMust(t, e, "UPDATE t SET v = upper(v) RETURNING id, v")
	if res.RowCount != 2 {
		t.Fatalf("expected 2 rows, got %d", res.RowCount)
	}
	got := map[interface{}]interface{}{}
	for _, row := range res.Rows {
		got[row[0]] = row[1]
	}
	if got[int64(1)] != "A" || got[int64(2)] != "B" {
		t.Fatalf("unexpected returning rows %v", res.Rows)
	}
}

func TestDeleteReturning(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (1, 'a'), (2, 'b')")
	res := execMust(t, e, "DELETE FROM t WHERE id = 1 RETURNING id, v")
	if res.RowCount != 1 || res.Rows[0][0] != int64(1) || res.Rows[0][1] != "a" {
		t.Fatalf("unexpected returning rows %v", res.Rows)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("expected RowsAffected 1, got %d", res.RowsAffected)
	}
}

func TestSQLiteVersionDistinctFromPizzasqlVersion(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	sqlite := execMust(t, e, "SELECT sqlite_version()")
	psql := execMust(t, e, "SELECT pizzasql_version()")
	if sqlite.Rows[0][0] != SQLiteCompatVersion {
		t.Fatalf("sqlite_version = %v, want %s", sqlite.Rows[0][0], SQLiteCompatVersion)
	}
	if SQLiteCompatVersion < "3.35.0" {
		t.Fatalf("SQLite compatibility floor must be >= 3.35.0, got %s", SQLiteCompatVersion)
	}
	if sqlite.Rows[0][0] == psql.Rows[0][0] {
		t.Fatalf("sqlite_version and pizzasql_version must differ")
	}
}

func TestGeneratedStoredColumnRecompute(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (a INTEGER, b INTEGER, total INTEGER GENERATED ALWAYS AS (a + b) STORED)")
	execMust(t, e, "INSERT INTO t (a, b) VALUES (2, 3)")
	res := execMust(t, e, "SELECT total FROM t")
	if res.Rows[0][0] != int64(5) {
		t.Fatalf("generated total = %v, want 5", res.Rows[0][0])
	}
	execMust(t, e, "UPDATE t SET a = 10")
	res = execMust(t, e, "SELECT total FROM t")
	if res.Rows[0][0] != int64(13) {
		t.Fatalf("generated total after update = %v, want 13", res.Rows[0][0])
	}
}

func TestGeneratedColumnReferencesAutoIncrementID(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, stored INTEGER GENERATED ALWAYS AS (id + 1) STORED)")
	res := execMust(t, e, "INSERT INTO t (id) VALUES (NULL) RETURNING id, stored")
	if res.Rows[0][0] != int64(1) || res.Rows[0][1] != int64(2) {
		t.Fatalf("generated id reference = %v, want [1 2]", res.Rows[0])
	}
	res = execMust(t, e, "SELECT stored FROM t WHERE id = 1")
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("persisted generated value = %v, want 2", res.Rows[0][0])
	}
}

func TestGeneratedColumnRejectsUserWrites(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (a INTEGER, b INTEGER GENERATED ALWAYS AS (a + 1) STORED)")

	if _, err := execSQL(e, "INSERT INTO t (a, b) VALUES (1, 99)"); err == nil {
		t.Fatal("expected explicit insert into generated column to fail")
	}
	if _, err := execSQL(e, "UPDATE t SET b = 5"); err == nil {
		t.Fatal("expected update of generated column to fail")
	}
}

func TestInsertOrReplacePrimaryKey(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execMust(t, e, "INSERT INTO t VALUES (1, 'a')")
	execMust(t, e, "INSERT OR REPLACE INTO t VALUES (1, 'b')")
	res := execMust(t, e, "SELECT v FROM t")
	if res.RowCount != 1 || res.Rows[0][0] != "b" {
		t.Fatalf("INSERT OR REPLACE result = %v", res.Rows)
	}
}

func TestTableUniqueOnConflictReplace(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE t (
		site_id INTEGER,
		path TEXT,
		total INTEGER,
		CONSTRAINT "t#site#path" UNIQUE(site_id, path) ON CONFLICT REPLACE
	)`)
	execMust(t, e, "INSERT INTO t (site_id, path, total) VALUES (1, '/a', 10)")
	execMust(t, e, "INSERT INTO t (site_id, path, total) VALUES (1, '/a', 42)")

	res := execMust(t, e, "SELECT total FROM t WHERE site_id = 1 AND path = '/a'")
	if res.RowCount != 1 {
		t.Fatalf("expected 1 row after replace, got %d", res.RowCount)
	}
	if res.Rows[0][0] != int64(42) {
		t.Fatalf("expected replaced total 42, got %v", res.Rows[0][0])
	}
}

func TestInsertIgnoreUniqueIndex(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE)")
	execMust(t, e, "INSERT INTO users VALUES (1, 'same@example.com')")

	res := execMust(t, e, "INSERT OR IGNORE INTO users VALUES (2, 'same@example.com')")
	if res.RowsAffected != 0 {
		t.Fatalf("INSERT OR IGNORE affected %d rows, want 0", res.RowsAffected)
	}
	res = execMust(t, e, "INSERT INTO users VALUES (3, 'same@example.com') ON CONFLICT DO NOTHING")
	if res.RowsAffected != 0 {
		t.Fatalf("targetless DO NOTHING affected %d rows, want 0", res.RowsAffected)
	}
	res = execMust(t, e, "SELECT id FROM users")
	if res.RowCount != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("users = %v, want only id 1", res.Rows)
	}

	execMust(t, e, "INSERT INTO users VALUES (4, NULL)")
	execMust(t, e, "INSERT OR IGNORE INTO users VALUES (5, NULL)")
	execMust(t, e, "INSERT INTO users VALUES (6, NULL) ON CONFLICT DO NOTHING")
	res = execMust(t, e, "SELECT count(*) FROM users WHERE email IS NULL")
	if res.Rows[0][0] != int64(3) {
		t.Fatalf("NULL unique values = %v, want 3 rows", res.Rows[0][0])
	}
}

func TestJSON1Functions(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	res := execMust(t, e, `SELECT json_extract('{"a": 1, "b": [10, 20]}', '$.b[1]')`)
	if res.Rows[0][0] != int64(20) {
		t.Fatalf("json_extract = %v, want 20", res.Rows[0][0])
	}

	res = execMust(t, e, `SELECT json_set('{"a": 1}', '$.a', 2)`)
	if res.Rows[0][0] != `{"a":2}` {
		t.Fatalf("json_set = %v", res.Rows[0][0])
	}

	// The JSON subtype must survive nested calls, matching GoatCounter's
	// json_insert(json_extract(...), '$[#]', json(...)) pattern.
	res = execMust(t, e, `SELECT json_insert(json_extract('{"w":[]}', '$.w'), '$[#]', json('{"n":"languages"}'))`)
	if res.Rows[0][0] != `[{"n":"languages"}]` {
		t.Fatalf("json_insert with json() = %v", res.Rows[0][0])
	}

	res = execMust(t, e, `SELECT json_replace('{"collect": 1}', '$.collect', json_extract('{"collect": 1}', '$.collect') | 64)`)
	if res.Rows[0][0] != `{"collect":65}` {
		t.Fatalf("json_replace with bitwise = %v", res.Rows[0][0])
	}

	res = execMust(t, e, `SELECT json_group_array(x) FROM (SELECT 1 AS x UNION ALL SELECT 2 UNION ALL SELECT 3) AS t`)
	if res.Rows[0][0] != `[1,2,3]` {
		t.Fatalf("json_group_array = %v", res.Rows[0][0])
	}
}

func TestBitwiseOperators(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	cases := map[string]int64{
		"SELECT 6 & 3":     2,
		"SELECT 6 | 1":     7,
		"SELECT 1 << 4":    16,
		"SELECT 32 >> 2":   8,
		"SELECT ~0":        -1,
		"SELECT 1 + 2 | 4": 7, // (1+2)|4
		"SELECT 2 | 1 * 8": 10,
	}
	for sql, want := range cases {
		res := execMust(t, e, sql)
		if res.Rows[0][0] != want {
			t.Errorf("%s = %v, want %d", sql, res.Rows[0][0], want)
		}
	}
}

func TestPercentDiff(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	res := execMust(t, e, "SELECT percent_diff(10, 15)")
	if res.Rows[0][0] != float64(50) {
		t.Fatalf("percent_diff(10,15) = %v, want 50", res.Rows[0][0])
	}
	res = execMust(t, e, "SELECT percent_diff(0, 5)")
	if f, ok := res.Rows[0][0].(float64); !ok || f <= 0 {
		t.Fatalf("percent_diff(0,5) should be +Inf, got %v", res.Rows[0][0])
	}
	res = execMust(t, e, "SELECT percent_diff(NULL, 5)")
	if res.Rows[0][0] != nil {
		t.Fatalf("percent_diff(NULL,5) should be NULL, got %v", res.Rows[0][0])
	}
}

func TestBlobLiteralStorageAndFunctions(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE blobs (id INTEGER PRIMARY KEY, data BLOB)")
	execMust(t, e, "INSERT INTO blobs (id, data) VALUES (1, X'00FF10')")

	res := execMust(t, e, "SELECT data FROM blobs WHERE id = 1")
	b, ok := res.Rows[0][0].([]byte)
	if !ok || len(b) != 3 || b[0] != 0x00 || b[1] != 0xFF || b[2] != 0x10 {
		t.Fatalf("blob round-trip = %#v", res.Rows[0][0])
	}

	res = execMust(t, e, "SELECT hex(data), typeof(data) FROM blobs WHERE id = 1")
	if res.Rows[0][0] != "00FF10" {
		t.Fatalf("hex = %v", res.Rows[0][0])
	}
	if res.Rows[0][1] != "blob" {
		t.Fatalf("typeof = %v", res.Rows[0][1])
	}

	res = execMust(t, e, "SELECT unhex('00FF')")
	if b, ok := res.Rows[0][0].([]byte); !ok || len(b) != 2 || b[1] != 0xFF {
		t.Fatalf("unhex = %#v", res.Rows[0][0])
	}

	res = execMust(t, e, "SELECT CAST('abc' AS BLOB)")
	if b, ok := res.Rows[0][0].([]byte); !ok || string(b) != "abc" {
		t.Fatalf("cast to blob = %#v", res.Rows[0][0])
	}
}

func TestExpressionUniqueIndexLower(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX users_email_lower ON users (lower(email))")
	execMust(t, e, "INSERT INTO users (id, email) VALUES (1, 'Alice@Example.com')")

	if _, err := execSQL(e, "INSERT INTO users (id, email) VALUES (2, 'alice@example.com')"); err == nil {
		t.Fatal("expected expression unique index to reject a case-insensitive duplicate")
	}
	// A NULL or distinct value is still allowed.
	execMust(t, e, "INSERT INTO users (id, email) VALUES (3, 'bob@example.com')")
}

func TestExpressionUniqueIndexReplace(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, tag TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX users_email_lower ON users (lower(email))")
	execMust(t, e, "INSERT INTO users (id, email, tag) VALUES (1, 'Alice@Example.com', 'old')")
	// INSERT OR REPLACE must replace the conflicting row even though the
	// conflict is on a case-insensitive expression index.
	execMust(t, e, "INSERT OR REPLACE INTO users (id, email, tag) VALUES (2, 'alice@example.com', 'new')")

	res := execMust(t, e, "SELECT id, tag FROM users")
	if res.RowCount != 1 {
		t.Fatalf("expected 1 row after expression replace, got %d: %v", res.RowCount, res.Rows)
	}
	if res.Rows[0][0] != int64(2) || res.Rows[0][1] != "new" {
		t.Fatalf("unexpected replaced row %v", res.Rows[0])
	}
}

func TestInsertReturningMetadataTypes(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)")
	res := execMust(t, e, "INSERT INTO t (id, name) VALUES (7, 'x') RETURNING id, name")
	if len(res.ColumnTypes) != 2 || res.ColumnTypes[0] != "BIGINT" || res.ColumnTypes[1] != "TEXT" {
		t.Fatalf("unexpected returning column types %v", res.ColumnTypes)
	}
}

func TestGeneratedColumnReturning(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (a INTEGER, b INTEGER GENERATED ALWAYS AS (a * 2) STORED)")
	res := execMust(t, e, "INSERT INTO t (a) VALUES (21) RETURNING a, b")
	if res.Rows[0][1] != int64(42) {
		t.Fatalf("expected generated b=42 in RETURNING, got %v", res.Rows[0][1])
	}
}

func TestGeneratedExpressionIndexText(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE users (email TEXT)")
	execMust(t, e, "CREATE UNIQUE INDEX users_email_lower ON users (lower(email))")
	idx, err := schema.GetIndex("users_email_lower")
	if err != nil {
		t.Fatalf("GetIndex: %v", err)
	}
	if len(idx.Columns) != 1 || idx.Columns[0].Expression == "" {
		t.Fatalf("expected persisted expression, got %#v", idx.Columns)
	}
	if !strings.Contains(strings.ToLower(idx.Columns[0].Expression), "lower(") {
		t.Fatalf("unexpected expression text %q", idx.Columns[0].Expression)
	}
}

func TestJoinUsingMultipleColumns(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE counts (site_id INTEGER, path_id INTEGER, total INTEGER)")
	execMust(t, e, "CREATE TABLE paths (site_id INTEGER, path_id INTEGER, path TEXT)")
	execMust(t, e, "INSERT INTO counts VALUES (1, 1, 4), (1, 2, 8), (2, 1, 16)")
	execMust(t, e, "INSERT INTO paths VALUES (1, 1, '/one'), (1, 2, '/two'), (2, 2, '/other')")

	res := execMust(t, e, "SELECT paths.path, counts.total FROM counts JOIN paths USING (site_id, path_id) ORDER BY counts.total")
	if res.RowCount != 2 || res.Rows[0][0] != "/one" || res.Rows[1][0] != "/two" {
		t.Fatalf("JOIN USING rows = %v", res.Rows)
	}
}

func TestJoinUsingInCommaFromList(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE base (id INTEGER)")
	execMust(t, e, "CREATE TABLE left_rows (id INTEGER)")
	execMust(t, e, "CREATE TABLE right_rows (id INTEGER)")
	execMust(t, e, "INSERT INTO base VALUES (1)")
	execMust(t, e, "INSERT INTO left_rows VALUES (1)")
	execMust(t, e, "INSERT INTO right_rows VALUES (1), (2)")

	res := execMust(t, e, "SELECT left_rows.id FROM base, left_rows JOIN right_rows USING (id)")
	if res.RowCount != 1 {
		t.Fatalf("mixed JOIN USING returned %d rows, want 1: %v", res.RowCount, res.Rows)
	}
}

func TestSQLiteDynamicTypingAssignments(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE settings (value VARCHAR)")
	execMust(t, e, "INSERT INTO settings VALUES (2)")
	execMust(t, e, "UPDATE settings SET value = X'0102'")
	res := execMust(t, e, "SELECT typeof(value), hex(value) FROM settings")
	if res.Rows[0][0] != "blob" || res.Rows[0][1] != "0102" {
		t.Fatalf("dynamic value = %v", res.Rows[0])
	}
}

func TestRenameTableAboveSingleBatchLimit(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE old_rows (id INTEGER PRIMARY KEY, value TEXT)")

	// A rename writes two KV operations per row; 32,768 rows exceed PizzaKV's
	// 65,535-operation batch limit.
	rows := make([]storage.Row, 32768)
	for i := range rows {
		rows[i] = storage.Row{"id": int64(i + 1), "value": fmt.Sprintf("v%d", i+1)}
	}
	if _, err := table.InsertBulk("old_rows", rows); err != nil {
		t.Fatalf("insert rows: %v", err)
	}
	execMust(t, e, "ALTER TABLE old_rows RENAME TO new_rows")
	res := execMust(t, e, "SELECT count(*) FROM new_rows")
	if res.Rows[0][0] != int64(len(rows)) {
		t.Fatalf("renamed table has %v rows, want %d", res.Rows[0][0], len(rows))
	}
}
