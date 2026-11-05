package httpserver

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"github.com/danfragoso/pizzasql-next/pkg/analyzer"
	"github.com/danfragoso/pizzasql-next/pkg/capabilities"
	"github.com/danfragoso/pizzasql-next/pkg/csvexport"
	"github.com/danfragoso/pizzasql-next/pkg/csvimport"
	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/sqlexport"
	"github.com/danfragoso/pizzasql-next/pkg/sqlimport"
	"github.com/danfragoso/pizzasql-next/pkg/sqliteimport"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/version"
)

// QueryRequest represents a single query request.
type QueryRequest struct {
	SQL           string        `json:"sql"`
	Params        []interface{} `json:"params"`
	TransactionID string        `json:"transactionId,omitempty"`
}

// ExecuteRequest represents a batch execution request.
type ExecuteRequest struct {
	Statements  []QueryRequest `json:"statements"`
	Transaction bool           `json:"transaction"`
}

// TransactionRequest represents a transaction management request.
type TransactionRequest struct {
	TransactionID string `json:"transactionId"`
}

// handleQuery handles POST /query
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only POST method is allowed", nil)
		return
	}

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Invalid JSON in request body", nil)
		return
	}

	if req.SQL == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SQL", "SQL query is required", nil)
		return
	}

	var tx *transactionExecutor
	var exec *executor.Executor
	if req.TransactionID != "" {
		var ok bool
		tx, ok = s.getTransactionExecutor(req.TransactionID, r.Header.Get("X-Database"))
		if !ok {
			writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "unknown transaction ID", nil)
			return
		}
		exec = tx.exec
	} else {
		// Get database from X-Database header.
		dbName := r.Header.Get("X-Database")
		var err error
		exec, _, err = s.getExecutorForDatabase(dbName)
		if err != nil {
			writeError(w, http.StatusBadRequest, "DATABASE_ERROR", fmt.Sprintf("database not found: %s", dbName), nil)
			return
		}
	}

	// Check for pretty print
	pretty := r.URL.Query().Get("pretty") == "true"
	explain := r.URL.Query().Get("explain") == "true"
	readonly := r.URL.Query().Get("readonly") == "true"

	// Parse timeout
	timeout := 5 * time.Minute
	if t := r.URL.Query().Get("timeout"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		}
	}

	// Execute with timeout
	resultChan := make(chan *QueryResponse, 1)
	errorChan := make(chan error, 1)

	go func() {
		if tx != nil {
			defer tx.mu.Unlock()
		}
		start := time.Now()

		// Check readonly mode
		if readonly {
			upper := strings.ToUpper(strings.TrimSpace(req.SQL))
			if strings.HasPrefix(upper, "INSERT") ||
				strings.HasPrefix(upper, "UPDATE") ||
				strings.HasPrefix(upper, "DELETE") ||
				strings.HasPrefix(upper, "CREATE") ||
				strings.HasPrefix(upper, "DROP") ||
				strings.HasPrefix(upper, "ALTER") {
				errorChan <- &HTTPError{
					Code:    "READ_ONLY_MODE",
					Message: "Write operations not allowed in read-only mode",
					Status:  http.StatusForbidden,
				}
				return
			}
		}

		// Substitute parameters
		sql := substituteParams(req.SQL, req.Params)

		// Parse SQL
		l := lexer.New(sql)
		p := parser.New(l)
		stmt, err := p.Parse()
		if err != nil {
			errorChan <- &HTTPError{
				Code:    "SYNTAX_ERROR",
				Message: err.Error(),
				Status:  http.StatusBadRequest,
			}
			return
		}

		// Execute using the database-specific executor
		result, err := exec.Execute(stmt)
		if err != nil {
			errorChan <- sqlStateHTTPError(err)
			return
		}

		duration := time.Since(start)

		// Build response
		resp := &QueryResponse{
			Columns:            make([]ColumnInfo, len(result.Columns)),
			Rows:               sanitizeRows(result.Rows),
			RowsAffected:       result.RowsAffected,
			LastInsertID:       result.LastInsertID,
			LSN:                result.LSN,
			ExecutionTimeMicro: duration.Microseconds(),
			RowsReturned:       len(result.Rows),
		}

		for i, col := range result.Columns {
			colType := result.GetColumnType(i)
			// If no type info, infer from first row values
			if colType == "ANY" && len(result.Rows) > 0 && i < len(result.Rows[0]) {
				colType = inferType(result.Rows[0][i])
			}
			resp.Columns[i] = ColumnInfo{
				Name: col,
				Type: colType,
			}
		}

		// Calculate bytes read (approximate size of the result set)
		// This is the serialized JSON size of the rows data
		if jsonBytes, err := json.Marshal(result.Rows); err == nil {
			resp.BytesRead = int64(len(jsonBytes))
		}

		if explain {
			resp.QueryPlan = []string{"Full table scan"} // TODO: Real query plan
		}

		resultChan <- resp
	}()

	select {
	case resp := <-resultChan:
		atomic.AddInt64(&s.stats.QueriesExecuted, 1)
		atomic.AddInt64(&s.stats.QueriesSuccess, 1)
		writeJSON(w, http.StatusOK, resp, pretty)
	case err := <-errorChan:
		atomic.AddInt64(&s.stats.QueriesExecuted, 1)
		atomic.AddInt64(&s.stats.QueriesError, 1)
		if httpErr, ok := err.(*HTTPError); ok {
			writeError(w, httpErr.Status, httpErr.Code, httpErr.Message, httpErr.Details)
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		}
	case <-time.After(timeout):
		atomic.AddInt64(&s.stats.QueriesExecuted, 1)
		atomic.AddInt64(&s.stats.QueriesError, 1)
		writeError(w, http.StatusRequestTimeout, "TIMEOUT", "Query execution timeout", nil)
	}
}

