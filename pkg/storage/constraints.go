package storage

import "fmt"

// NotNullViolationError identifies the final row that violates a required column.
type NotNullViolationError struct {
	Table  string
	Column string
}

func (e *NotNullViolationError) Error() string {
	return fmt.Sprintf("NOT NULL constraint failed: %s.%s", e.Table, e.Column)
}

func validateNotNull(schema *Schema, row Row) error {
	for _, column := range schema.Columns {
		if !column.Nullable && row[column.Name] == nil {
			return &NotNullViolationError{Table: schema.Name, Column: column.Name}
		}
	}
	return nil
}
