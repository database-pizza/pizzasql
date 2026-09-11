package sqliteimport

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	_ "modernc.org/sqlite"
)

const insertBatchSize = 500

// ImportOptions configures SQLite import behavior.
type ImportOptions struct {
	CreateTables bool     // Create tables from the source schema (default true)
	IgnoreErrors bool     // Continue on individual row/statement errors
	TableFilter  []string // If non-empty, only import these tables
}

// DefaultImportOptions returns sensible defaults.
func DefaultImportOptions() ImportOptions {
	return ImportOptions{
		CreateTables: true,
		IgnoreErrors: false,
	}
}

// ImportResult contains the results of an import operation.
type ImportResult struct {
	TablesCreated  []string `json:"tablesCreated"`
	TablesImported []string `json:"tablesImported"`
	RowsInserted   int64    `json:"rowsInserted"`
	IndexesCreated int      `json:"indexesCreated"`
	Errors         []string `json:"errors,omitempty"`
}

// ImportSQLiteFile imports a SQLite .db file into a PizzaSQL executor.
func ImportSQLiteFile(path string, exec *executor.Executor, opts ImportOptions) (*ImportResult, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open sqlite file: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("cannot read sqlite file: %w", err)
	}

	return importFromDB(db, exec, opts)
}

// ImportSQLiteBytes imports a SQLite database from raw bytes (e.g. from an HTTP upload).
// It writes to a temporary file, imports, then removes the file.
func ImportSQLiteBytes(data []byte, exec *executor.Executor, opts ImportOptions) (*ImportResult, error) {
	tmp, err := os.CreateTemp("", "pizzasql-sqlite-*.db")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write temp file: %w", err)
	}
	tmp.Close()

	return ImportSQLiteFile(tmpPath, exec, opts)
}

func importFromDB(db *sql.DB, exec *executor.Executor, opts ImportOptions) (*ImportResult, error) {
	result := &ImportResult{
		TablesCreated:  []string{},
		TablesImported: []string{},
		Errors:         []string{},
	}

	// Load table list and DDL from sqlite_master
	type tableEntry struct {
		name string
		ddl  string
	}
	var tables []tableEntry

	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("query sqlite_master: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var ddlNull sql.NullString
		if err := rows.Scan(&name, &ddlNull); err != nil {
			continue
		}
		if !ddlNull.Valid || ddlNull.String == "" {
			continue
		}
		tables = append(tables, tableEntry{name: name, ddl: ddlNull.String})
	}
	rows.Close()

	// Apply table filter
	if len(opts.TableFilter) > 0 {
		filter := make(map[string]bool, len(opts.TableFilter))
		for _, t := range opts.TableFilter {
			filter[strings.ToLower(t)] = true
		}
		filtered := tables[:0]
		for _, t := range tables {
			if filter[strings.ToLower(t.name)] {
				filtered = append(filtered, t)
			}
		}
		tables = filtered
	}

	// Create tables
	if opts.CreateTables {
		for _, t := range tables {
			ddl := sanitizeDDL(t.ddl)
			if err := execStatement(exec, ddl); err != nil {
				msg := fmt.Sprintf("create table %s: %v", t.name, err)
				result.Errors = append(result.Errors, msg)
				if !opts.IgnoreErrors {
					return result, fmt.Errorf("%s", msg)
				}
				continue
			}
			result.TablesCreated = append(result.TablesCreated, t.name)
		}
	}

	// Import indexes
	idxRows, err := db.Query(`SELECT sql FROM sqlite_master WHERE type='index' AND sql IS NOT NULL AND name NOT LIKE 'sqlite_%'`)
	if err == nil {
		defer idxRows.Close()
		for idxRows.Next() {
			var idxSQL string
			if err := idxRows.Scan(&idxSQL); err != nil {
				continue
			}
			idxSQL = sanitizeDDL(idxSQL)
			if err := execStatement(exec, idxSQL); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("create index: %v", err))
			} else {
				result.IndexesCreated++
			}
		}
		idxRows.Close()
	}

	// Import rows per table
	for _, t := range tables {
		n, err := importTableRows(db, exec, t.name, opts.IgnoreErrors)
		if err != nil {
			msg := fmt.Sprintf("import rows for %s: %v", t.name, err)
			result.Errors = append(result.Errors, msg)
			if !opts.IgnoreErrors {
				return result, fmt.Errorf("%s", msg)
			}
			continue
		}
		result.TablesImported = append(result.TablesImported, t.name)
		result.RowsInserted += n
	}

	return result, nil
}

