package executor

import (
	"fmt"
	"strings"
)

// Result represents the result of executing a SQL statement.
type Result struct {
	Columns      []string        // Column names
	ColumnTypes  []string        // Column types (for type inference)
	Rows         [][]interface{} // Row data
	RowCount     int             // Number of affected/returned rows
	RowsAffected int64           // Number of rows affected (for INSERT/UPDATE/DELETE)
	LastInsertID int64           // Last insert ID (for INSERT with AUTOINCREMENT)
	CommandTag   string          // Command type (SELECT, INSERT, UPDATE, DELETE, etc.)
}

// NewResult creates a new empty result.
func NewResult(tag string) *Result {
	return &Result{
		CommandTag: tag,
		Rows:       make([][]interface{}, 0),
	}
}

// AddColumn adds a column to the result.
func (r *Result) AddColumn(name string) {
	r.Columns = append(r.Columns, name)
}

// AddRow adds a row to the result.
func (r *Result) AddRow(values ...interface{}) {
	for i, v := range values {
		if jt, ok := v.(jsonText); ok {
			values[i] = string(jt)
		}
	}
	r.Rows = append(r.Rows, values)
	r.RowCount = len(r.Rows)
}

// SetRowCount sets the row count (for non-SELECT queries).
func (r *Result) SetRowCount(count int) {
	r.RowCount = count
	r.RowsAffected = int64(count)
}

// SetLastInsertID sets the last insert ID.
func (r *Result) SetLastInsertID(id int64) {
	r.LastInsertID = id
}

// AddColumnWithType adds a column with its type to the result.
func (r *Result) AddColumnWithType(name, colType string) {
	r.Columns = append(r.Columns, name)
	r.ColumnTypes = append(r.ColumnTypes, colType)
}

// GetColumnType returns the type for a column index.
func (r *Result) GetColumnType(idx int) string {
	if idx < len(r.ColumnTypes) {
		return r.ColumnTypes[idx]
	}
	return "ANY"
}

// String returns a string representation of the result.
func (r *Result) String() string {
	var sb strings.Builder

	if len(r.Columns) > 0 {
		// Calculate column widths
		widths := make([]int, len(r.Columns))
		for i, col := range r.Columns {
			widths[i] = len(col)
		}
		for _, row := range r.Rows {
			for i, val := range row {
				if i < len(widths) {
					w := len(fmt.Sprintf("%v", val))
					if w > widths[i] {
						widths[i] = w
					}
				}
			}
		}

		// Print header
		for i, col := range r.Columns {
			if i > 0 {
				sb.WriteString(" | ")
			}
			sb.WriteString(padRight(col, widths[i]))
		}
		sb.WriteString("\n")

		// Print separator
		for i, w := range widths {
			if i > 0 {
				sb.WriteString("-+-")
			}
			sb.WriteString(strings.Repeat("-", w))
		}
		sb.WriteString("\n")

		// Print rows
		for _, row := range r.Rows {
			for i, val := range row {
				if i > 0 {
					sb.WriteString(" | ")
				}
				if i < len(widths) {
					sb.WriteString(padRight(fmt.Sprintf("%v", val), widths[i]))
				}
			}
			sb.WriteString("\n")
		}
	}

	// Print row count
	sb.WriteString(fmt.Sprintf("(%d row", r.RowCount))
	if r.RowCount != 1 {
		sb.WriteString("s")
	}
	sb.WriteString(")\n")

	return sb.String()
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// ToMaps converts the result to a slice of maps.
func (r *Result) ToMaps() []map[string]interface{} {
	result := make([]map[string]interface{}, len(r.Rows))
	for i, row := range r.Rows {
		m := make(map[string]interface{})
		for j, col := range r.Columns {
			if j < len(row) {
				m[col] = row[j]
			}
		}
		result[i] = m
	}
	return result
}
