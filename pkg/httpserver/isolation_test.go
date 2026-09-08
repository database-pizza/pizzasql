package httpserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

func newTestDBServer(t *testing.T) *Server {
	t.Helper()
	kv := testkv.New(t)
	pool := kv.Pool(8)
	t.Cleanup(func() { pool.Close() })

	dm := storage.NewDatabaseManager(pool, &storage.DatabaseManagerConfig{
		DefaultDatabase: "testdb",
		AutoCreate:      true,
	})
	config := DefaultConfig()
	config.EnableAuth = false
	return NewWithDatabaseManager(config, dm)
}

func postQuery(t *testing.T, s *Server, sql string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(QueryRequest{SQL: sql})
	r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleQuery(w, r)
	return w
}

func postTransactionQuery(t *testing.T, s *Server, txID, sql string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(QueryRequest{SQL: sql, TransactionID: txID})
	r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleQuery(w, r)
	return w
}

func beginTransaction(t *testing.T, s *Server, database string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/transaction/begin", nil)
	r.Header.Set("X-Database", database)
	w := httptest.NewRecorder()
	s.handleTransactionBegin(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("begin status %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp["transactionId"].(string)
}

func postExecute(t *testing.T, s *Server, req ExecuteRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/execute", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleExecute(w, r)
	return w
}

// TestHTTPTransactionIsolation verifies that concurrent HTTP requests do not
// share mutable executor state: two concurrent transactional batch executes
// both commit their own rows without cross-talk.
func TestHTTPTransactionIsolation(t *testing.T) {
	s := newTestDBServer(t)
	w := postQuery(t, s, "CREATE TABLE items (id INTEGER PRIMARY KEY, v TEXT)")
	if w.Code != http.StatusOK {
		t.Fatalf("create table: %d %s", w.Code, w.Body.String())
	}

	const n = 20
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := ExecuteRequest{
				Transaction: true,
				Statements: []QueryRequest{
					{SQL: "INSERT INTO items VALUES (" + itoa(i) + ", 'v')"},
				},
			}
			codes <- postExecute(t, s, req).Code
		}(i)
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("execute transaction returned status %d", code)
		}
	}

	res := postQuery(t, s, "SELECT COUNT(*) FROM items")
	var resp QueryResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0][0] != float64(n) {
		t.Fatalf("expected %d rows, got %v", n, resp.Rows)
	}
}

// TestHTTPTransactionEndpointsIsolation verifies that the session-based
// BEGIN/COMMIT endpoints keep separate transactions isolated and single-use.
func TestHTTPTransactionEndpointsIsolation(t *testing.T) {
	s := newTestDBServer(t)
	if w := postQuery(t, s, "CREATE TABLE t (id INTEGER PRIMARY KEY)"); w.Code != http.StatusOK {
		t.Fatalf("create table: %d", w.Code)
	}

	begin := func() string {
		r := httptest.NewRequest(http.MethodPost, "/transaction/begin", nil)
		w := httptest.NewRecorder()
		s.handleTransactionBegin(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("begin status %d", w.Code)
		}
		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		return resp["transactionId"].(string)
	}
	commit := func(txID string) int {
		body, _ := json.Marshal(TransactionRequest{TransactionID: txID})
		r := httptest.NewRequest(http.MethodPost, "/transaction/commit", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleTransactionCommit(w, r)
		return w.Code
	}

	tx1 := begin()
	tx2 := begin()
	if tx1 == tx2 {
		t.Fatal("expected distinct transaction IDs")
	}
	if w := postTransactionQuery(t, s, tx1, "INSERT INTO t VALUES (1)"); w.Code != http.StatusOK {
		t.Fatalf("tx1 insert status %d: %s", w.Code, w.Body.String())
	}
	if w := postTransactionQuery(t, s, tx2, "INSERT INTO t VALUES (2)"); w.Code != http.StatusOK {
		t.Fatalf("tx2 insert status %d: %s", w.Code, w.Body.String())
	}
	if w := postQuery(t, s, "SELECT COUNT(*) FROM t"); w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("[[0]]")) {
		t.Fatalf("uncommitted rows became visible: %d %s", w.Code, w.Body.String())
	}

	// Committing one transaction must not affect the other.
	if code := commit(tx1); code != http.StatusOK {
		t.Fatalf("commit tx1 status %d", code)
	}
	// A second commit of the same transaction fails (single-use).
	if code := commit(tx1); code != http.StatusNotFound {
		t.Fatalf("expected single-use transaction, got status %d", code)
	}
	// The other transaction is still usable.
	if code := commit(tx2); code != http.StatusOK {
		t.Fatalf("commit tx2 status %d", code)
	}
}

func TestHTTPTransactionIDsAreDatabaseScopedAndExpire(t *testing.T) {
	s := newTestDBServer(t)
	txID := beginTransaction(t, s, "tenant-a")

	body, _ := json.Marshal(QueryRequest{SQL: "SELECT 1", TransactionID: txID})
	r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	r.Header.Set("X-Database", "tenant-b")
	w := httptest.NewRecorder()
	s.handleQuery(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-database transaction returned %d, want 404", w.Code)
	}

	s.transactionExecutorsMu.Lock()
	s.transactionExecutors[txID].expiresAt = time.Now().Add(-time.Second)
	s.transactionExecutorsMu.Unlock()
	body, _ = json.Marshal(QueryRequest{SQL: "SELECT 1", TransactionID: txID})
	r = httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	r.Header.Set("X-Database", "tenant-a")
	w = httptest.NewRecorder()
	s.handleQuery(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expired transaction returned %d, want 404", w.Code)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