// handleExecute handles POST /execute for batch operations
func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only POST method is allowed", nil)
		return
	}

	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Invalid JSON in request body", nil)
		return
	}

	if len(req.Statements) == 0 {
		writeError(w, http.StatusBadRequest, "MISSING_STATEMENTS", "At least one statement is required", nil)
		return
	}

	// Get database from X-Database header
	dbName := r.Header.Get("X-Database")
	exec, _, err := s.getExecutorForDatabase(dbName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DATABASE_ERROR", fmt.Sprintf("database not found: %s", dbName), nil)
		return
	}

	pretty := r.URL.Query().Get("pretty") == "true"
	start := time.Now()

	results := make([]ExecuteResult, 0, len(req.Statements))

	// Start transaction if requested
	if req.Transaction {
		l := lexer.New("BEGIN")
		p := parser.New(l)
		stmt, _ := p.Parse()
		if _, err := exec.Execute(stmt); err != nil {
			writeError(w, http.StatusInternalServerError, "TRANSACTION_ERROR", err.Error(), nil)
			return
		}
	}

	var executeErr error
	for _, stmt := range req.Statements {
		// Substitute parameters
		sql := substituteParams(stmt.SQL, stmt.Params)

		l := lexer.New(sql)
		p := parser.New(l)
		parsed, err := p.Parse()
		if err != nil {
			executeErr = err
			break
		}

		result, err := exec.Execute(parsed)
		if err != nil {
			executeErr = err
			break
		}

		results = append(results, ExecuteResult{
			RowsAffected: result.RowsAffected,
			LastInsertID: result.LastInsertID,
			LSN:          result.LSN,
		})
	}

	// Handle transaction
	if req.Transaction {
		if executeErr != nil {
			// Rollback on error
			l := lexer.New("ROLLBACK")
			p := parser.New(l)
			stmt, _ := p.Parse()
			if _, rollbackErr := exec.Execute(stmt); rollbackErr != nil {
				writeError(w, http.StatusInternalServerError, "ROLLBACK_ERROR", rollbackErr.Error(), nil)
				return
			}

			writeError(w, http.StatusBadRequest, "TRANSACTION_ERROR", executeErr.Error(), nil)
			return
		} else {
			// Commit on success
			l := lexer.New("COMMIT")
			p := parser.New(l)
			stmt, _ := p.Parse()
			if _, err := exec.Execute(stmt); err != nil {
				if errors.Is(err, storage.ErrSerialization) {
					writeError(w, http.StatusConflict, "SERIALIZATION_FAILURE", err.Error(), nil)
				} else if constraintErr := constraintViolationHTTPError(err); constraintErr != nil {
					writeError(w, constraintErr.Status, constraintErr.Code, constraintErr.Message, constraintErr.Details)
				} else {
					writeError(w, http.StatusBadRequest, "TRANSACTION_ERROR", err.Error(), nil)
				}
				return
			}
		}
	} else if executeErr != nil {
		httpErr := sqlStateHTTPError(executeErr)
		writeError(w, httpErr.Status, httpErr.Code, httpErr.Message, httpErr.Details)
		return
	}

	resp := &ExecuteResponse{
		Results:       results,
		ExecutionTime: time.Since(start).String(),
	}

	writeJSON(w, http.StatusOK, resp, pretty)
}

