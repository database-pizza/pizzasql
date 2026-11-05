package executor

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// defaultColumnValue resolves a DEFAULT declaration into the representation the
// schema catalog persists. A constant is evaluated once and stored as its value.
// The whitelist of clock-reading defaults is stored as SQL text instead, so it
// is evaluated on every write rather than frozen at CREATE/ALTER time.
//
// Every other non-constant expression is rejected: the catalog would have to
// store its SQL text and a write would have to evaluate it, and silently
// freezing the DDL-time value is worse than refusing the declaration.
func defaultColumnValue(e *Executor, column string, expr parser.Expr) (interface{}, string, error) {
	if isConstantExpr(expr) {
		value, err := e.evalExpr(expr, nil)
		if err != nil {
			return nil, "", fmt.Errorf("evaluate DEFAULT for column %q: %w", column, err)
		}
		return value, "", nil
	}
	if isWhitelistedDefaultExpr(expr) {
		return nil, parser.FormatExpr(expr), nil
	}
	return nil, "", pgerror.Unsupported(
		"DEFAULT for column %q is not a constant expression: %s; PizzaSQL evaluates a non-constant default at write time only for CURRENT_TIMESTAMP, CURRENT_DATE, CURRENT_TIME, datetime('now'), date('now'), and time('now'). Omit the default and supply the value on INSERT",
		column, parser.FormatExpr(expr),
	)
}

// isConstantExpr reports whether expr is a literal, a unary sign applied to a
// literal, a parenthesized constant, or a cast of a constant. Everything else
// (column references including CURRENT_TIMESTAMP, function calls, subqueries,
// arithmetic) is non-constant.
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

// isWhitelistedDefaultExpr reports whether expr is one of the clock-reading
// defaults PizzaSQL evaluates per write. The list is deliberately small: these
// are the forms that SQLite applications declare, and each maps to a value the
// engine already produces for the equivalent expression.
func isWhitelistedDefaultExpr(expr parser.Expr) bool {
	switch ex := expr.(type) {
	case *parser.ColumnRef:
		if ex.Table != "" {
			return false
		}
		switch strings.ToLower(ex.Column) {
		case "current_timestamp", "current_date", "current_time":
			return true
		}
	case *parser.FunctionCall:
		if len(ex.Args) != 1 {
			return false
		}
		switch strings.ToLower(ex.Name) {
		case "datetime", "date", "time":
		default:
			return false
		}
		lit, ok := ex.Args[0].(*parser.LiteralExpr)
		return ok && lit.Type == lexer.TokenString && strings.EqualFold(lit.Value, "now")
	}
	return false
}

// defaultExprCache holds parsed default expressions. The stored form is SQL
// text validated at DDL time, so parsing it again per statement uses the same
// parser path and keeps the catalog independent of the AST.
var defaultExprCache sync.Map // map[string]parser.Expr

func parseDefaultExpr(text string) (parser.Expr, error) {
	if cached, ok := defaultExprCache.Load(text); ok {
		return cached.(parser.Expr), nil
	}
	expr, err := parser.ParseExpr(text)
	if err != nil {
		return nil, fmt.Errorf("invalid stored DEFAULT expression %q: %w", text, err)
	}
	defaultExprCache.Store(text, expr)
	return expr, nil
}

// EvalDefaultExpression evaluates a persisted non-constant DEFAULT expression
// at the current time. It is the storage layer's evaluator for columns added
// with ALTER TABLE ADD COLUMN and for row images that predate such a column; an
// INSERT or UPDATE evaluates the expression once per statement itself.
func EvalDefaultExpression(text string) (interface{}, error) {
	return evalDefaultExpressionAt(text, time.Now().UTC())
}

// evalDefaultExpressionAt evaluates a persisted default with an explicit clock.
// The value shapes match what the same expression returns in a query, so a
// defaulted timestamp column compares equal to CURRENT_TIMESTAMP:
// CURRENT_TIMESTAMP and datetime('now') yield "2006-01-02 15:04:05" UTC,
// CURRENT_DATE and date('now') yield "2006-01-02", and CURRENT_TIME and
// time('now') yield "15:04:05". That space-separated UTC text is what the
// engine already stores and what the timestamptz OID path emits, so clients
// that decode timestamptz accept it.
func evalDefaultExpressionAt(text string, now time.Time) (interface{}, error) {
	expr, err := parseDefaultExpr(text)
	if err != nil {
		return nil, err
	}
	if !isWhitelistedDefaultExpr(expr) {
		return nil, pgerror.Unsupported(
			"stored DEFAULT expression %q is not supported; PizzaSQL only evaluates CURRENT_TIMESTAMP, CURRENT_DATE, CURRENT_TIME, datetime('now'), date('now'), and time('now') at write time",
			text,
		)
	}
	switch ex := expr.(type) {
	case *parser.ColumnRef:
		value, _ := sqliteCurrentTimeValueAt(ex.Column, now)
		return value, nil
	case *parser.FunctionCall:
		keyword := map[string]string{
			"datetime": "current_timestamp",
			"date":     "current_date",
			"time":     "current_time",
		}[strings.ToLower(ex.Name)]
		value, _ := sqliteCurrentTimeValueAt(keyword, now)
		return value, nil
	}
	return nil, pgerror.Unsupported("stored DEFAULT expression %q is not supported", text)
}

// statementDefaults evaluates every expression default in the schema once for
// the statement. A multi-row INSERT therefore stamps all its rows with one
// value, while a later statement reads the clock again. Constant defaults stay
// in the catalog and are applied by the storage layer.
func (e *Executor) statementDefaults(schema *storage.Schema) (map[string]interface{}, error) {
	var defaults map[string]interface{}
	for _, column := range schema.Columns {
		if column.DefaultExpr == "" {
			continue
		}
		value, err := e.evalDefaultExpr(column.DefaultExpr)
		if err != nil {
			return nil, fmt.Errorf("evaluate DEFAULT for column %q: %w", column.Name, err)
		}
		if defaults == nil {
			defaults = make(map[string]interface{})
		}
		defaults[column.Name] = value
	}
	return defaults, nil
}

func (e *Executor) evalDefaultExpr(text string) (interface{}, error) {
	now := time.Now().UTC()
	if e.clock != nil {
		now = e.clock().UTC()
	}
	return evalDefaultExpressionAt(text, now)
}

// applyRowDefaults fills the expression-defaulted columns a statement omitted.
// An explicit NULL is already present in the row, so it wins over the default.
func applyRowDefaults(row storage.Row, defaults map[string]interface{}) {
	for name, value := range defaults {
		if _, present := lookupRowValue(row, name); present {
			continue
		}
		row[name] = value
	}
}
