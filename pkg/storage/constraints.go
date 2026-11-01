package storage

import "fmt"

// PrimaryKeyViolationError preserves duplicate-key identity through wrapped
// executor errors so wire adapters can report integrity SQLSTATE 23505.
type PrimaryKeyViolationError struct {
	Table string
	Key   string
}

func (e *PrimaryKeyViolationError) Error() string {
	return fmt.Sprintf("duplicate primary key: %s", e.Key)
}

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