// handleSchemaTables handles GET /schema/tables
func (s *Server) handleSchemaTables(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only GET method is allowed", nil)
		return
	}

	// Get database from X-Database header (trim whitespace)
	dbName := strings.TrimSpace(r.Header.Get("X-Database"))

	_, schema, err := s.getExecutorForDatabase(dbName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DATABASE_ERROR", fmt.Sprintf("database not found: %s", dbName), nil)
		return
	}

	actualDB := schema.GetDatabaseName()

	tables, err := schema.ListTables()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SCHEMA_ERROR", err.Error(), nil)
		return
	}

	// Include the actual database name and requested name in the response for verification
	resp := map[string]interface{}{
		"database":           actualDB,
		"requested_database": dbName,
		"tables":             tables,
	}

	pretty := r.URL.Query().Get("pretty") == "true"
	writeJSON(w, http.StatusOK, resp, pretty)
}

// handleSchemaTable handles GET /schema/tables/{table}
func (s *Server) handleSchemaTable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only GET method is allowed", nil)
		return
	}

	// Get database from X-Database header
	dbName := r.Header.Get("X-Database")
	_, schemaManager, err := s.getExecutorForDatabase(dbName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DATABASE_ERROR", fmt.Sprintf("database not found: %s", dbName), nil)
		return
	}

	// Extract table name from path
	path := strings.TrimPrefix(r.URL.Path, "/schema/tables/")
	tableName := strings.TrimSpace(path)

	if tableName == "" {
		writeError(w, http.StatusBadRequest, "MISSING_TABLE_NAME", "Table name is required", nil)
		return
	}

	schema, err := schemaManager.GetSchema(tableName)
	if err != nil {
		writeError(w, http.StatusNotFound, "TABLE_NOT_FOUND", fmt.Sprintf("Table '%s' not found", tableName), nil)
		return
	}

	columns := make([]map[string]interface{}, len(schema.Columns))
	for i, col := range schema.Columns {
		columns[i] = map[string]interface{}{
			"name":       col.Name,
			"type":       col.Type,
			"nullable":   col.Nullable,
			"primaryKey": col.PrimaryKey,
			"default":    col.DefaultLiteral(),
		}
	}

	resp := map[string]interface{}{
		"name":    schema.Name,
		"columns": columns,
	}

	pretty := r.URL.Query().Get("pretty") == "true"
	writeJSON(w, http.StatusOK, resp, pretty)
}

// handleCapabilities serves the machine-readable capability document so clients
// can discover supported SQL surface for this engine build.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	pretty := r.URL.Query().Get("pretty") == "true"
	writeJSON(w, http.StatusOK, capabilities.Current(), pretty)
}

// handleVersion reports the build identity and the protocol compatibility
// floor separately, so clients can pin behavior to a specific engine build.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"name":            "pizzasql",
		"version":         version.String(),
		"commit":          version.Commit,
		"date":            version.Date,
		"dirty":           version.Dirty != "",
		"postgres_compat": version.PostgreSQLCompatVersion,
		"sqlite_compat":   executor.SQLiteCompatVersion,
	}
	pretty := r.URL.Query().Get("pretty") == "true"
	writeJSON(w, http.StatusOK, resp, pretty)
}

// constraintViolationHTTPError maps a row-constraint violation onto the same
// SQLSTATE response the PostgreSQL endpoint reports, and returns nil for any
// other error. Transaction endpoints use it for a deferred unique, not-null, or
// check failure at COMMIT while keeping their generic TRANSACTION_ERROR
// fallback for errors that are not constraint violations.
func constraintViolationHTTPError(err error) *HTTPError {
	var checkErr *storage.CheckViolationError
	var notNullErr *storage.NotNullViolationError
	var uniqueErr *storage.UniqueViolationError
	var primaryKeyErr *storage.PrimaryKeyViolationError
	var foreignKeyErr *storage.ForeignKeyViolationError
	switch {
	case errors.As(err, &foreignKeyErr):
		return &HTTPError{
			Status:  http.StatusConflict,
			Code:    "23503",
			Message: err.Error(),
			Details: map[string]interface{}{"constraint": foreignKeyErr.Constraint},
		}
	case errors.As(err, &checkErr):
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    "23514",
			Message: err.Error(),
			Details: map[string]interface{}{"constraint": checkErr.Constraint},
		}
	case errors.As(err, &notNullErr):
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    "23502",
			Message: err.Error(),
		}
	case errors.As(err, &uniqueErr):
		return &HTTPError{
			Status:  http.StatusConflict,
			Code:    "23505",
			Message: err.Error(),
			Details: map[string]interface{}{"constraint": uniqueErr.Constraint},
		}
	case errors.As(err, &primaryKeyErr):
		return &HTTPError{
			Status:  http.StatusConflict,
			Code:    "23505",
			Message: err.Error(),
		}
	default:
		return nil
	}
}