func importTableRows(db *sql.DB, exec *executor.Executor, table string, ignoreErrors bool) (int64, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT * FROM %q`, table))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	if len(cols) == 0 {
		return 0, nil
	}

	var total int64
	var batch []string

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		// Build multi-row INSERT
		colList := quoteIdentList(cols)
		sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s",
			quoteIdent(table), colList, strings.Join(batch, ", "))
		if err := execStatement(exec, sql); err != nil {
			return err
		}
		total += int64(len(batch))
		batch = batch[:0]
		return nil
	}

	vals := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}

	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			if ignoreErrors {
				continue
			}
			return total, err
		}

		batch = append(batch, rowToValueList(vals))

		if len(batch) >= insertBatchSize {
			if err := flush(); err != nil {
				if ignoreErrors {
					batch = batch[:0]
					continue
				}
				return total, err
			}
		}
	}

	if err := rows.Err(); err != nil {
		return total, err
	}

	if err := flush(); err != nil {
		return total, err
	}

	return total, nil
}

// rowToValueList converts a row of Go values into a SQL VALUES tuple string.
func rowToValueList(vals []interface{}) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = sqlLiteral(v)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// sqlLiteral converts a Go value (from the sqlite driver) to a SQL literal string.
func sqlLiteral(v interface{}) string {
	if v == nil {
		return "NULL"
	}
	switch val := v.(type) {
	case int64:
		return fmt.Sprintf("%d", val)
	case float64:
		return fmt.Sprintf("%g", val)
	case string:
		return "'" + strings.ReplaceAll(val, "'", "''") + "'"
	case []byte:
		// Store blobs as hex text strings (PizzaSQL has no X'' literal support)
		return "'" + hex.EncodeToString(val) + "'"
	case bool:
		if val {
			return "1"
		}
		return "0"
	default:
		s := fmt.Sprintf("%v", val)
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteIdentList(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = quoteIdent(c)
	}
	return strings.Join(parts, ", ")
}

func execStatement(exec *executor.Executor, sql string) error {
	l := lexer.New(sql)
	p := parser.New(l)
	stmt, err := p.Parse()
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	_, err = exec.Execute(stmt)
	return err
}

// pizzasqlReservedKeywords is the set of tokens that PizzaSQL reserves but are
// commonly used as column names in SQLite schemas.
var pizzasqlReservedKeywords = map[string]bool{
	"key": true, "value": true, "type": true, "name": true,
	"index": true, "view": true, "table": true, "column": true,
	"group": true, "order": true, "range": true, "match": true,
}

var (
	reAutoincrement = regexp.MustCompile(`(?i)\bAUTOINCREMENT\b`)
	reWithoutRowid  = regexp.MustCompile(`(?i)\bWITHOUT\s+ROWID\b`)
	reStrict        = regexp.MustCompile(`(?i),?\s*\bSTRICT\b`)
	// REFERENCES x(y) ON DELETE/UPDATE action — strip whole inline FK clause.
	// Use \w+ (not \S+) so the trailing comma of the column is preserved.
	reInlineRefs = regexp.MustCompile(`(?i)\bREFERENCES\s+\w+\s*(?:\([^)]*\))?\s*(?:(?:ON\s+(?:DELETE|UPDATE)\s+(?:CASCADE|SET\s+NULL|SET\s+DEFAULT|RESTRICT|NO\s+ACTION))\s*)*`)
	// Table-level FOREIGN KEY constraint lines
	reTableFK = regexp.MustCompile(`(?i),?\s*FOREIGN\s+KEY\s*\([^)]*\)\s*REFERENCES\s+\w+\s*(?:\([^)]*\))?\s*(?:(?:ON\s+(?:DELETE|UPDATE)\s+(?:CASCADE|SET\s+NULL|SET\s+DEFAULT|RESTRICT|NO\s+ACTION))\s*)*`)
	// Table-level CHECK constraints
	reTableCheck = regexp.MustCompile(`(?i),?\s*CHECK\s*\([^)]*\)`)
	reOnConflict = regexp.MustCompile(`(?i)\bON\s+CONFLICT\s+\w+`)
	// Complex DEFAULT expressions: DEFAULT (...) — strip entirely, keep no default
	reComplexDefault = regexp.MustCompile(`(?i)\bDEFAULT\s*\([^)]*\)`)
	// Trailing comma before closing paren
	reTableTrailing = regexp.MustCompile(`(?m),\s*\)`)
	// DESC/ASC in index column lists
	reIndexColOrder = regexp.MustCompile(`(?i)\b(ASC|DESC)\b`)
	// Column name (first word) followed by a type keyword on each column line
	reColumnName = regexp.MustCompile(`(?m)^\s{1,}(\w+)(\s+)`)
)

// sanitizeDDL strips SQLite-specific clauses that PizzaSQL doesn't support.
func sanitizeDDL(ddl string) string {
	ddl = reAutoincrement.ReplaceAllString(ddl, "")
	ddl = reWithoutRowid.ReplaceAllString(ddl, "")
	ddl = reStrict.ReplaceAllString(ddl, "")
	ddl = reTableFK.ReplaceAllString(ddl, "")
	ddl = reTableCheck.ReplaceAllString(ddl, "")
	ddl = reComplexDefault.ReplaceAllString(ddl, "")
	ddl = reInlineRefs.ReplaceAllString(ddl, "")
	ddl = reOnConflict.ReplaceAllString(ddl, "")

	// Strip ASC/DESC from index column lists
	upper := strings.ToUpper(ddl)
	if strings.Contains(upper, "CREATE INDEX") || strings.Contains(upper, "CREATE UNIQUE INDEX") {
		ddl = reIndexColOrder.ReplaceAllString(ddl, "")
	}

	// Quote column names that clash with PizzaSQL reserved keywords
	ddl = reColumnName.ReplaceAllStringFunc(ddl, func(m string) string {
		// Extract leading whitespace, word, trailing whitespace
		sub := reColumnName.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		word, ws := sub[1], sub[2]
		if pizzasqlReservedKeywords[strings.ToLower(word)] {
			leading := m[:len(m)-len(word)-len(ws)]
			return leading + `"` + word + `"` + ws
		}
		return m
	})

	// Clean up trailing commas before closing paren
	ddl = reTableTrailing.ReplaceAllStringFunc(ddl, func(s string) string {
		return ")"
	})
	return strings.TrimSpace(ddl)
}
