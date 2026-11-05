package executor

import (
	"fmt"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// EvalCheckExpression evaluates a persisted CHECK predicate against a row. It is
// the storage layer's CHECK evaluator, stateless and concurrency-safe like
// EvalStoredExpression.
//
// The result follows SQL three-valued logic: NULL and any other value that is
// not definitely false passes the constraint. The truth test is the engine's
// WHERE predicate test (toBool), so a CHECK and an equivalent WHERE filter agree
// on the same row.
func EvalCheckExpression(expression string, row storage.Row) (bool, error) {
	expr, err := parseStoredExpr(expression)
	if err != nil {
		return false, err
	}
	value, err := statelessEval.evalExpr(expr, row)
	if err != nil {
		return false, err
	}
	if value == nil {
		return true, nil
	}
	return toBool(value), nil
}

// checkConstraintFor turns a parsed CHECK predicate into its durable form. Only
// deterministic, subquery-free predicates over unqualified columns of the table
// are accepted: the expression is persisted as SQL text and re-evaluated on
// every write, so an aggregate, window function, subquery, or clock/random
// function could not be enforced consistently.
func checkConstraintFor(name string, expr parser.Expr, schema *storage.Schema) (storage.CheckConstraint, error) {
	text := parser.FormatExpr(expr)
	if err := ValidateDeterministicExpr(expr); err != nil {
		return storage.CheckConstraint{}, pgerror.Unsupported(
			"CHECK expression %s is not allowed: %v; a CHECK predicate must be deterministic and free of subqueries, aggregates, and window functions",
			text, err,
		)
	}
	for _, ref := range collectTableColumnRefs(expr) {
		if ref.tbl != "" {
			return storage.CheckConstraint{}, pgerror.Unsupported(
				"CHECK expression %s uses the qualified column reference %s.%s; name the column without a table qualifier so the predicate stays bound to this table",
				text, ref.tbl, ref.col,
			)
		}
	}
	if err := ValidateIndexColumns(expr, schema); err != nil {
		return storage.CheckConstraint{}, pgerror.Unsupported(
			"CHECK expression %s is invalid: %v", text, err,
		)
	}
	// The predicate is persisted as this exact text and re-parsed by the
	// evaluator on every later write. Reject a DDL whose formatted text does not
	// round-trip instead of publishing a constraint that fails every write.
	if _, err := parseStoredExpr(text); err != nil {
		return storage.CheckConstraint{}, pgerror.Unsupported(
			"CHECK expression %s cannot be stored for later evaluation: %v; the predicate must round-trip through the engine's SQL text form",
			text, err,
		)
	}
	return storage.CheckConstraint{Name: name, Expression: text}, nil
}

// tableChecks appends the parsed CHECK constraints of a column definition or a
// table-level constraint list. The schema passed in must already contain every
// column the predicate may reference, so callers validate after the full column
// list is built.
func (e *Executor) tableChecks(schema *storage.Schema, columnConstraints []parser.ColumnConstraint, tableConstraints []parser.TableConstraint) ([]storage.CheckConstraint, error) {
	var checks []storage.CheckConstraint
	for _, constraint := range columnConstraints {
		if constraint.Type != parser.ConstraintCheck {
			continue
		}
		check, err := checkConstraintFor(constraint.Name, constraint.Check, schema)
		if err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	for _, constraint := range tableConstraints {
		if constraint.Type != parser.ConstraintCheck {
			continue
		}
		check, err := checkConstraintFor(constraint.Name, constraint.Check, schema)
		if err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	return checks, nil
}

// schemaWithColumn returns the table schema plus one not-yet-published column,
// so a CHECK predicate declared on that column can be validated against the
// table it will apply to.
func schemaWithColumn(schema *storage.Schema, column storage.Column) *storage.Schema {
	extended := *schema
	extended.Columns = append(append([]storage.Column(nil), schema.Columns...), column)
	return &extended
}

// rejectCheckColumnChange refuses to rename or drop a column that a persisted
// CHECK predicate references. Predicates are stored as SQL text, so the engine
// cannot rewrite them; leaving the old identifier behind would make the
// constraint evaluate to NULL and silently stop enforcing it.
func (e *Executor) rejectCheckColumnChange(table, column, action string) error {
	schema, err := e.session.GetSchema(table)
	if err != nil {
		return err
	}
	for _, check := range schema.Checks {
		references, err := storedExpressionReferencesColumn(check.Expression, column)
		if err != nil {
			return fmt.Errorf("CHECK constraint %s: %w", check.Label(), err)
		}
		if references {
			return pgerror.Unsupported(
				"cannot %s column %s: CHECK constraint %s depends on it; recreate the table without that constraint first",
				action, column, check.Label(),
			)
		}
	}
	return nil
}