// sqlStateHTTPError maps engine errors onto the same SQLSTATE codes the
// PostgreSQL endpoint reports. Typed constraint violations map first; the
// executor also returns plain errors on released builds, so the
// unique-violation, not-null, and unsupported-feature markers are matched on
// their stable message prefixes as a fallback.
func sqlStateHTTPError(err error) *HTTPError {
	if err == nil {
		return nil
	}
	if typed := constraintViolationHTTPError(err); typed != nil {
		return typed
	}
	message := err.Error()
	if strings.HasPrefix(message, "UNIQUE constraint failed: ") {
		constraint := strings.TrimPrefix(message, "UNIQUE constraint failed: ")
		return &HTTPError{
			Status:  http.StatusConflict,
			Code:    "23505",
			Message: message,
			Details: map[string]interface{}{"constraint": constraint},
		}
	}
	if strings.HasPrefix(message, "NOT NULL constraint failed: ") {
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    "23502",
			Message: message,
		}
	}
	if strings.HasPrefix(message, "FOREIGN KEY constraint failed: ") {
		constraint := strings.TrimPrefix(message, "FOREIGN KEY constraint failed: ")
		return &HTTPError{
			Status:  http.StatusConflict,
			Code:    "23503",
			Message: message,
			Details: map[string]interface{}{"constraint": constraint},
		}
	}
	if strings.HasPrefix(message, "CHECK constraint failed: ") {
		constraint := strings.TrimPrefix(message, "CHECK constraint failed: ")
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    "23514",
			Message: message,
			Details: map[string]interface{}{"constraint": constraint},
		}
	}
	if errors.Is(err, storage.ErrSerialization) {
		return &HTTPError{
			Status:  http.StatusConflict,
			Code:    "40001",
			Message: message,
		}
	}
	if errors.Is(err, pgerror.ErrLockNotAvailable) {
		return &HTTPError{
			Status:  http.StatusConflict,
			Code:    pgerror.CodeLockNotAvailable,
			Message: message,
		}
	}
	if errors.Is(err, pgerror.ErrDeadlockDetected) {
		return &HTTPError{
			Status:  http.StatusConflict,
			Code:    pgerror.CodeDeadlockDetected,
			Message: message,
		}
	}
	if strings.Contains(message, "feature not supported:") {
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    "0A000",
			Message: message,
		}
	}
	if errors.Is(err, pgerror.ErrInvalidText) {
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    pgerror.CodeInvalidText,
			Message: message,
		}
	}
	if errors.Is(err, pgerror.ErrInvalidParameter) {
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    pgerror.CodeInvalidParameter,
			Message: message,
		}
	}
	if errors.Is(err, pgerror.ErrInvalidColumnReference) {
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    pgerror.CodeInvalidColumnReference,
			Message: message,
		}
	}
	var analysisErr *analyzer.AnalysisError
	if errors.Is(err, pgerror.ErrDatatypeMismatch) || (errors.As(err, &analysisErr) && analysisErr.Type == analyzer.ErrTypeMismatch) {
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    pgerror.CodeDatatypeMismatch,
			Message: message,
		}
	}
	if errors.Is(err, pgerror.ErrUndefinedTable) || (errors.As(err, &analysisErr) && analysisErr.Type == analyzer.ErrTableNotFound) {
		return &HTTPError{
			Status:  http.StatusNotFound,
			Code:    pgerror.CodeUndefinedTable,
			Message: message,
		}
	}
	return &HTTPError{
		Status:  http.StatusInternalServerError,
		Code:    "EXECUTION_ERROR",
		Message: message,
	}
}

