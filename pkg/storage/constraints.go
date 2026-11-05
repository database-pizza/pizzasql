package storage

import (
	"fmt"
	"strings"
)

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

// CheckViolationError identifies the CHECK predicate a write violated. It is a
// distinct type so the protocol layers can report SQLSTATE 23514.
type CheckViolationError struct {
	Table      string
	Constraint string
}

func (e *CheckViolationError) Error() string {
	return "CHECK constraint failed: " + e.Constraint
}

// validateChecks evaluates every persisted CHECK predicate against a complete
// row image. The evaluator reports NULL and other unknown results as passing, so
// SQL three-valued logic is preserved: a constraint only rejects a definite
// false.
func (m *SchemaManager) validateChecks(schema *Schema, row Row) error {
	if len(schema.Checks) == 0 {
		return nil
	}
	eval := m.checkEvaluator()
	if eval == nil {
		return fmt.Errorf("cannot validate CHECK constraints on %s: no CHECK evaluator installed", schema.Name)
	}
	for _, check := range schema.Checks {
		holds, err := eval(check.Expression, row)
		if err != nil {
			return fmt.Errorf("evaluate CHECK constraint %s on %s: %w", check.Label(), schema.Name, err)
		}
		if !holds {
			return &CheckViolationError{Table: schema.Name, Constraint: check.Label()}
		}
	}
	return nil
}

// validateRowConstraints enforces the NOT NULL and CHECK constraints a schema
// declares against a complete row image. Every write path calls it after
// defaults are applied and before the row becomes visible.
func (m *SchemaManager) validateRowConstraints(schema *Schema, row Row) error {
	if err := validateNotNull(schema, row); err != nil {
		return err
	}
	return m.validateChecks(schema, row)
}

// applySchemaDefaults completes a row image that was built against an older
// schema: columns the row predates receive their catalog default, and the row is
// then validated against the required columns of the current schema.
//
// Write paths re-run it under the table gate because building a row image and
// publishing it are separate steps. A transaction can normalize an INSERT before
// a concurrent ADD COLUMN publishes a column and commit after it; the commit's
// compare check cannot reject that row, because a new key is only checked for
// absence. Re-applying the current schema keeps such a row from becoming a
// durable NOT NULL violation, and gives it the default every other row received
// from the backfill. An absent key is the only thing filled, so an explicit NULL
// still does not select a default. Callers validate CHECK predicates separately
// through validateRowConstraints/validateChecks so each write site can run them
// against the final row image.
func applySchemaDefaults(schema *Schema, row Row) error {
	for _, column := range schema.Columns {
		if _, ok := rowColumnValue(row, column.Name); ok {
			continue
		}
		if column.Default != nil {
			row[column.Name] = column.Default
		}
	}
	return validateNotNull(schema, row)
}

// rowColumnValue returns the value a row holds for a column, matching the column
// name case-insensitively.
func rowColumnValue(row Row, name string) (interface{}, bool) {
	if value, ok := row[name]; ok {
		return value, true
	}
	for key, value := range row {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return nil, false
}
