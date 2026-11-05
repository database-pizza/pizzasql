package storage

import (
	"fmt"
	"strings"
	"time"
)

// PrimaryKeyViolationError preserves duplicate-key identity through wrapped
// executor errors so wire adapters can report integrity SQLSTATE 23505.
type PrimaryKeyViolationError struct {
	Table string
	// Key is the primary key rendered for a reader, never the durable
	// length-prefixed encoding.
	Key string
}

func (e *PrimaryKeyViolationError) Error() string {
	return fmt.Sprintf("duplicate primary key: %s", e.Key)
}

// newPrimaryKeyViolation reports the duplicate key its write collided with. The
// message renders the key from the row instead of the durable key: a composite
// key is stored length-prefixed ("<len>:<value>" repeated), so splitting that
// encoding to label the violation would corrupt any component that itself
// contains the separator.
func newPrimaryKeyViolation(table string, schema *Schema, row Row) *PrimaryKeyViolationError {
	return &PrimaryKeyViolationError{Table: table, Key: primaryKeyLabel(schema, row)}
}

// primaryKeyLabel renders a row's primary-key components for a reader: a
// single-column key is that one component and a composite key is a parenthesized
// tuple such as (1,11). A text component is rendered as a SQL literal with
// embedded quotes doubled, so a value containing a comma or a quote stays
// distinguishable from the tuple syntax.
func primaryKeyLabel(schema *Schema, row Row) string {
	columns := schema.PrimaryKeyList()
	parts := make([]string, len(columns))
	for i, column := range columns {
		value, _ := rowColumnValue(row, column)
		parts[i] = primaryKeyComponent(value)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, ",") + ")"
}

func primaryKeyComponent(value interface{}) string {
	switch v := value.(type) {
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case []byte:
		return "'" + strings.ReplaceAll(string(v), "'", "''") + "'"
	default:
		return fmt.Sprintf("%v", v)
	}
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
//
// A constant default is copied from the catalog. An expression default is
// evaluated here instead, so a row that reaches storage without one (for
// example a staged transaction row whose column was published later) receives a
// value rather than freezing the DDL-time one. Ordinary INSERT/UPDATE resolve
// expression defaults in the executor before the write; this is the fallback
// for every other path.
func (m *SchemaManager) applySchemaDefaults(schema *Schema, row Row) error {
	for _, column := range schema.Columns {
		if _, ok := rowColumnValue(row, column.Name); ok {
			continue
		}
		value, ok, err := m.columnDefaultValue(column, m.defaultNow())
		if err != nil {
			return err
		}
		if ok {
			row[column.Name] = value
		}
	}
	return validateNotNull(schema, row)
}

// columnDefaultValue returns the value a row receives for a column it omits.
// A column with no default reports ok=false. DefaultExpr takes precedence over
// Default; the two are never both set by DDL, which keeps constant defaults in
// the catalog as values and clock-reading defaults as SQL text. now is the
// instant the caller sampled for its statement; a constant default ignores it.
func (m *SchemaManager) columnDefaultValue(column Column, now time.Time) (interface{}, bool, error) {
	if column.DefaultExpr != "" {
		eval := m.defaultEvaluator()
		if eval == nil {
			return nil, false, fmt.Errorf("column %q has a DEFAULT expression but no evaluator is installed", column.Name)
		}
		value, err := eval(column.DefaultExpr, now)
		if err != nil {
			return nil, false, fmt.Errorf("evaluate DEFAULT for column %q: %w", column.Name, err)
		}
		return value, true, nil
	}
	if column.Default != nil {
		return column.Default, true, nil
	}
	return nil, false, nil
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