// handleHealth handles GET /health
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"status":  "ok",
		"version": version.String(),
		"uptime":  time.Since(s.stats.StartTime).String(),
	}

	pretty := r.URL.Query().Get("pretty") == "true"
	writeJSON(w, http.StatusOK, resp, pretty)
}

// handleStats handles GET /stats
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	// Get database from X-Database header
	dbName := r.Header.Get("X-Database")
	_, schema, _ := s.getExecutorForDatabase(dbName)

	var tables []string
	if schema != nil {
		tables, _ = schema.ListTables()
	}

	var avgQueryTime string
	if s.stats.QueriesExecuted > 0 {
		avgQueryTime = "N/A" // TODO: Track actual query times
	} else {
		avgQueryTime = "0ms"
	}

	resp := map[string]interface{}{
		"queriesExecuted": atomic.LoadInt64(&s.stats.QueriesExecuted),
		"queriesSuccess":  atomic.LoadInt64(&s.stats.QueriesSuccess),
		"queriesError":    atomic.LoadInt64(&s.stats.QueriesError),
		"tablesCount":     len(tables),
		"avgQueryTime":    avgQueryTime,
		"uptime":          time.Since(s.stats.StartTime).String(),
	}

	pretty := r.URL.Query().Get("pretty") == "true"
	writeJSON(w, http.StatusOK, resp, pretty)
}

// handleTransactionBegin handles POST /transaction/begin
func (s *Server) handleTransactionBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only POST method is allowed", nil)
		return
	}

	// Get database from X-Database header
	dbName := r.Header.Get("X-Database")
	txID, _, err := s.beginTransaction(dbName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "TRANSACTION_ERROR", err.Error(), nil)
		return
	}

	resp := map[string]interface{}{
		"transactionId": txID,
		"status":        "started",
	}

	pretty := r.URL.Query().Get("pretty") == "true"
	writeJSON(w, http.StatusOK, resp, pretty)
}

// handleTransactionCommit handles POST /transaction/commit
func (s *Server) handleTransactionCommit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only POST method is allowed", nil)
		return
	}

	var req TransactionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Invalid JSON in request body", nil)
		return
	}
	tx, ok := s.takeTransactionExecutor(req.TransactionID, r.Header.Get("X-Database"))
	if !ok {
		writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "unknown transaction ID", nil)
		return
	}
	defer tx.mu.Unlock()

	l := lexer.New("COMMIT")
	p := parser.New(l)
	stmt, _ := p.Parse()
	if _, err := tx.exec.Execute(stmt); err != nil {
		if errors.Is(err, storage.ErrSerialization) {
			writeError(w, http.StatusConflict, "SERIALIZATION_FAILURE", err.Error(), nil)
		} else if constraintErr := constraintViolationHTTPError(err); constraintErr != nil {
			writeError(w, constraintErr.Status, constraintErr.Code, constraintErr.Message, constraintErr.Details)
		} else {
			writeError(w, http.StatusBadRequest, "TRANSACTION_ERROR", err.Error(), nil)
		}
		return
	}

	resp := map[string]interface{}{
		"status": "committed",
	}

	pretty := r.URL.Query().Get("pretty") == "true"
	writeJSON(w, http.StatusOK, resp, pretty)
}

