package httpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

// setupKVTestServer builds an HTTP server backed by the in-memory testkv so
// these feature tests never depend on a running PizzaKV process.
func setupKVTestServer(t *testing.T) *Server {
	t.Helper()
	kv := testkv.New(t)
	pool := kv.Pool(4)
	t.Cleanup(func() { pool.Close() })
	schema := storage.NewSchemaManager(pool, "test_http_features")
	table := storage.NewTableManager(pool, schema, "test_http_features")
	exec := executor.New(schema, table)
	config := DefaultConfig()
	config.EnableAuth = false
	return New(config, exec, schema)
}

func queryHTTP(t *testing.T, server *Server, sql string) QueryResponse {
	t.Helper()
	body, _ := json.Marshal(QueryRequest{SQL: sql})
	r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleQuery(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("query %q: status %d body %s", sql, w.Code, w.Body.String())
	}
	var resp QueryResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestHTTPInsertReturning(t *testing.T) {
	server := setupKVTestServer(t)
	queryHTTP(t, server, "CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)")

	resp := queryHTTP(t, server, "INSERT INTO users (name) VALUES ('alice') RETURNING id, name")
	if len(resp.Columns) != 2 || resp.Columns[0].Name != "id" || resp.Columns[1].Name != "name" {
		t.Fatalf("unexpected columns %#v", resp.Columns)
	}
	if len(resp.Rows) != 1 || resp.Rows[0][0].(float64) != 1 || resp.Rows[0][1] != "alice" {
		t.Fatalf("unexpected rows %#v", resp.Rows)
	}
	if resp.RowsAffected != 1 || resp.LastInsertID != 1 {
		t.Fatalf("rowsAffected=%d lastInsertId=%d", resp.RowsAffected, resp.LastInsertID)
	}
}

func TestHTTPBlobTextWire(t *testing.T) {
	server := setupKVTestServer(t)
	queryHTTP(t, server, "CREATE TABLE blobs (id INTEGER PRIMARY KEY, data BLOB)")
	queryHTTP(t, server, "INSERT INTO blobs (id, data) VALUES (1, X'00FF10')")

	resp := queryHTTP(t, server, "SELECT data FROM blobs WHERE id = 1")
	if len(resp.Rows) != 1 {
		t.Fatalf("expected 1 row, got %#v", resp.Rows)
	}
	if resp.Rows[0][0] != `\x00ff10` {
		t.Fatalf("blob HTTP representation = %v, want \\x00ff10", resp.Rows[0][0])
	}
}

func TestHTTPSQLiteVersionAndPercentDiff(t *testing.T) {
	server := setupKVTestServer(t)
	resp := queryHTTP(t, server, "SELECT sqlite_version(), percent_diff(1, 2)")
	if resp.Rows[0][0] != executor.SQLiteCompatVersion {
		t.Fatalf("sqlite_version = %v", resp.Rows[0][0])
	}
	if resp.Rows[0][1].(float64) != 100 {
		t.Fatalf("percent_diff = %v", resp.Rows[0][1])
	}
}
