package sqlexport

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// ExportOptions configures export behavior.
type ExportOptions struct {
	Tables      []string // Specific tables to export (empty = all tables)
	IncludeData bool     // Include INSERT statements (default: true)
	DropTables  bool     // Add DROP TABLE IF EXISTS before CREATE
}

// DefaultExportOptions returns sensible defaults.
func DefaultExportOptions() ExportOptions {
	return ExportOptions{
		Tables:      nil,
		IncludeData: true,
		DropTables:  false,
	}
}

// ExportDatabase exports an entire database to SQL text.
func ExportDatabase(schema *storage.SchemaManager, table *storage.TableManager, opts ExportOptions) (string, error) {
	var sb strings.Builder

	// Write header
	sb.WriteString("-- PizzaSQL Export\n")
	sb.WriteString(fmt.Sprintf("-- Database: %s\n", schema.GetDatabaseName()))
	sb.WriteString(fmt.Sprintf("-- Date: %s\n", time.Now().UTC().Format(time.RFC3339)))
	sb.WriteString("\n")

	// Get tables to export
	tables := opts.Tables
	if len(tables) == 0 {
		var err error
		tables, err = schema.ListTables()
		if err != nil {
			return "", fmt.Errorf("failed to list tables: %w", err)
		}
	}

	// Sort tables for consistent output
	sort.Strings(tables)

	// Export each table
	for i, tableName := range tables {
		tableSQL, err := ExportTable(schema, table, tableName, opts)
		if err != nil {
			return "", fmt.Errorf("failed to export table %s: %w", tableName, err)
		}

		sb.WriteString(tableSQL)

		// Add separator between tables
		if i < len(tables)-1 {
			sb.WriteString("\n")
		}
	}

	return sb.String(), nil
}

// ExportTable exports a single table to SQL text.
func ExportTable(schema *storage.SchemaManager, table *storage.TableManager, tableName string, opts ExportOptions) (string, error) {
	var sb strings.Builder

	// Get table schema
	tableSchema, err := schema.GetSchema(tableName)
	if err != nil {
		return "", fmt.Errorf("failed to get schema for table %s: %w", tableName, err)
	}

	// Write DROP TABLE if requested
	if opts.DropTables {
		sb.WriteString(fmt.Sprintf("DROP TABLE IF EXISTS %s;\n", quoteIdentifier(tableName)))
	}

	// Write CREATE TABLE
	createSQL := generateCreateTable(tableSchema)
	sb.WriteString(createSQL)
	sb.WriteString("\n")

	// Write INSERT statements if requested
	if opts.IncludeData {
		rows, err := table.Select(tableName, nil)
		if err != nil {
			return "", fmt.Errorf("failed to select data from table %s: %w", tableName, err)
		}

		if len(rows) > 0 {
			sb.WriteString("\n")
			insertSQL := generateInserts(tableName, tableSchema, rows)
			sb.WriteString(insertSQL)
		}
	}

	return sb.String(), nil
}

// generateCreateTable generates a CREATE TABLE statement from schema.
func generateCreateTable(schema *storage.Schema) string {
	compositeKey := schema.IsCompositePrimaryKey()
	defs := make([]string, 0, len(schema.Columns)+1)
	for _, col := range schema.Columns {
		// Skip internal _rowid_ column
		if col.Name == "_rowid_" {
			continue
		}

		var b strings.Builder
		b.WriteString(quoteIdentifier(col.Name))
		b.WriteString(" ")
		b.WriteString(col.Type)

		// A single-column key keeps its inline PRIMARY KEY; a composite key is
		// emitted once at the end so no column claims the key by itself.
		inlineKey := col.PrimaryKey && !compositeKey
		if inlineKey {
			b.WriteString(" PRIMARY KEY")
		}

		if !col.Nullable && !inlineKey {
			b.WriteString(" NOT NULL")
		}

		// Add DEFAULT value. A non-constant default is emitted as its stored SQL
		// text so the dump keeps evaluating it instead of freezing a value.
		if col.DefaultExpr != "" {
			b.WriteString(" DEFAULT ")
			b.WriteString(col.DefaultExpr)
		} else if col.Default != nil {
			b.WriteString(" DEFAULT ")
			b.WriteString(formatValue(col.Default, col.Type))
		}

		defs = append(defs, "    "+b.String())
	}
	if compositeKey {
		columns := schema.PrimaryKeyList()
		quoted := make([]string, len(columns))
		for i, name := range columns {
			quoted[i] = quoteIdentifier(name)
		}
		defs = append(defs, "    PRIMARY KEY ("+strings.Join(quoted, ", ")+")")
	}

	return fmt.Sprintf("CREATE TABLE %s (\n%s\n);", quoteIdentifier(schema.Name), strings.Join(defs, ",\n"))
}