// substituteParams replaces ? placeholders with actual parameter values. It
// scans the original SQL once, skipping quoted strings, quoted identifiers, and
// comments, so a question mark inside a bound value or a literal is never
// mistaken for the next placeholder.
func substituteParams(sql string, params []interface{}) string {
	if len(params) == 0 {
		return sql
	}
	var b strings.Builder
	b.Grow(len(sql) + 32)
	param := 0
	for i := 0; i < len(sql); {
		switch sql[i] {
		case '\'':
			j := i + 1
			for j < len(sql) {
				if sql[j] == '\'' {
					if j+1 < len(sql) && sql[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			b.WriteString(sql[i:j])
			i = j
		case '"':
			j := i + 1
			for j < len(sql) {
				if sql[j] == '"' {
					if j+1 < len(sql) && sql[j+1] == '"' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			b.WriteString(sql[i:j])
			i = j
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				j := i + 2
				for j < len(sql) && sql[j] != '\n' {
					j++
				}
				b.WriteString(sql[i:j])
				i = j
				continue
			}
			b.WriteByte(sql[i])
			i++
		case '/':
			if i+1 < len(sql) && sql[i+1] == '*' {
				j := i + 2
				for j+1 < len(sql) && !(sql[j] == '*' && sql[j+1] == '/') {
					j++
				}
				if j+1 < len(sql) {
					j += 2
				}
				b.WriteString(sql[i:j])
				i = j
				continue
			}
			b.WriteByte(sql[i])
			i++
		case '?':
			if param < len(params) {
				b.WriteString(formatParam(params[param]))
				param++
			} else {
				b.WriteByte('?')
			}
			i++
		default:
			b.WriteByte(sql[i])
			i++
		}
	}
	return b.String()
}

func formatParam(param interface{}) string {
	switch v := param.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case int, int64, int32, int16, int8:
		return fmt.Sprintf("%d", v)
	case float64, float32:
		return fmt.Sprintf("%g", v)
	case bool:
		if v {
			return "1"
		}
		return "0"
	default:
		return "'" + strings.ReplaceAll(fmt.Sprintf("%v", v), "'", "''") + "'"
	}
}

// inferType infers SQL type from a Go value.
func inferType(v interface{}) string {
	switch v.(type) {
	case nil:
		return "NULL"
	case int, int64, int32, int16, int8:
		return "INTEGER"
	case float64, float32:
		return "REAL"
	case string:
		return "TEXT"
	case []byte:
		return "BLOB"
	case bool:
		return "INTEGER"
	default:
		return "ANY"
	}
}

// handleTransactionRollback handles POST /transaction/rollback
func (s *Server) handleTransactionRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only POST method is allowed", nil)
		return
	}

	var req TransactionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Invalid JSON in request body", nil)
		return
	}
	tx, ok := s.takeTransactionExecutor(req.TransactionID, r.Header.Get("X-Database"))
	if !ok {
		writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "unknown transaction ID", nil)
		return
	}
	defer tx.mu.Unlock()

	l := lexer.New("ROLLBACK")
	p := parser.New(l)
	stmt, _ := p.Parse()
	if _, err := tx.exec.Execute(stmt); err != nil {
		writeError(w, http.StatusBadRequest, "TRANSACTION_ERROR", err.Error(), nil)
		return
	}

	resp := map[string]interface{}{
		"status": "rolled back",
	}

	pretty := r.URL.Query().Get("pretty") == "true"
	writeJSON(w, http.StatusOK, resp, pretty)
}

// handleMetrics handles GET /metrics in Prometheus format
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only GET method is allowed", nil)
		return
	}

	// Get database from X-Database header
	dbName := r.Header.Get("X-Database")
	_, schema, _ := s.getExecutorForDatabase(dbName)

	var tables []string
	if schema != nil {
		tables, _ = schema.ListTables()
	}

	uptime := time.Since(s.stats.StartTime).Seconds()

	queriesTotal := atomic.LoadInt64(&s.stats.QueriesExecuted)
	queriesSuccess := atomic.LoadInt64(&s.stats.QueriesSuccess)
	queriesError := atomic.LoadInt64(&s.stats.QueriesError)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Write Prometheus format metrics
	fmt.Fprintf(w, "# HELP pizzasql_queries_total Total number of queries executed\n")
	fmt.Fprintf(w, "# TYPE pizzasql_queries_total counter\n")
	fmt.Fprintf(w, "pizzasql_queries_total{status=\"success\"} %d\n", queriesSuccess)
	fmt.Fprintf(w, "pizzasql_queries_total{status=\"error\"} %d\n", queriesError)
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP pizzasql_queries_executed_total Total queries executed (all statuses)\n")
	fmt.Fprintf(w, "# TYPE pizzasql_queries_executed_total counter\n")
	fmt.Fprintf(w, "pizzasql_queries_executed_total %d\n", queriesTotal)
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP pizzasql_tables_count Number of tables in the database\n")
	fmt.Fprintf(w, "# TYPE pizzasql_tables_count gauge\n")
	fmt.Fprintf(w, "pizzasql_tables_count %d\n", len(tables))
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP pizzasql_uptime_seconds Server uptime in seconds\n")
	fmt.Fprintf(w, "# TYPE pizzasql_uptime_seconds gauge\n")
	fmt.Fprintf(w, "pizzasql_uptime_seconds %.2f\n", uptime)
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP pizzasql_info PizzaSQL server information\n")
	fmt.Fprintf(w, "# TYPE pizzasql_info gauge\n")
	fmt.Fprintf(w, "pizzasql_info{version=\"%s\"} 1\n", version.PrometheusLabel())
}

