package httpserver

import (
	"encoding/hex"
	"log"
	"net/http"

	"github.com/goccy/go-json"
)

// sanitizeRows converts non-JSON scalar values into a lossless textual form.
// BLOBs are rendered as PostgreSQL-style \x-hex so HTTP clients receive the
// same bytea text representation as the PostgreSQL wire protocol instead of an
// encoding-dependent base64 string.
func sanitizeRows(rows [][]interface{}) [][]interface{} {
	for _, row := range rows {
		for i, v := range row {
			if b, ok := v.([]byte); ok {
				row[i] = `\x` + hex.EncodeToString(b)
			}
		}
	}
	return rows
}

// ColumnInfo represents column metadata.
type ColumnInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// QueryResponse represents a query response.
type QueryResponse struct {
	Columns      []ColumnInfo    `json:"columns"`
	Rows         [][]interface{} `json:"rows"`
	RowsAffected int64           `json:"rowsAffected"`
	LastInsertID int64           `json:"lastInsertId"`
	QueryPlan    []string        `json:"queryPlan,omitempty"`
	// Metrics for usage tracking and billing
	BytesRead          int64 `json:"bytesRead"`          // Total bytes in the result set
	RowsReturned       int   `json:"rowsReturned"`       // Number of rows returned
	ExecutionTimeMicro int64 `json:"executionTimeMicro"` // Execution time in microseconds
}

// ExecuteResult represents a single execution result.
type ExecuteResult struct {
	RowsAffected int64 `json:"rowsAffected"`
	LastInsertID int64 `json:"lastInsertId"`
}

// ExecuteResponse represents a batch execution response.
type ExecuteResponse struct {
	Results       []ExecuteResult `json:"results"`
	ExecutionTime string          `json:"executionTime"`
}

// ErrorResponse represents an error response.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains error details.
type ErrorDetail struct {
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details,omitempty"`
}

// HTTPError represents an HTTP error with custom fields.
type HTTPError struct {
	Code    string
	Message string
	Status  int
	Details map[string]interface{}
}

func (e *HTTPError) Error() string {
	return e.Message
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, data interface{}, pretty bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	encoder := json.NewEncoder(w)
	if pretty {
		encoder.SetIndent("", "  ")
	}
	encoder.Encode(data)
}

// writeError writes an error response.
func writeError(w http.ResponseWriter, status int, code, message string, details map[string]interface{}) {
	resp := ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
			Details: details,
		},
	}

	// Log server errors (5xx status codes)
	if status >= 500 {
		log.Printf("ERROR [%d] %s: %s", status, code, message)
	}

	writeJSON(w, status, resp, false)
}
