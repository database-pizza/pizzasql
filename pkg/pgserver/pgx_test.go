package pgserver

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

// connectPGX starts a wire server over net.Pipe and connects pgx to it, so the
// tests exercise the same Parse/Bind/Describe/Execute sequence a real client
// uses. pgx's default QueryExecMode is cache_statement.
func connectPGX(t *testing.T) *pgx.Conn {
	t.Helper()
	kv := testkv.New(t)
	pool := kv.Pool(4)
	t.Cleanup(func() { pool.Close() })
	dbManager := storage.NewDatabaseManager(pool, &storage.DatabaseManagerConfig{
		DefaultDatabase: "pgx_parameters",
		AutoCreate:      true,
	})

	server, client := net.Pipe()
	done := make(chan error, 1)
	conn := NewConnection(server, dbManager, true)
	go func() { done <- conn.Handle() }()
	t.Cleanup(func() {
		client.Close()
		server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server connection did not close")
		}
	})

	config, err := pgx.ParseConfig("postgres://tester@localhost:5432/pgx_parameters?sslmode=disable")
	if err != nil {
		t.Fatalf("parse pgx config: %v", err)
	}
	config.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return client, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pgxConn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	t.Cleanup(func() { _ = pgxConn.Close(context.Background()) })
	return pgxConn
}

