package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/version"
)

func TestVersionEndpointReportsBuildAndCompatibility(t *testing.T) {
	server := setupKVTestServer(t)

	r := httptest.NewRequest(http.MethodGet, "/version", nil)
	w := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp["version"]; got != version.String() {
		t.Fatalf("version = %v, want %q", got, version.String())
	}
	if got := resp["postgres_compat"]; got != version.PostgreSQLCompatVersion {
		t.Fatalf("postgres_compat = %v, want %q", got, version.PostgreSQLCompatVersion)
	}
	if got := resp["sqlite_compat"]; got != executor.SQLiteCompatVersion {
		t.Fatalf("sqlite_compat = %v, want %q", got, executor.SQLiteCompatVersion)
	}
	if _, ok := resp["commit"]; !ok {
		t.Fatal("response is missing commit")
	}
}