// handleExport handles GET /export
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only GET method is allowed", nil)
		return
	}

	// Get database from X-Database header
	dbName := strings.TrimSpace(r.Header.Get("X-Database"))
	_, schema, err := s.getExecutorForDatabase(dbName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DATABASE_ERROR", fmt.Sprintf("database not found: %s", dbName), nil)
		return
	}

	// Get the table manager from the database instance
	dbInstance, err := s.dbManager.GetDatabase(dbName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DATABASE_ERROR", fmt.Sprintf("database not found: %s", dbName), nil)
		return
	}

	// Get format parameter (default: sql)
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "sql"
	}

	tableName := r.URL.Query().Get("table")

	switch format {
	case "csv":
		// CSV export requires a single table
		if tableName == "" {
			writeError(w, http.StatusBadRequest, "TABLE_REQUIRED", "CSV export requires 'table' parameter", nil)
			return
		}

		csvOpts := csvexport.DefaultExportOptions()
		csvOpts.Table = tableName

		data, err := csvexport.ExportTableToBytes(schema, dbInstance.Table, csvOpts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "EXPORT_ERROR", err.Error(), nil)
			return
		}

		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.csv\"", tableName))
		w.WriteHeader(http.StatusOK)
		w.Write(data)

	case "sql", "sqlite":
		// SQL export (also used for SQLite-compatible export)
		opts := sqlexport.DefaultExportOptions()

		// Specific table(s) to export
		if tableName != "" {
			opts.Tables = strings.Split(tableName, ",")
		}

		// Include data (default: true)
		if r.URL.Query().Get("schema_only") == "true" {
			opts.IncludeData = false
		}

		// Include DROP TABLE statements
		if r.URL.Query().Get("drop") == "true" {
			opts.DropTables = true
		}

		// Generate SQL export
		sql, err := sqlexport.ExportDatabase(schema, dbInstance.Table, opts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "EXPORT_ERROR", err.Error(), nil)
			return
		}

		// Determine filename and content type
		ext := "sql"
		contentType := "application/sql"
		if format == "sqlite" {
			ext = "sql" // Still SQL text, but sqlite-compatible
		}

		filename := schema.GetDatabaseName() + "_export." + ext
		if len(opts.Tables) == 1 {
			filename = opts.Tables[0] + "_export." + ext
		}

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sql))

	default:
		writeError(w, http.StatusBadRequest, "INVALID_FORMAT",
			fmt.Sprintf("Invalid format '%s'. Supported formats: sql, csv", format), nil)
	}
}