func preparePGXSchema(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, doc JSONB)`,
		`INSERT INTO t (id, name, doc) VALUES (1, 'alpha', '{"a": 1}')`,
		`INSERT INTO t (id, name, doc) VALUES (2, 'beta', '{"b": 2}')`,
		`CREATE TABLE n (id INTEGER PRIMARY KEY, amount NUMERIC)`,
		`INSERT INTO n (id, amount) VALUES (1, '1.5'), (2, '2.25')`,
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// ParameterDescription carries the inferred OID for every parameter, which is
// what pgx validates its argument list against.
func TestPGXDescribesInferredParameterTypes(t *testing.T) {
	conn := connectPGX(t)
	preparePGXSchema(t, conn)
	ctx := context.Background()
	cases := []struct {
		sql  string
		want []uint32
	}{
		{`SELECT $1::INTEGER`, []uint32{23}},
		{`SELECT $1::TEXT[]`, []uint32{1009}},
		{`SELECT $1::JSONB`, []uint32{3802}},
		{`SELECT id FROM t WHERE id = $1`, []uint32{23}},
		{`SELECT name FROM t WHERE name = $1`, []uint32{25}},
		{`SELECT id FROM t WHERE id = ANY($1)`, []uint32{1007}},
		{`SELECT id FROM t WHERE name = ANY($1)`, []uint32{1009}},
		{`SELECT id FROM n WHERE amount = ANY($1)`, []uint32{1231}},
		{`SELECT id FROM t LIMIT $1 OFFSET $2`, []uint32{20, 20}},
		{`SELECT doc FROM t WHERE doc = $1::JSONB`, []uint32{3802}},
		{`SELECT doc FROM t WHERE doc = $1`, []uint32{3802}},
		{`INSERT INTO t (id, name) VALUES ($1, $2)`, []uint32{23, 25}},
		{`UPDATE t SET name = $1 WHERE id = $2`, []uint32{25, 23}},
		{`SELECT $1`, []uint32{25}},
	}
	for i, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			sd, err := conn.Prepare(ctx, fmt.Sprintf("described_%d", i), tc.sql)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if !reflect.DeepEqual(sd.ParamOIDs, tc.want) {
				t.Fatalf("ParamOIDs = %v, want %v", sd.ParamOIDs, tc.want)
			}
		})
	}
}

// The default connection mode (cache_statement) and the other describe-based
// modes bind and execute typed parameters.
func TestPGXDefaultQueryModes(t *testing.T) {
	conn := connectPGX(t)
	preparePGXSchema(t, conn)
	ctx := context.Background()
	modes := []struct {
		name string
		mode pgx.QueryExecMode
	}{
		{"cache_statement", pgx.QueryExecModeCacheStatement},
		{"cache_describe", pgx.QueryExecModeCacheDescribe},
		{"describe_exec", pgx.QueryExecModeDescribeExec},
	}
	for i, tc := range modes {
		insertID := int32(10 + i)
		t.Run(tc.name, func(t *testing.T) {
			var integer int32
			if err := conn.QueryRow(ctx, `SELECT $1::INTEGER`, tc.mode, int32(41)).Scan(&integer); err != nil {
				t.Fatalf("cast parameter: %v", err)
			}
			if integer != 41 {
				t.Fatalf("cast parameter = %d, want 41", integer)
			}

			var id int32
			if err := conn.QueryRow(ctx, `SELECT id FROM t WHERE id = $1`, tc.mode, int32(1)).Scan(&id); err != nil {
				t.Fatalf("comparison parameter: %v", err)
			}
			if id != 1 {
				t.Fatalf("comparison parameter = %d, want 1", id)
			}

			var ids []int32
			rows, err := conn.Query(ctx, `SELECT id FROM t WHERE id = ANY($1) ORDER BY id`, tc.mode, []int32{1, 2})
			if err != nil {
				t.Fatalf("ANY parameter: %v", err)
			}
			for rows.Next() {
				var value int32
				if err := rows.Scan(&value); err != nil {
					t.Fatalf("scan ANY row: %v", err)
				}
				ids = append(ids, value)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("ANY rows: %v", err)
			}
			if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
				t.Fatalf("ANY rows = %v, want [1 2]", ids)
			}

			var textIDs []int32
			rows, err = conn.Query(ctx, `SELECT id FROM t WHERE name = ANY($1) ORDER BY id`, tc.mode, []string{"alpha", "beta"})
			if err != nil {
				t.Fatalf("text[] parameter: %v", err)
			}
			for rows.Next() {
				var value int32
				if err := rows.Scan(&value); err != nil {
					t.Fatalf("scan text[] row: %v", err)
				}
				textIDs = append(textIDs, value)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("text[] rows: %v", err)
			}
			if len(textIDs) != 2 || textIDs[0] != 1 || textIDs[1] != 2 {
				t.Fatalf("text[] rows = %v, want [1 2]", textIDs)
			}

			var numericIDs []int32
			rows, err = conn.Query(ctx, `SELECT id FROM n WHERE amount = ANY($1) ORDER BY id`, tc.mode, []float64{1.5})
			if err != nil {
				t.Fatalf("numeric[] parameter: %v", err)
			}
			for rows.Next() {
				var value int32
				if err := rows.Scan(&value); err != nil {
					t.Fatalf("scan numeric[] row: %v", err)
				}
				numericIDs = append(numericIDs, value)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("numeric[] rows: %v", err)
			}
			if len(numericIDs) != 1 || numericIDs[0] != 1 {
				t.Fatalf("numeric[] rows = %v, want [1]", numericIDs)
			}

			tag, err := conn.Exec(ctx, `INSERT INTO t (id, name) VALUES ($1, $2)`, tc.mode, insertID, "inserted")
			if err != nil {
				t.Fatalf("insert parameter: %v", err)
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("insert affected %d rows, want 1", tag.RowsAffected())
			}
			var name string
			if err := conn.QueryRow(ctx, `SELECT name FROM t WHERE id = $1`, insertID).Scan(&name); err != nil {
				t.Fatalf("read inserted row: %v", err)
			}
			if name != "inserted" {
				t.Fatalf("inserted name = %q", name)
			}

			var doc string
			if err := conn.QueryRow(ctx, `SELECT doc FROM t WHERE doc = $1::JSONB`, tc.mode, map[string]any{"a": 1}).Scan(&doc); err != nil {
				t.Fatalf("jsonb parameter: %v", err)
			}
			if doc != `{"a":1}` {
				t.Fatalf("jsonb parameter = %q, want {\"a\":1}", doc)
			}

			var untyped string
			if err := conn.QueryRow(ctx, `SELECT $1`, tc.mode, "hello").Scan(&untyped); err != nil {
				t.Fatalf("untyped parameter: %v", err)
			}
			if untyped != "hello" {
				t.Fatalf("untyped parameter = %q", untyped)
			}
		})
	}
}

// A client-supplied Parse OID wins over inference, and Bind decodes the value
// with it.
func TestPGXExplicitParameterOIDWins(t *testing.T) {
	conn := connectPGX(t)
	ctx := context.Background()
	sd, err := conn.PgConn().Prepare(ctx, "explicit_oid", `SELECT $1`, []uint32{20})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !reflect.DeepEqual(sd.ParamOIDs, []uint32{20}) {
		t.Fatalf("ParamOIDs = %v, want [20]", sd.ParamOIDs)
	}
	result := conn.PgConn().ExecPrepared(ctx, "explicit_oid", [][]byte{[]byte("42")}, []int16{0}, []int16{0}).Read()
	if result.Err != nil {
		t.Fatalf("execute: %v", result.Err)
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 || string(result.Rows[0][0]) != "42" {
		t.Fatalf("explicit OID rows = %v, want [[42]]", result.Rows)
	}
}

// An explicit OID that contradicts the inferred one wins for that parameter
// while the other parameters are still inferred; the base branch would have
// described 0 for the unspecified parameter.
func TestPGXExplicitParameterOIDConflict(t *testing.T) {
	conn := connectPGX(t)
	preparePGXSchema(t, conn)
	ctx := context.Background()

	sd, err := conn.PgConn().Prepare(ctx, "conflict_mixed", `SELECT $1::TEXT, id FROM t WHERE id = $2`, []uint32{0, 20})
	if err != nil {
		t.Fatalf("prepare mixed: %v", err)
	}
	if want := []uint32{25, 20}; !reflect.DeepEqual(sd.ParamOIDs, want) {
		t.Fatalf("ParamOIDs = %v, want %v", sd.ParamOIDs, want)
	}
	result := conn.PgConn().ExecPrepared(ctx, "conflict_mixed", [][]byte{[]byte("x"), []byte("1")}, []int16{0, 0}, []int16{0}).Read()
	if result.Err != nil {
		t.Fatalf("execute mixed: %v", result.Err)
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 2 || string(result.Rows[0][0]) != "x" || string(result.Rows[0][1]) != "1" {
		t.Fatalf("mixed explicit rows = %v, want [[x 1]]", result.Rows)
	}

	// The client's OID beats the cast on the same parameter.
	sd, err = conn.PgConn().Prepare(ctx, "conflict_cast", `SELECT $1::INTEGER`, []uint32{25})
	if err != nil {
		t.Fatalf("prepare cast: %v", err)
	}
	if want := []uint32{25}; !reflect.DeepEqual(sd.ParamOIDs, want) {
		t.Fatalf("cast conflict ParamOIDs = %v, want %v", sd.ParamOIDs, want)
	}
	result = conn.PgConn().ExecPrepared(ctx, "conflict_cast", [][]byte{[]byte("41")}, []int16{0}, []int16{0}).Read()
	if result.Err != nil {
		t.Fatalf("execute cast: %v", result.Err)
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 || string(result.Rows[0][0]) != "41" {
		t.Fatalf("cast conflict rows = %v, want [[41]]", result.Rows)
	}
}

// The described parameter count is the number of placeholders, so a client
// catches a wrong argument count before sending Bind instead of failing on an
// empty declaration.
func TestPGXParameterCountMismatch(t *testing.T) {
	conn := connectPGX(t)
	preparePGXSchema(t, conn)
	ctx := context.Background()
	var value int32
	for _, tc := range []struct {
		name string
		args []any
		want string
	}{
		{"missing_argument", nil, "expected 1 arguments, got 0"},
		{"extra_argument", []any{int32(1), int32(2)}, "expected 1 arguments, got 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := conn.QueryRow(ctx, `SELECT id FROM t WHERE id = $1`, tc.args...).Scan(&value)
			if err == nil {
				t.Fatal("query with a wrong argument count succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
