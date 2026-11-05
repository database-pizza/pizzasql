package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

type httpErrorBody struct {
	Error struct {
		Code    string                 `json:"code"`
		Message string                 `json:"message"`
		Details map[string]interface{} `json:"details"`
	} `json:"error"`
}

func TestHTTPUniqueViolationReturns23505(t *testing.T) {
	server := setupKVTestServer(t)
	queryHTTP(t, server, "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE)")
	queryHTTP(t, server, "INSERT INTO users (id, email) VALUES (1, 'a@x')")

	body, _ := json.Marshal(QueryRequest{SQL: "INSERT INTO users (id, email) VALUES (2, 'a@x')"})
	r := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	server.handleQuery(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	var resp httpErrorBody
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "23505" {
		t.Fatalf("code = %q, want 23505", resp.Error.Code)
	}
	constraint, _ := resp.Error.Details["constraint"].(string)
	if constraint == "" || !strings.Contains(resp.Error.Message, constraint) {
		t.Fatalf("details.constraint = %q, message = %q", constraint, resp.Error.Message)
	}
}

// A duplicate composite key over HTTP keeps SQLSTATE 23505 and reports the key
// components instead of the length-prefixed durable encoding.
func TestHTTPCompositePrimaryKeyViolationReportsComponents(t *testing.T) {
	server := setupKVTestServer(t)
	queryHTTP(t, server, "CREATE TABLE pairs (a INTEGER, b INTEGER, PRIMARY KEY (a, b))")
	queryHTTP(t, server, "INSERT INTO pairs (a, b) VALUES (1, 11)")

	body, _ := json.Marshal(QueryRequest{SQL: "INSERT INTO pairs (a, b) VALUES (1, 11)"})
	r := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	server.handleQuery(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	var resp httpErrorBody
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "23505" {
		t.Fatalf("code = %q, want 23505", resp.Error.Code)
	}
	if want := "duplicate primary key: (1,11)"; resp.Error.Message != want {
		t.Fatalf("message = %q, want %q", resp.Error.Message, want)
	}
}

// A constraint the commit discovers must report its SQLSTATE instead of the
// generic TRANSACTION_ERROR the transaction endpoints use for other failures.
func TestHTTPCommitCheckViolationReturns23514(t *testing.T) {
	s := newTestDBServer(t)
	if w := postQuery(t, s, "CREATE TABLE t (id INTEGER PRIMARY KEY)"); w.Code != http.StatusOK {
		t.Fatalf("create table: %d %s", w.Code, w.Body.String())
	}
	txID := beginTransaction(t, s, "")
	if w := postTransactionQuery(t, s, txID, "INSERT INTO t (id) VALUES (1)"); w.Code != http.StatusOK {
		t.Fatalf("staged insert: %d %s", w.Code, w.Body.String())
	}
	// A second session publishes the constraint the staged row will violate.
	if w := postQuery(t, s, "ALTER TABLE t ADD COLUMN qty INTEGER DEFAULT -1 CHECK (qty >= 0)"); w.Code != http.StatusOK {
		t.Fatalf("add column: %d %s", w.Code, w.Body.String())
	}

	body, _ := json.Marshal(TransactionRequest{TransactionID: txID})
	r := httptest.NewRequest(http.MethodPost, "/transaction/commit", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	s.handleTransactionCommit(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("commit status = %d, want 400; body %s", w.Code, w.Body.String())
	}
	var resp httpErrorBody
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "23514" {
		t.Fatalf("code = %q, want 23514", resp.Error.Code)
	}
	constraint, _ := resp.Error.Details["constraint"].(string)
	if constraint == "" || !strings.Contains(resp.Error.Message, constraint) {
		t.Fatalf("details.constraint = %q, message = %q", constraint, resp.Error.Message)
	}
}

// The transaction endpoints only replace TRANSACTION_ERROR for constraint
// violations; other commit failures keep the generic code.
func TestConstraintViolationHTTPErrorIgnoresOtherErrors(t *testing.T) {
	if got := constraintViolationHTTPError(errors.New("boom")); got != nil {
		t.Fatalf("generic error mapped to %+v, want nil", got)
	}
}

func TestHTTPCheckViolationReturns23514(t *testing.T) {
	server := setupKVTestServer(t)
	queryHTTP(t, server, "CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER, CONSTRAINT positive_age CHECK (age > 0))")

	body, _ := json.Marshal(QueryRequest{SQL: "INSERT INTO t (id, age) VALUES (1, -1)"})
	r := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	server.handleQuery(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
	var resp httpErrorBody
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "23514" {
		t.Fatalf("code = %q, want 23514", resp.Error.Code)
	}
	constraint, _ := resp.Error.Details["constraint"].(string)
	if constraint != "positive_age" {
		t.Fatalf("details.constraint = %q, want positive_age", constraint)
	}
}

func TestSQLStateHTTPErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unique", fmt.Errorf("UNIQUE constraint failed: uniq_users_email"), http.StatusConflict, "23505"},
		{"foreign key", fmt.Errorf("FOREIGN KEY constraint failed: parent_id -> parent"), http.StatusConflict, "23503"},
		{"typed foreign key", &storage.ForeignKeyViolationError{Table: "child", Constraint: "parent_id -> parent"}, http.StatusConflict, "23503"},
		{"not null", fmt.Errorf("NOT NULL constraint failed: t.v"), http.StatusBadRequest, "23502"},
		{"check", fmt.Errorf("CHECK constraint failed: positive_age"), http.StatusBadRequest, "23514"},
		{"typed check", &storage.CheckViolationError{Table: "t", Constraint: "positive_age"}, http.StatusBadRequest, "23514"},
		{"typed not null", &storage.NotNullViolationError{Table: "t", Column: "v"}, http.StatusBadRequest, "23502"},
		{"typed unique", &storage.UniqueViolationError{Constraint: "uq_email"}, http.StatusConflict, "23505"},
		{"typed primary key", &storage.PrimaryKeyViolationError{Table: "t", Key: "1"}, http.StatusConflict, "23505"},
		{"serialization", fmt.Errorf("commit: %w", storage.ErrSerialization), http.StatusConflict, "40001"},
		{"unsupported", fmt.Errorf("feature not supported: DEFAULT is not constant"), http.StatusBadRequest, "0A000"},
		{"undefined table", pgerror.UndefinedTable("relation %q does not exist", "missing"), http.StatusNotFound, "42P01"},
		{"other", errors.New("boom"), http.StatusInternalServerError, "EXECUTION_ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqlStateHTTPError(tt.err)
			if got.Status != tt.status || got.Code != tt.code {
				t.Fatalf("got status=%d code=%s, want status=%d code=%s", got.Status, got.Code, tt.status, tt.code)
			}
		})
	}
}

// The batch /execute endpoint commits its own transaction, so a constraint the
// commit discovers must map to 23514 there too instead of TRANSACTION_ERROR.
func TestHTTPExecuteCommitCheckViolationReturns23514(t *testing.T) {
	s := newTestDBServer(t)
	if w := postQuery(t, s, "CREATE TABLE t (id INTEGER PRIMARY KEY)"); w.Code != http.StatusOK {
		t.Fatalf("create table: %d %s", w.Code, w.Body.String())
	}

	w := postExecute(t, s, ExecuteRequest{
		Transaction: true,
		Statements: []QueryRequest{
			{SQL: "INSERT INTO t (id) VALUES (1)"},
			{SQL: "ALTER TABLE t ADD COLUMN qty INTEGER DEFAULT -1 CHECK (qty >= 0)"},
		},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("execute status = %d, want 400; body %s", w.Code, w.Body.String())
	}
	var resp httpErrorBody
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "23514" {
		t.Fatalf("code = %q, want 23514", resp.Error.Code)
	}
}

// A dangling foreign key reports 23503 through the query endpoint, with the
// constraint label in details.
func TestHTTPForeignKeyViolationReturns23503(t *testing.T) {
	server := setupKVTestServer(t)
	queryHTTP(t, server, "CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	queryHTTP(t, server, "CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, CONSTRAINT child_parent FOREIGN KEY (parent_id) REFERENCES parent(id))")

	body, _ := json.Marshal(QueryRequest{SQL: "INSERT INTO child (id, parent_id) VALUES (1, 99)"})
	r := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	server.handleQuery(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	var resp httpErrorBody
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "23503" {
		t.Fatalf("code = %q, want 23503", resp.Error.Code)
	}
	if constraint, _ := resp.Error.Details["constraint"].(string); constraint != "child_parent" {
		t.Fatalf("details.constraint = %q, want child_parent", constraint)
	}
}

// Array containment and set-returning-function column aliases run through the
// HTTP surface with the same values and SQLSTATEs as the wire protocol.
func TestHTTPArrayContainmentAndSRFAlias(t *testing.T) {
	server := setupKVTestServer(t)
	queryHTTP(t, server, `CREATE TABLE items (id INTEGER PRIMARY KEY, nums INTEGER[])`)
	queryHTTP(t, server, `INSERT INTO items (id, nums) VALUES (1, '{1,2,NULL}'), (2, '{3}')`)

	resp := queryHTTP(t, server, `SELECT id FROM items WHERE nums @> ARRAY[2]`)
	if len(resp.Rows) != 1 || resp.Rows[0][0].(float64) != 1 {
		t.Fatalf("array containment rows = %#v", resp.Rows)
	}
	resp = queryHTTP(t, server, `SELECT id FROM items WHERE nums @> ARRAY[NULL]::INTEGER[]`)
	if len(resp.Rows) != 1 || resp.Rows[0][0].(float64) != 1 {
		t.Fatalf("NULL element containment rows = %#v", resp.Rows)
	}

	resp = queryHTTP(t, server, `SELECT n FROM unnest(ARRAY[5,6]) AS u(n) WHERE n = 6`)
	if len(resp.Columns) != 1 || resp.Columns[0].Name != "n" || len(resp.Rows) != 1 || resp.Rows[0][0].(float64) != 6 {
		t.Fatalf("alias list response = columns %#v rows %#v", resp.Columns, resp.Rows)
	}

	post := func(sql string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(QueryRequest{SQL: sql})
		r := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
		w := httptest.NewRecorder()
		server.handleQuery(w, r)
		return w
	}
	for _, tc := range []struct {
		sql  string
		code string
	}{
		{`SELECT * FROM unnest(ARRAY[1]) AS u(a, b)`, pgerror.CodeInvalidColumnReference},
		{`SELECT * FROM items WHERE nums @> 1`, pgerror.CodeDatatypeMismatch},
	} {
		w := post(tc.sql)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400; body %s", tc.sql, w.Code, w.Body.String())
		}
		var resp httpErrorBody
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Error.Code != tc.code {
			t.Fatalf("%s code = %q, want %s", tc.sql, resp.Error.Code, tc.code)
		}
	}
}