// generateInserts generates INSERT statements for rows.
func generateInserts(tableName string, schema *storage.Schema, rows []storage.Row) string {
	var sb strings.Builder

	// Get column names (excluding _rowid_)
	var columns []string
	var colTypes []string
	for _, col := range schema.Columns {
		if col.Name != "_rowid_" {
			columns = append(columns, col.Name)
			colTypes = append(colTypes, col.Type)
		}
	}

	// Generate INSERT for each row
	for _, row := range rows {
		sb.WriteString(fmt.Sprintf("INSERT INTO %s (", quoteIdentifier(tableName)))

		// Column names
		for i, col := range columns {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(quoteIdentifier(col))
		}

		sb.WriteString(") VALUES (")

		// Values
		for i, col := range columns {
			if i > 0 {
				sb.WriteString(", ")
			}
			value := row[col]
			sb.WriteString(formatValue(value, colTypes[i]))
		}

		sb.WriteString(");\n")
	}

	return sb.String()
}

// formatValue formats a Go value as a SQL literal.
func formatValue(value interface{}, colType string) string {
	if value == nil {
		return "NULL"
	}

	switch v := value.(type) {
	case string:
		return formatString(v)
	case float64:
		// Check if it's actually an integer
		if strings.Contains(strings.ToUpper(colType), "INT") {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%g", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case int:
		return fmt.Sprintf("%d", v)
	case bool:
		if v {
			return "1"
		}
		return "0"
	case []byte:
		return fmt.Sprintf("X'%s'", hex.EncodeToString(v))
	default:
		// Fallback: treat as string
		return formatString(fmt.Sprintf("%v", v))
	}
}

// formatString formats a string as a SQL string literal with proper escaping.
func formatString(s string) string {
	// Escape single quotes by doubling them
	escaped := strings.ReplaceAll(s, "'", "''")
	return fmt.Sprintf("'%s'", escaped)
}

// quoteIdentifier quotes a SQL identifier if needed.
func quoteIdentifier(name string) string {
	// Check if identifier needs quoting
	needsQuote := false

	// Check for reserved words or special characters
	lower := strings.ToLower(name)
	reserved := map[string]bool{
		"table": true, "select": true, "insert": true, "update": true,
		"delete": true, "create": true, "drop": true, "index": true,
		"from": true, "where": true, "and": true, "or": true,
		"order": true, "by": true, "group": true, "having": true,
		"limit": true, "offset": true, "join": true, "on": true,
		"as": true, "null": true, "not": true, "in": true,
		"like": true, "between": true, "is": true, "primary": true,
		"key": true, "unique": true, "default": true, "values": true,
	}

	if reserved[lower] {
		needsQuote = true
	}

	// Check for special characters
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_') {
			needsQuote = true
			break
		}
	}

	// Check if starts with digit
	if len(name) > 0 && name[0] >= '0' && name[0] <= '9' {
		needsQuote = true
	}

	if needsQuote {
		// Use double quotes and escape any existing double quotes
		escaped := strings.ReplaceAll(name, "\"", "\"\"")
		return fmt.Sprintf("\"%s\"", escaped)
	}

	return name
}
