package executor

import (
	"fmt"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

// constantDefaultValue evaluates a DEFAULT expression that cannot depend on the
// row, session clock, or other mutable state.
//
// The schema catalog stores the evaluated value, not the expression SQL, so a
// non-constant default such as CURRENT_TIMESTAMP would be frozen at DDL time and
// every later INSERT would silently receive the same value. Rejecting the
// declaration is safer than accepting it with different semantics. Per-INSERT
// evaluation is not implemented.
func constantDefaultValue(e *Executor, column string, expr parser.Expr) (interface{}, error) {
	if !isConstantExpr(expr) {
		return nil, pgerror.Unsupported(
			"DEFAULT for column %q is not a constant expression: %s; PizzaSQL stores the evaluated default in the schema catalog, so a non-constant default would be frozen at CREATE/ALTER time. Omit the default and supply the value on INSERT",
			column, parser.FormatExpr(expr),
		)
	}
	value, err := e.evalExpr(expr, nil)
	if err != nil {
		return nil, fmt.Errorf("evaluate DEFAULT for column %q: %w", column, err)
	}
	return value, nil
}

// isConstantExpr reports whether expr is a literal, a unary sign applied to a
// literal, a parenthesized constant, or a cast of a constant. Everything else
// (column references including CURRENT_TIMESTAMP, function calls, subqueries,
// arithmetic) is rejected because its value can change between statements.
func isConstantExpr(expr parser.Expr) bool {
	switch ex := expr.(type) {
	case *parser.LiteralExpr:
		return true
	case *parser.UnaryExpr:
		if ex.Op != lexer.TokenMinus && ex.Op != lexer.TokenPlus {
			return false
		}
		return isConstantExpr(ex.Operand)
	case *parser.ParenExpr:
		return isConstantExpr(ex.Expr)
	case *parser.CastExpr:
		return isConstantExpr(ex.Expr)
	default:
		return false
	}
}
