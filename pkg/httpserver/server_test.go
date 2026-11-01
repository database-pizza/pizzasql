package httpserver

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

func setupTestServer(t *testing.T) (*Server, *storage.KVPool) {
	pool, err := storage.NewKVPool("localhost:8085", 5, 5*time.Second)
	if err != nil {
		t.Skip("PizzaKV not available, skipping HTTP server tests")
	}

	schema := storage.NewSchemaManager(pool, "test_http_db")
	table := storage.NewTableManager(pool, schema, "test_http_db")
	exec := executor.New(schema, table)

	config := DefaultConfig()
	config.EnableAuth = false

	server := New(config, exec, schema)

	return server, pool
}

func TestQueryEndpoint(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	// Create test table
	req := QueryRequest{
		SQL: "CREATE TABLE test_users (id INTEGER PRIMARY KEY, name TEXT)",
	}
	body, _ := json.Marshal(req)

	r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w := httptest.NewRecorder()

	server.handleQuery(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Insert data
	req = QueryRequest{
		SQL: "INSERT INTO test_users (id, name) VALUES (1, 'Alice')",
	}
	body, _ = json.Marshal(req)

	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w = httptest.NewRecorder()

	server.handleQuery(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp QueryResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if resp.RowsAffected != 1 {
		t.Errorf("expected 1 row affected, got %d", resp.RowsAffected)
	}

	// Query data
	req = QueryRequest{
		SQL: "SELECT * FROM test_users WHERE id = 1",
	}
	body, _ = json.Marshal(req)

	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w = httptest.NewRecorder()

	server.handleQuery(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	json.NewDecoder(w.Body).Decode(&resp)

	if len(resp.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(resp.Rows))
	}

	// Cleanup
	req = QueryRequest{SQL: "DROP TABLE test_users"}
	body, _ = json.Marshal(req)
	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w = httptest.NewRecorder()
	server.handleQuery(w, r)
}

func TestExecuteEndpoint(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	// Create table first
	createReq := QueryRequest{
		SQL: "CREATE TABLE test_batch (id INTEGER PRIMARY KEY, value TEXT)",
	}
	body, _ := json.Marshal(createReq)
	r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleQuery(w, r)

	// Batch insert
	req := ExecuteRequest{
		Statements: []QueryRequest{
			{SQL: "INSERT INTO test_batch (id, value) VALUES (1, 'first')"},
			{SQL: "INSERT INTO test_batch (id, value) VALUES (2, 'second')"},
		},
		Transaction: true,
	}
	body, _ = json.Marshal(req)

	r = httptest.NewRequest(http.MethodPost, "/execute", bytes.NewReader(body))
	w = httptest.NewRecorder()

	server.handleExecute(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp ExecuteResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if len(resp.Results) != 2 {
		t.Errorf("expected 2 results, got %d", len(resp.Results))
	}

	// Cleanup
	dropReq := QueryRequest{SQL: "DROP TABLE test_batch"}
	body, _ = json.Marshal(dropReq)
	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w = httptest.NewRecorder()
	server.handleQuery(w, r)
}

func TestSchemaEndpoints(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	// Create test table
	createReq := QueryRequest{
		SQL: "CREATE TABLE test_schema (id INTEGER PRIMARY KEY, name TEXT)",
	}
	body, _ := json.Marshal(createReq)
	r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleQuery(w, r)

	// List tables
	r = httptest.NewRequest(http.MethodGet, "/schema/tables", nil)
	w = httptest.NewRecorder()

	server.handleSchemaTables(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var tablesResp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&tablesResp)

	tables := tablesResp["tables"].([]interface{})
	found := false
	for _, table := range tables {
		if table.(string) == "test_schema" {
			found = true
			break
		}
	}

	if !found {
		t.Error("test_schema table not found in list")
	}

	// Get table schema
	r = httptest.NewRequest(http.MethodGet, "/schema/tables/test_schema", nil)
	w = httptest.NewRecorder()

	server.handleSchemaTable(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Cleanup
	dropReq := QueryRequest{SQL: "DROP TABLE test_schema"}
	body, _ = json.Marshal(dropReq)
	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w = httptest.NewRecorder()
	server.handleQuery(w, r)
}

func TestHealthEndpoint(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got '%v'", resp["status"])
	}
}

func TestStatsEndpoint(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	r := httptest.NewRequest(http.MethodGet, "/stats", nil)
	w := httptest.NewRecorder()

	server.handleStats(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)

	if _, ok := resp["queriesExecuted"]; !ok {
		t.Error("expected queriesExecuted in response")
	}
}

func TestReadOnlyMode(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	// Try to insert in readonly mode
	req := QueryRequest{
		SQL: "INSERT INTO test (id) VALUES (1)",
	}
	body, _ := json.Marshal(req)

	r := httptest.NewRequest(http.MethodPost, "/query?readonly=true", bytes.NewReader(body))
	w := httptest.NewRecorder()

	server.handleQuery(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", w.Code)
	}
}

func TestCORSMiddleware(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	r := httptest.NewRequest(http.MethodOptions, "/query", nil)
	w := httptest.NewRecorder()

	handler := server.corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	handler.ServeHTTP(w, r)

	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS headers not set correctly")
	}
}

func TestParameterizedQuery(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	// Create test table
	createReq := QueryRequest{
		SQL: "CREATE TABLE test_params (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)",
	}
	body, _ := json.Marshal(createReq)
	r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleQuery(w, r)

	// Insert with parameters
	req := QueryRequest{
		SQL:    "INSERT INTO test_params (id, name, age) VALUES (?, ?, ?)",
		Params: []interface{}{1, "Alice", 30},
	}
	body, _ = json.Marshal(req)

	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w = httptest.NewRecorder()

	server.handleQuery(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Query with parameters
	req = QueryRequest{
		SQL:    "SELECT * FROM test_params WHERE name = ?",
		Params: []interface{}{"Alice"},
	}
	body, _ = json.Marshal(req)

	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w = httptest.NewRecorder()

	server.handleQuery(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp QueryResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if len(resp.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(resp.Rows))
	}

	// Cleanup
	dropReq := QueryRequest{SQL: "DROP TABLE test_params"}
	body, _ = json.Marshal(dropReq)
	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w = httptest.NewRecorder()
	server.handleQuery(w, r)
}

func TestSubstituteParams(t *testing.T) {
	tests := []struct {
		sql      string
		params   []interface{}
		expected string
	}{
		{"SELECT * FROM users WHERE id = ?", []interface{}{42}, "SELECT * FROM users WHERE id = 42"},
		{"SELECT * FROM users WHERE name = ?", []interface{}{"Alice"}, "SELECT * FROM users WHERE name = 'Alice'"},
		{"SELECT * FROM users WHERE name = ?", []interface{}{"O'Brien"}, "SELECT * FROM users WHERE name = 'O''Brien'"},
		{"INSERT INTO t (a, b) VALUES (?, ?)", []interface{}{1, "test"}, "INSERT INTO t (a, b) VALUES (1, 'test')"},
		{"SELECT * FROM t WHERE x = ?", []interface{}{nil}, "SELECT * FROM t WHERE x = NULL"},
		{"SELECT * FROM t WHERE x = ?", []interface{}{true}, "SELECT * FROM t WHERE x = 1"},
		{"SELECT * FROM t WHERE x = ?", []interface{}{false}, "SELECT * FROM t WHERE x = 0"},
		{"SELECT * FROM t WHERE x = ?", []interface{}{3.14}, "SELECT * FROM t WHERE x = 3.14"},
	}

	for _, tt := range tests {
		result := substituteParams(tt.sql, tt.params)
		if result != tt.expected {
			t.Errorf("substituteParams(%q, %v) = %q, want %q", tt.sql, tt.params, result, tt.expected)
		}
	}
}

func TestCompressionMiddleware(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	// Create a handler that returns some JSON
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"message": "hello world"}`))
	})

	// Wrap with compression middleware
	handler := server.compressionMiddleware(testHandler)

	// Request with gzip accept header
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, r)

	// Check that response is gzip encoded
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Error("expected gzip Content-Encoding header")
	}

	// Decompress and verify content
	gr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer gr.Close()

	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("failed to read gzip body: %v", err)
	}

	if string(body) != `{"message": "hello world"}` {
		t.Errorf("unexpected body: %s", string(body))
	}
}

func TestCompressionMiddlewareNoGzip(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	// Create a handler that returns some text
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello world"))
	})

	// Wrap with compression middleware
	handler := server.compressionMiddleware(testHandler)

	// Request WITHOUT gzip accept header
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, r)

	// Check that response is NOT gzip encoded
	if w.Header().Get("Content-Encoding") == "gzip" {
		t.Error("should not have gzip encoding without Accept-Encoding header")
	}

	if w.Body.String() != "hello world" {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestMetricsEndpoint(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	server.handleMetrics(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()

	// Check for expected Prometheus metrics
	expectedMetrics := []string{
		"pizzasql_queries_total",
		"pizzasql_queries_executed_total",
		"pizzasql_tables_count",
		"pizzasql_uptime_seconds",
		"pizzasql_info",
	}

	for _, metric := range expectedMetrics {
		if !strings.Contains(body, metric) {
			t.Errorf("expected metric %s in response", metric)
		}
	}

	// Check content type
	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") {
		t.Errorf("expected text/plain content type, got %s", contentType)
	}
}

func TestInferType(t *testing.T) {
	tests := []struct {
		value    interface{}
		expected string
	}{
		{nil, "NULL"},
		{int64(42), "INTEGER"},
		{int(42), "INTEGER"},
		{3.14, "REAL"},
		{"hello", "TEXT"},
		{[]byte{1, 2, 3}, "BLOB"},
		{true, "INTEGER"},
		{false, "INTEGER"},
	}

	for _, tt := range tests {
		result := inferType(tt.value)
		if result != tt.expected {
			t.Errorf("inferType(%v) = %s, want %s", tt.value, result, tt.expected)
		}
	}
}

func TestTransactionEndpoints(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	// Test BEGIN transaction
	r := httptest.NewRequest(http.MethodPost, "/transaction/begin", nil)
	w := httptest.NewRecorder()
	server.handleTransactionBegin(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("BEGIN: expected status 200, got %d", w.Code)
	}

	var beginResp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&beginResp)
	if beginResp["status"] != "started" {
		t.Errorf("BEGIN: expected status 'started', got '%v'", beginResp["status"])
	}

	// Test COMMIT transaction
	body, err := json.Marshal(TransactionRequest{TransactionID: beginResp["transactionId"].(string)})
	if err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest(http.MethodPost, "/transaction/commit", bytes.NewReader(body))
	w = httptest.NewRecorder()
	server.handleTransactionCommit(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("COMMIT: expected status 200, got %d", w.Code)
	}

	var commitResp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&commitResp)
	if commitResp["status"] != "committed" {
		t.Errorf("COMMIT: expected status 'committed', got '%v'", commitResp["status"])
	}

	// Test BEGIN again for rollback test
	r = httptest.NewRequest(http.MethodPost, "/transaction/begin", nil)
	w = httptest.NewRecorder()
	server.handleTransactionBegin(w, r)
	if err := json.NewDecoder(w.Body).Decode(&beginResp); err != nil {
		t.Fatal(err)
	}
	body, err = json.Marshal(TransactionRequest{TransactionID: beginResp["transactionId"].(string)})
	if err != nil {
		t.Fatal(err)
	}

	// Test ROLLBACK transaction
	r = httptest.NewRequest(http.MethodPost, "/transaction/rollback", bytes.NewReader(body))
	w = httptest.NewRecorder()
	server.handleTransactionRollback(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("ROLLBACK: expected status 200, got %d", w.Code)
	}

	var rollbackResp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&rollbackResp)
	if rollbackResp["status"] != "rolled back" {
		t.Errorf("ROLLBACK: expected status 'rolled back', got '%v'", rollbackResp["status"])
	}
}

func TestTransactionEndpointsMethodNotAllowed(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	// Test GET on transaction endpoints (should fail)
	endpoints := []struct {
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"/transaction/begin", server.handleTransactionBegin},
		{"/transaction/commit", server.handleTransactionCommit},
		{"/transaction/rollback", server.handleTransactionRollback},
	}

	for _, ep := range endpoints {
		r := httptest.NewRequest(http.MethodGet, ep.path, nil)
		w := httptest.NewRecorder()
		ep.handler(w, r)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected status 405 for GET, got %d", ep.path, w.Code)
		}
	}
}

func TestAuthMiddleware(t *testing.T) {
	pool, err := storage.NewKVPool("localhost:8085", 5, 5*time.Second)
	if err != nil {
		t.Skip("PizzaKV not available, skipping auth tests")
	}
	defer pool.Close()

	schema := storage.NewSchemaManager(pool, "test_auth_db")
	table := storage.NewTableManager(pool, schema, "test_auth_db")
	exec := executor.New(schema, table)

	config := DefaultConfig()
	config.EnableAuth = true
	config.APIKeys = []string{"test-api-key-123", "another-key-456"}

	server := New(config, exec, schema)

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	handler := server.authMiddleware(testHandler)

	// Test without Authorization header
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth header, got %d", w.Code)
	}

	// Test with invalid API key
	r = httptest.NewRequest(http.MethodGet, "/test", nil)
	r.Header.Set("Authorization", "Bearer invalid-key")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 with invalid key, got %d", w.Code)
	}

	// Test with valid API key
	r = httptest.NewRequest(http.MethodGet, "/test", nil)
	r.Header.Set("Authorization", "Bearer test-api-key-123")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with valid key, got %d", w.Code)
	}
}

func TestQueryEndpointErrors(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	// Test with missing SQL
	req := QueryRequest{
		SQL: "",
	}
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleQuery(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty SQL, got %d", w.Code)
	}

	// Test with invalid JSON
	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader([]byte("invalid json")))
	w = httptest.NewRecorder()
	server.handleQuery(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}

	// Test with syntax error
	req = QueryRequest{
		SQL: "SELEC * FORM users",
	}
	body, _ = json.Marshal(req)
	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w = httptest.NewRecorder()
	server.handleQuery(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for SQL syntax error, got %d", w.Code)
	}
}

func TestPrettyPrintOption(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	req := QueryRequest{
		SQL: "SELECT 1 as num",
	}
	body, _ := json.Marshal(req)

	// Without pretty
	r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleQuery(w, r)

	normalResponse := w.Body.String()

	// With pretty
	body, _ = json.Marshal(req)
	r = httptest.NewRequest(http.MethodPost, "/query?pretty=true", bytes.NewReader(body))
	w = httptest.NewRecorder()
	server.handleQuery(w, r)

	prettyResponse := w.Body.String()

	// Pretty response should be longer due to formatting
	if len(prettyResponse) <= len(normalResponse) {
		t.Error("pretty response should be longer than normal response")
	}

	// Pretty response should contain newlines
	if !strings.Contains(prettyResponse, "\n") {
		t.Error("pretty response should contain newlines")
	}
}

func TestSchemaTableNotFound(t *testing.T) {
	server, pool := setupTestServer(t)
	defer pool.Close()

	r := httptest.NewRequest(http.MethodGet, "/schema/tables/nonexistent_table_xyz", nil)
	w := httptest.NewRecorder()
	server.handleSchemaTable(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent table, got %d", w.Code)
	}
}
