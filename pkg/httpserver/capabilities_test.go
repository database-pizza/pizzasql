package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCapabilitiesEndpoint(t *testing.T) {
	server := setupKVTestServer(t)

	r := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	w := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		SchemaVersion int               `json:"schema_version"`
		Engine        string            `json:"engine"`
		ErrorCodes    map[string]string `json:"error_codes"`
		Constraints   struct {
			Unique struct {
				Supported bool   `json:"supported"`
				SQLState  string `json:"sqlstate"`
			} `json:"unique"`
		} `json:"constraints"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SchemaVersion < 1 {
		t.Fatalf("schema_version = %d", resp.SchemaVersion)
	}
	if resp.Engine != "pizzasql" {
		t.Fatalf("engine = %q", resp.Engine)
	}
	if !resp.Constraints.Unique.Supported || resp.Constraints.Unique.SQLState != "23505" {
		t.Fatalf("unique capability = %+v", resp.Constraints.Unique)
	}
	if resp.ErrorCodes["40001"] != "serialization_failure" {
		t.Fatalf("error_codes = %v", resp.ErrorCodes)
	}
}
