package pgserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// preparePGXBinaryResults creates the table the binary-result tests scan.
func preparePGXBinaryResults(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE TABLE b (id INTEGER PRIMARY KEY, small SMALLINT, big BIGINT, flag BOOLEAN, ratio REAL, score DOUBLE,
			name TEXT, data BLOB, doc JSONB, tags TEXT[], nums INTEGER[], longs BIGINT[], flags BOOLEAN[], amounts NUMERIC[])`,
		`INSERT INTO b VALUES (1, 7, 9000000000, 1, 1.5, 2.25, 'alpha', X'00ff', '{"a": 1}',
			'{x,"y z"}', '{1,2}', '{9000000000}', '{t,f}', '{1.10,12345678901234567890}')`,
		`INSERT INTO b VALUES (2, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
			'{}', '{NULL,3}', '{NULL,-1}', '{f,NULL}', '{NULL,2}')`,
		`CREATE TABLE stamps (id INTEGER PRIMARY KEY, created_at TIMESTAMP)`,
		`INSERT INTO stamps VALUES (1, '2024-01-02 03:04:05')`,
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// pgx asks for per-column binary results with QueryResultFormats. The default
// QueryExecMode still prepares the statement and binds the requested formats,
// so this is the path a pgx application uses when it wants binary results.
func TestPGXBinaryResultScalars(t *testing.T) {
	conn := connectPGX(t)
	preparePGXBinaryResults(t, conn)
	ctx := context.Background()

	rows, err := conn.Query(ctx,
		`SELECT id, small, big, flag, ratio, score, name FROM b ORDER BY id`,
		pgx.QueryResultFormats{1})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for i, field := range rows.FieldDescriptions() {
		if field.Format != 1 {
			t.Fatalf("field %d format = %d, want binary", i, field.Format)
		}
	}
	var seen int
	for rows.Next() {
		var (
			id    int32
			small *int16
			big   *int64
			flag  *bool
			ratio *float32
			score *float64
			name  *string
		)
		if err := rows.Scan(&id, &small, &big, &flag, &ratio, &score, &name); err != nil {
			t.Fatalf("scan row %d: %v", seen, err)
		}
		switch seen {
		case 0:
			if id != 1 || small == nil || *small != 7 {
				t.Fatalf("row 1 id/small = %d/%v", id, small)
			}
			if big == nil || *big != 9000000000 {
				t.Fatalf("row 1 big = %v", big)
			}
			if flag == nil || !*flag || ratio == nil || *ratio != 1.5 || score == nil || *score != 2.25 {
				t.Fatalf("row 1 flag/ratio/score = %v/%v/%v", flag, ratio, score)
			}
			if name == nil || *name != "alpha" {
				t.Fatalf("row 1 name = %v", name)
			}
		case 1:
			if id != 2 {
				t.Fatalf("row 2 id = %d", id)
			}
			if small != nil || big != nil || flag != nil || ratio != nil || score != nil || name != nil {
				t.Fatalf("row 2 NULL columns = %v %v %v %v %v %v", small, big, flag, ratio, score, name)
			}
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != 2 {
		t.Fatalf("got %d rows, want 2", seen)
	}
}

func TestPGXBinaryResultByteaAndJSONB(t *testing.T) {
	conn := connectPGX(t)
	preparePGXBinaryResults(t, conn)
	ctx := context.Background()

	var data []byte
	var doc map[string]any
	if err := conn.QueryRow(ctx, `SELECT data, doc FROM b WHERE id = 1`, pgx.QueryResultFormats{1}).
		Scan(&data, &doc); err != nil {
		t.Fatalf("scan bytea/jsonb: %v", err)
	}
	if len(data) != 2 || data[0] != 0x00 || data[1] != 0xff {
		t.Fatalf("bytea = %v, want [0 255]", data)
	}
	if len(doc) != 1 || doc["a"] != float64(1) {
		t.Fatalf("jsonb = %v, want {\"a\": 1}", doc)
	}

	var raw json.RawMessage
	if err := conn.QueryRow(ctx, `SELECT doc FROM b WHERE id = 1`, pgx.QueryResultFormats{1}).
		Scan(&raw); err != nil {
		t.Fatalf("scan jsonb RawMessage: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("jsonb RawMessage %q: %v", raw, err)
	}
	if decoded["a"] != float64(1) {
		t.Fatalf("jsonb RawMessage = %q", raw)
	}
}

func TestPGXBinaryResultArrays(t *testing.T) {
	conn := connectPGX(t)
	preparePGXBinaryResults(t, conn)
	ctx := context.Background()

	var tags []string
	var nums []int32
	var longs []int64
	var flags []bool
	if err := conn.QueryRow(ctx,
		`SELECT tags, nums, longs, flags FROM b WHERE id = 1`, pgx.QueryResultFormats{1}).
		Scan(&tags, &nums, &longs, &flags); err != nil {
		t.Fatalf("scan arrays: %v", err)
	}
	if len(tags) != 2 || tags[0] != "x" || tags[1] != "y z" {
		t.Fatalf("tags = %q", tags)
	}
	if len(nums) != 2 || nums[0] != 1 || nums[1] != 2 {
		t.Fatalf("nums = %v", nums)
	}
	if len(longs) != 1 || longs[0] != 9000000000 {
		t.Fatalf("longs = %v", longs)
	}
	if len(flags) != 2 || !flags[0] || flags[1] {
		t.Fatalf("flags = %v", flags)
	}

	// An empty array round-trips as an empty, non-nil slice.
	var empty []string
	if err := conn.QueryRow(ctx, `SELECT tags FROM b WHERE id = 2`, pgx.QueryResultFormats{1}).
		Scan(&empty); err != nil {
		t.Fatalf("scan empty array: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty array = %v (nil=%v)", empty, empty == nil)
	}

	// NULL elements decode into pointer elements, distinct from zero values.
	var elements []*int32
	if err := conn.QueryRow(ctx, `SELECT nums FROM b WHERE id = 2`, pgx.QueryResultFormats{1}).
		Scan(&elements); err != nil {
		t.Fatalf("scan NULL array elements: %v", err)
	}
	if len(elements) != 2 || elements[0] != nil || elements[1] == nil || *elements[1] != 3 {
		t.Fatalf("NULL array elements = %v", elements)
	}

}

func TestPGXBinaryResultNumericArray(t *testing.T) {
	conn := connectPGX(t)
	preparePGXBinaryResults(t, conn)
	ctx := context.Background()

	var amounts []pgtype.Numeric
	if err := conn.QueryRow(ctx, `SELECT amounts FROM b WHERE id = 1`, pgx.QueryResultFormats{1}).
		Scan(&amounts); err != nil {
		t.Fatalf("scan numeric array: %v", err)
	}
	if len(amounts) != 2 {
		t.Fatalf("amounts = %v, want 2 values", amounts)
	}
	want := []string{"1.10", "12345678901234567890"}
	for i, amount := range amounts {
		value, err := amount.Value()
		if err != nil {
			t.Fatalf("numeric %d value: %v", i, err)
		}
		if value != want[i] {
			t.Fatalf("numeric %d = %v, want %q", i, value, want[i])
		}
	}

	// NULL numeric elements stay NULL.
	var second []pgtype.Numeric
	if err := conn.QueryRow(ctx, `SELECT amounts FROM b WHERE id = 2`, pgx.QueryResultFormats{1}).
		Scan(&second); err != nil {
		t.Fatalf("scan numeric array with NULL: %v", err)
	}
	if len(second) != 2 || second[0].Valid || !second[1].Valid {
		t.Fatalf("numeric array with NULL = %+v", second)
	}
}

// A per-column list mixes text and binary in one result, and pgx decodes each
// column with the format its RowDescription reports.
func TestPGXBinaryResultMixedFormats(t *testing.T) {
	conn := connectPGX(t)
	preparePGXBinaryResults(t, conn)
	ctx := context.Background()

	rows, err := conn.Query(ctx, `SELECT name, id, flag FROM b WHERE id = 1`,
		pgx.QueryResultFormats{0, 1, 0})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	formats := []int16{}
	for _, field := range rows.FieldDescriptions() {
		formats = append(formats, field.Format)
	}
	if !equalInt16(formats, []int16{0, 1, 0}) {
		t.Fatalf("formats = %v, want [0 1 0]", formats)
	}
	var name string
	var id int32
	var flag string
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	if err := rows.Scan(&name, &id, &flag); err != nil {
		t.Fatalf("scan mixed formats: %v", err)
	}
	if name != "alpha" || id != 1 || flag != "1" {
		t.Fatalf("mixed row = %q %d %q", name, id, flag)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

// A binary request for a type without a binary encoder reports SQLSTATE 0A000
// and the connection keeps working for the next query.
func TestPGXBinaryResultUnsupportedOID(t *testing.T) {
	conn := connectPGX(t)
	preparePGXBinaryResults(t, conn)
	ctx := context.Background()

	rows, err := conn.Query(ctx, `SELECT created_at FROM stamps`, pgx.QueryResultFormats{1})
	if err == nil {
		// Execute errors are delivered through the rows, not the Query call.
		rows.Close()
		err = rows.Err()
	}
	if err == nil {
		t.Fatal("binary timestamptz query succeeded, want 0A000")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != ErrCodeFeatureNotSupported {
		t.Fatalf("error = %v, want SQLSTATE %s", err, ErrCodeFeatureNotSupported)
	}

	var created string
	if err := conn.QueryRow(ctx, `SELECT created_at FROM stamps`).Scan(&created); err != nil {
		t.Fatalf("text query after 0A000: %v", err)
	}
	if created != "2024-01-02 03:04:05" {
		t.Fatalf("text timestamp = %q", created)
	}
}

// Without an explicit result format pgx keeps getting text results.
func TestPGXTextResultsUnchanged(t *testing.T) {
	conn := connectPGX(t)
	preparePGXBinaryResults(t, conn)
	ctx := context.Background()

	rows, err := conn.Query(ctx, `SELECT id, name FROM b WHERE id = 1`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for i, field := range rows.FieldDescriptions() {
		if field.Format != 0 {
			t.Fatalf("field %d format = %d, want text", i, field.Format)
		}
	}
	var id int32
	var name string
	if !rows.Next() {
		t.Fatal("no row")
	}
	if err := rows.Scan(&id, &name); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if id != 1 || name != "alpha" {
		t.Fatalf("text row = %d %q", id, name)
	}
}

// RETURNING rows honor the requested binary formats like any other result.
func TestPGXBinaryResultReturning(t *testing.T) {
	conn := connectPGX(t)
	preparePGXBinaryResults(t, conn)
	ctx := context.Background()

	var id int32
	if err := conn.QueryRow(ctx,
		`INSERT INTO stamps (id, created_at) VALUES (2, '2024-02-03 04:05:06') RETURNING id`,
		pgx.QueryResultFormats{1}).Scan(&id); err != nil {
		t.Fatalf("binary RETURNING: %v", err)
	}
	if id != 2 {
		t.Fatalf("RETURNING id = %d, want 2", id)
	}
}

// Binary results coexist with the parameter path from #40: a bound parameter is
// decoded to its text literal and the projected value is returned in binary.
func TestPGXBinaryResultWithParameters(t *testing.T) {
	conn := connectPGX(t)
	preparePGXBinaryResults(t, conn)
	ctx := context.Background()

	var id int32
	var name string
	if err := conn.QueryRow(ctx,
		`SELECT id, name FROM b WHERE id = $1`, pgx.QueryResultFormats{1}, int32(1)).
		Scan(&id, &name); err != nil {
		t.Fatalf("binary query with parameter: %v", err)
	}
	if id != 1 || name != "alpha" {
		t.Fatalf("row = %d %q", id, name)
	}

	var doubled int64
	if err := conn.QueryRow(ctx,
		`SELECT $1::BIGINT * 2`, pgx.QueryResultFormats{1}, int64(21)).Scan(&doubled); err != nil {
		t.Fatalf("binary result of a parameter expression: %v", err)
	}
	if doubled != 42 {
		t.Fatalf("doubled = %d, want 42", doubled)
	}
}