// handleImport handles POST /import
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only POST method is allowed", nil)
		return
	}

	// Get database from X-Database header
	dbName := strings.TrimSpace(r.Header.Get("X-Database"))
	exec, schema, err := s.getExecutorForDatabase(dbName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DATABASE_ERROR", fmt.Sprintf("database not found: %s", dbName), nil)
		return
	}

	// Get the table manager from the database instance
	dbInstance, err := s.dbManager.GetDatabase(dbName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DATABASE_ERROR", fmt.Sprintf("database not found: %s", dbName), nil)
		return
	}

	// Get format parameter (default: sql, can be auto-detected)
	format := strings.ToLower(r.URL.Query().Get("format"))
	tableName := r.URL.Query().Get("table")
	ignoreErrors := r.URL.Query().Get("ignore_errors") == "true"
	createTable := r.URL.Query().Get("create_table") == "true"
	pretty := r.URL.Query().Get("pretty") == "true"

	// Check content type
	contentType := r.Header.Get("Content-Type")

	var fileContent []byte
	var filename string

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// Handle file upload
		if err := r.ParseMultipartForm(32 << 20); err != nil { // 32MB max
			writeError(w, http.StatusBadRequest, "INVALID_FORM", "Failed to parse multipart form: "+err.Error(), nil)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "MISSING_FILE", "No file uploaded. Use 'file' field name.", nil)
			return
		}
		defer file.Close()
		filename = header.Filename

		// Read file content
		fileContent, err = io.ReadAll(file)
		if err != nil {
			writeError(w, http.StatusBadRequest, "READ_ERROR", "Failed to read uploaded file: "+err.Error(), nil)
			return
		}

	} else if strings.HasPrefix(contentType, "application/json") {
		// Handle JSON body with SQL content
		var req struct {
			SQL string `json:"sql"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_JSON", "Invalid JSON in request body", nil)
			return
		}
		fileContent = []byte(req.SQL)

	} else if strings.HasPrefix(contentType, "text/plain") || strings.HasPrefix(contentType, "application/sql") || strings.HasPrefix(contentType, "text/csv") {
		// Handle raw content in body
		var err error
		fileContent, err = io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "READ_ERROR", "Failed to read request body: "+err.Error(), nil)
			return
		}
		// Auto-detect CSV from content type
		if strings.HasPrefix(contentType, "text/csv") && format == "" {
			format = "csv"
		}

	} else {
		writeError(w, http.StatusBadRequest, "INVALID_CONTENT_TYPE",
			"Content-Type must be multipart/form-data, application/json, text/plain, text/csv, or application/sql", nil)
		return
	}

	if len(fileContent) == 0 {
		writeError(w, http.StatusBadRequest, "EMPTY_CONTENT", "No content provided", nil)
		return
	}

	// Auto-detect format from filename extension if not specified
	if format == "" && filename != "" {
		lower := strings.ToLower(filename)
		if strings.HasSuffix(lower, ".csv") {
			format = "csv"
		} else if strings.HasSuffix(lower, ".db") || strings.HasSuffix(lower, ".sqlite") || strings.HasSuffix(lower, ".sqlite3") {
			format = "sqlite"
		}
	}
	// Detect binary SQLite from content-type
	if format == "" {
		ct := strings.ToLower(contentType)
		if strings.Contains(ct, "application/x-sqlite3") || strings.Contains(ct, "application/octet-stream") {
			format = "sqlite"
		}
	}
	// Check magic bytes: SQLite files start with "SQLite format 3\000"
	if format == "" && len(fileContent) >= 16 && string(fileContent[:15]) == "SQLite format 3" {
		format = "sqlite"
	}
	if format == "" {
		format = "sql"
	}

	switch format {
	case "csv":
		// CSV import requires table name
		if tableName == "" {
			writeError(w, http.StatusBadRequest, "TABLE_REQUIRED", "CSV import requires 'table' parameter", nil)
			return
		}

		csvOpts := csvimport.DefaultImportOptions()
		csvOpts.TableName = tableName
		csvOpts.IgnoreErrors = ignoreErrors
		csvOpts.CreateTable = createTable

		result, err := csvimport.ImportCSV(strings.NewReader(string(fileContent)), schema, dbInstance.Table, csvOpts)
		if err != nil && !ignoreErrors {
			writeError(w, http.StatusBadRequest, "IMPORT_ERROR", err.Error(), map[string]interface{}{
				"rowsImported": result.RowsImported,
				"rowsSkipped":  result.RowsSkipped,
				"tableCreated": result.TableCreated,
				"errors":       result.Errors,
			})
			return
		}

		// Sync catalog after import
		exec.SyncCatalog()

		writeJSON(w, http.StatusOK, result, pretty)

	case "sqlite":
		// Binary SQLite .db import
		opts := sqliteimport.DefaultImportOptions()
		opts.CreateTables = r.URL.Query().Get("create_tables") != "false"
		opts.IgnoreErrors = ignoreErrors

		result, err := sqliteimport.ImportSQLiteBytes(fileContent, exec, opts)
		if err != nil && !ignoreErrors {
			writeError(w, http.StatusBadRequest, "IMPORT_ERROR", err.Error(), map[string]interface{}{
				"tablesCreated": result.TablesCreated,
				"rowsInserted":  result.RowsInserted,
				"errors":        result.Errors,
			})
			return
		}

		exec.SyncCatalog()
		writeJSON(w, http.StatusOK, result, pretty)

	case "sql":
		// SQL text import
		opts := sqlimport.DefaultImportOptions()
		opts.IgnoreErrors = ignoreErrors

		result, err := sqlimport.ImportSQL(exec, string(fileContent), opts)
		if err != nil && !ignoreErrors {
			writeError(w, http.StatusBadRequest, "IMPORT_ERROR", err.Error(), map[string]interface{}{
				"statementsExecuted": result.StatementsExecuted,
				"tablesCreated":      result.TablesCreated,
				"rowsInserted":       result.RowsInserted,
				"errors":             result.Errors,
			})
			return
		}

		// Sync catalog after import
		exec.SyncCatalog()

		writeJSON(w, http.StatusOK, result, pretty)

	default:
		writeError(w, http.StatusBadRequest, "INVALID_FORMAT",
			fmt.Sprintf("Invalid format '%s'. Supported formats: sql, csv", format), nil)
	}
}
