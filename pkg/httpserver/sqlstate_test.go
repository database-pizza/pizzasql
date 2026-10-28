package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestSQLStateHTTPErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unique", fmt.Errorf("UNIQUE constraint failed: uniq_users_email"), http.StatusConflict, "23505"},
		{"serialization", fmt.Errorf("commit: %w", storage.ErrSerialization), http.StatusConflict, "40001"},
		{"unsupported", fmt.Errorf("feature not supported: DEFAULT is not constant"), http.StatusBadRequest, "0A000"},
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
