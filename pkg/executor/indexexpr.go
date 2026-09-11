package executor

import (
	"fmt"
	"strings"
	"sync"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// statelessEval is a shared, immutable Executor used only to evaluate validated,
// side-effect-free expressions (expression indexes and generated columns) from
// the storage layer. It holds no session, catalog, or per-connection state, so
// concurrent evaluations from different connections share no mutable data. The
// expression validators below guarantee that only subquery-free, deterministic
// expressions reach it.
var statelessEval = &Executor{}

// storedExprCache memoizes parsed stored expressions across all connections. A
// stored expression is immutable text, so the cache is safe to share.
var storedExprCache sync.Map // string -> parser.Expr

// parseStoredExpr parses (once) an expression persisted in the durable schema
// (generated column or expression index) and rejects anything that is not
// deterministic, subquery-free SQL. The same text always parses to the same
// expression, so the result is cached.
func parseStoredExpr(text string) (parser.Expr, error) {
	if cached, ok := storedExprCache.Load(text); ok {
		return cached.(parser.Expr), nil
	}
	expr, err := parser.ParseExpr(text)
	if err != nil {
		return nil, fmt.Errorf("invalid stored expression %q: %w", text, err)
	}
	if err := ValidateDeterministicExpr(expr); err != nil {
		return nil, fmt.Errorf("invalid stored expression %q: %w", text, err)
	}
	storedExprCache.Store(text, expr)
	return expr, nil
}

// EvalStoredExpression evaluates a persisted, validated expression against a
// row. It is the storage layer's expression-index evaluator and is stateless and
// concurrency-safe.
func EvalStoredExpression(text string, row storage.Row) (interface{}, error) {
	expr, err := parseStoredExpr(text)
	if err != nil {
		return nil, err
	}
	return statelessEval.evalExpr(expr, row)
}

// ValidateDeterministicExpr rejects an expression that SQLite would not allow in
// an index or generated column: subqueries, window functions, aggregates, and
// non-deterministic or unknown functions.
func ValidateDeterministicExpr(expr parser.Expr) error {
	switch e := expr.(type) {
	case nil:
		return nil
	case *parser.LiteralExpr:
		return nil
	case *parser.ColumnRef:
		return nil
	case *parser.ParenExpr:
		return ValidateDeterministicExpr(e.Expr)
	case *parser.UnaryExpr:
		return ValidateDeterministicExpr(e.Operand)
	case *parser.BinaryExpr:
		if err := ValidateDeterministicExpr(e.Left); err != nil {
			return err
		}
		return ValidateDeterministicExpr(e.Right)
	case *parser.IsNullExpr:
		return ValidateDeterministicExpr(e.Left)
	case *parser.IsDistinctExpr:
		if err := ValidateDeterministicExpr(e.Left); err != nil {
			return err
		}
		return ValidateDeterministicExpr(e.Right)
	case *parser.BetweenExpr:
		if err := ValidateDeterministicExpr(e.Left); err != nil {
			return err
		}
		if err := ValidateDeterministicExpr(e.Low); err != nil {
			return err
		}
		return ValidateDeterministicExpr(e.High)
	case *parser.LikeExpr:
		if err := ValidateDeterministicExpr(e.Left); err != nil {
			return err
		}
		if err := ValidateDeterministicExpr(e.Pattern); err != nil {
			return err
		}
		return ValidateDeterministicExpr(e.Escape)
	case *parser.CaseExpr:
		if err := ValidateDeterministicExpr(e.Operand); err != nil {
			return err
		}
		for _, w := range e.Whens {
			if err := ValidateDeterministicExpr(w.Condition); err != nil {
				return err
			}
			if err := ValidateDeterministicExpr(w.Result); err != nil {
				return err
			}
		}
		return ValidateDeterministicExpr(e.Else)
	case *parser.CastExpr:
		return ValidateDeterministicExpr(e.Expr)
	case *parser.InExpr:
		if e.Subquery != nil {
			return fmt.Errorf("subqueries are not allowed in index expressions")
		}
		if err := ValidateDeterministicExpr(e.Left); err != nil {
			return err
		}
		for _, v := range e.Values {
			if err := ValidateDeterministicExpr(v); err != nil {
				return err
			}
		}
		return nil
	case *parser.FunctionCall:
		if e.Star {
			return fmt.Errorf("function %s(*) is not allowed in index expressions", e.Name)
		}
		if isAggregateFunctionName(e.Name, len(e.Args)) {
			return fmt.Errorf("aggregate function %s() is not allowed in index expressions", e.Name)
		}
		if !isDeterministicFunction(e.Name) {
			return fmt.Errorf("non-deterministic or unsupported function %s() is not allowed in index expressions", e.Name)
		}
		for _, a := range e.Args {
			if err := ValidateDeterministicExpr(a); err != nil {
				return err
			}
		}
		return nil
	case *parser.SubqueryExpr:
		return fmt.Errorf("subqueries are not allowed in index expressions")
	case *parser.ExistsExpr:
		return fmt.Errorf("subqueries are not allowed in index expressions")
	case *parser.WindowExpr:
		return fmt.Errorf("window functions are not allowed in index expressions")
	default:
		return fmt.Errorf("unsupported expression in index definition: %T", expr)
	}
}

// isAggregateFunctionName reports whether a function name is an aggregate in the
// given call shape. MIN/MAX are scalar with two or more arguments.
func isAggregateFunctionName(name string, argCount int) bool {
	switch strings.ToUpper(name) {
	case "COUNT", "SUM", "AVG", "TOTAL", "GROUP_CONCAT",
		"JSON_GROUP_ARRAY", "JSONB_GROUP_ARRAY", "JSON_GROUP_OBJECT", "JSONB_GROUP_OBJECT":
		return true
	case "MIN", "MAX":
		return argCount < 2
	}
	return false
}

// deterministicFunctions is the allowlist of scalar functions that may appear in
// a persisted expression. It intentionally excludes date/time functions (which
// are non-deterministic when using "now") and every random/session function.
var deterministicFunctions = map[string]bool{
	"LOWER": true, "UPPER": true, "LENGTH": true, "ABS": true,
	"COALESCE": true, "NULLIF": true, "IFNULL": true, "NVL": true,
	"TYPEOF": true, "SUBSTR": true, "SUBSTRING": true, "TRIM": true,
	"REPLACE": true, "PRINTF": true, "HEX": true, "UNHEX": true,
	"ZEROBLOB": true, "INSTR": true, "GLOB": true, "ROUND": true,
	"MAX": true, "MIN": true, "CONCAT": true, "PERCENT_DIFF": true,
	// JSON1 scalar functions.
	"JSON": true, "JSONB": true, "JSON_VALID": true, "JSONB_VALID": true,
	"JSON_TYPE": true, "JSON_EXTRACT": true, "JSONB_EXTRACT": true,
	"JSON_SET": true, "JSONB_SET": true, "JSON_INSERT": true, "JSONB_INSERT": true,
	"JSON_REPLACE": true, "JSONB_REPLACE": true, "JSON_REMOVE": true, "JSONB_REMOVE": true,
	"JSON_ARRAY": true, "JSONB_ARRAY": true, "JSON_OBJECT": true, "JSONB_OBJECT": true,
	"JSON_QUOTE": true, "JSONB_QUOTE": true,
}

func isDeterministicFunction(name string) bool {
	return deterministicFunctions[strings.ToUpper(name)]
}

// ValidateIndexColumns checks that every column referenced by a stored
// expression exists on the table (or is a hidden rowid alias). It is applied to
// expression indexes and generated columns at creation time.
func ValidateIndexColumns(expr parser.Expr, schema *storage.Schema) error {
	switch e := expr.(type) {
	case nil:
		return nil
	case *parser.ColumnRef:
		if e.Column == "*" {
			return fmt.Errorf("wildcards are not allowed in index expressions")
		}
		if storage.IsRowIDColumn(e.Column) {
			return nil
		}
		if _, ok := schema.GetColumn(e.Column); !ok {
			return fmt.Errorf("column not found in index expression: %s", e.Column)
		}
		return nil
	case *parser.ParenExpr:
		return ValidateIndexColumns(e.Expr, schema)
	case *parser.UnaryExpr:
		return ValidateIndexColumns(e.Operand, schema)
	case *parser.BinaryExpr:
		if err := ValidateIndexColumns(e.Left, schema); err != nil {
			return err
		}
		return ValidateIndexColumns(e.Right, schema)
	case *parser.IsNullExpr:
		return ValidateIndexColumns(e.Left, schema)
	case *parser.IsDistinctExpr:
		if err := ValidateIndexColumns(e.Left, schema); err != nil {
			return err
		}
		return ValidateIndexColumns(e.Right, schema)
	case *parser.BetweenExpr:
		if err := ValidateIndexColumns(e.Left, schema); err != nil {
			return err
		}
		if err := ValidateIndexColumns(e.Low, schema); err != nil {
			return err
		}
		return ValidateIndexColumns(e.High, schema)
	case *parser.LikeExpr:
		if err := ValidateIndexColumns(e.Left, schema); err != nil {
			return err
		}
		if err := ValidateIndexColumns(e.Pattern, schema); err != nil {
			return err
		}
		return ValidateIndexColumns(e.Escape, schema)
	case *parser.CaseExpr:
		if err := ValidateIndexColumns(e.Operand, schema); err != nil {
			return err
		}
		for _, w := range e.Whens {
			if err := ValidateIndexColumns(w.Condition, schema); err != nil {
				return err
			}
			if err := ValidateIndexColumns(w.Result, schema); err != nil {
				return err
			}
		}
		return ValidateIndexColumns(e.Else, schema)
	case *parser.CastExpr:
		return ValidateIndexColumns(e.Expr, schema)
	case *parser.InExpr:
		if err := ValidateIndexColumns(e.Left, schema); err != nil {
			return err
		}
		for _, v := range e.Values {
			if err := ValidateIndexColumns(v, schema); err != nil {
				return err
			}
		}
		return nil
	case *parser.FunctionCall:
		for _, a := range e.Args {
			if err := ValidateIndexColumns(a, schema); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}
