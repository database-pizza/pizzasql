package executor

import (
	"strconv"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// columnTypeResolver resolves a possibly-qualified column reference to the SQL
// type name declared in the schema.
type columnTypeResolver func(table, column string) (string, bool)

// schemaTypeResolver builds a resolver over a single table schema. The table
// qualifier is ignored because the analyzer has already validated it.
func schemaTypeResolver(schema *storage.Schema) columnTypeResolver {
	return func(_ string, column string) (string, bool) {
		if schema == nil {
			return "", false
		}
		for _, c := range schema.Columns {
			if strings.EqualFold(c.Name, column) {
				return c.Type, true
			}
		}
		return "", false
	}
}

// selectColumnTypes computes the result column types for a single-table SELECT
// projection, parallel to the column names produced by the normal projection
// expansion. Direct column references (including aliases) resolve to the
// declared schema type and common expressions resolve to the type the executor
// produces for them. Types are derived from schema metadata and the expression
// shape rather than row values so they remain correct even when the result set
// is empty.
func selectColumnTypes(stmt *parser.SelectStmt, schema *storage.Schema) []string {
	resolve := schemaTypeResolver(schema)
	types := make([]string, 0, len(stmt.Columns))
	for _, col := range stmt.Columns {
		if col.Star {
			for _, c := range schema.Columns {
				types = append(types, c.Type)
			}
			continue
		}
		if col.TableStar != "" {
			// Single-table qualified wildcard: expand the table's columns. The
			// analyzer already validated the qualifier, and for this path the
			// FROM clause holds exactly one table.
			for _, c := range schema.Columns {
				types = append(types, c.Type)
			}
			continue
		}
		types = append(types, inferExprType(col.Expr, resolve))
	}
	return types
}

// projectionColumnType resolves the type of a single projected expression
// against a single table schema.
func projectionColumnType(col parser.SelectColumn, schema *storage.Schema) string {
	return inferExprType(col.Expr, schemaTypeResolver(schema))
}

// joinedSelectColumnTypes resolves projections against every table in a joined
// or comma-separated FROM clause. An unqualified reference resolves to the first
// table that declares it, matching how row lookups are performed.
func (e *Executor) joinedSelectColumnTypes(stmt *parser.SelectStmt, refs []parser.TableRef) []string {
	resolve := e.joinedTypeResolver(refs)
	types := make([]string, 0, len(stmt.Columns))
	for _, projection := range stmt.Columns {
		if projection.Star {
			for _, ref := range refs {
				if schema, err := e.schema.GetSchema(ref.Name); err == nil {
					for _, column := range schema.Columns {
						types = append(types, column.Type)
					}
				}
			}
			continue
		}
		if projection.TableStar != "" {
			if columns, _, err := e.resolveTableStar(stmt.From, projection.TableStar); err == nil {
				for _, column := range columns {
					types = append(types, column.Type)
				}
			}
			continue
		}

		types = append(types, inferExprType(projection.Expr, resolve))
	}
	return types
}

// joinedTypeResolver resolves a column reference against the FROM clause. It
// mirrors the row lookup rules: a qualified reference is limited to the matching
// table alias or name, and the first table declaring an unqualified column wins.
func (e *Executor) joinedTypeResolver(refs []parser.TableRef) columnTypeResolver {
	return func(table, column string) (string, bool) {
		for _, ref := range refs {
			if table != "" && !strings.EqualFold(table, ref.Alias) && !strings.EqualFold(table, ref.Name) {
				continue
			}
			schema, err := e.schema.GetSchema(ref.Name)
			if err != nil {
				continue
			}
			for _, candidate := range schema.Columns {
				if strings.EqualFold(candidate.Name, column) {
					return candidate.Type, true
				}
			}
			if table != "" {
				// A qualifier names exactly one table; do not fall through to
				// another relation that happens to declare the same column.
				break
			}
		}
		return "", false
	}
}

// inferExprType reports the SQL type name of a projection expression, using the
// same names the schema stores so the wire layer can map them to PostgreSQL
// type OIDs. It only describes the values the executor produces; nothing is
// converted. TEXT is the fallback for expressions whose type is not known.
func inferExprType(expr parser.Expr, resolve columnTypeResolver) string {
	switch e := expr.(type) {
	case nil:
		return "TEXT"
	case *parser.ColumnRef:
		if resolve != nil {
			if typeName, ok := resolve(e.Table, e.Column); ok && !isUnknownTypeName(typeName) {
				return typeName
			}
		}
		if storage.IsRowIDColumn(e.Column) {
			// The hidden rowid alias is a signed 64-bit integer.
			return "BIGINT"
		}
		return "TEXT"
	case *parser.ParenExpr:
		return inferExprType(e.Expr, resolve)
	case *parser.LiteralExpr:
		return literalType(e)
	case *parser.CastExpr:
		if typeName, ok := castResultType(e.Type.Name); ok {
			return typeName
		}
		return "TEXT"
	case *parser.FunctionCall:
		return functionResultType(e, resolve)
	case *parser.BinaryExpr:
		return binaryResultType(e, resolve)
	case *parser.UnaryExpr:
		return unaryResultType(e, resolve)
	case *parser.CaseExpr:
		return caseResultType(e, resolve)
	case *parser.InExpr, *parser.BetweenExpr, *parser.LikeExpr, *parser.IsNullExpr,
		*parser.IsDistinctExpr, *parser.ExistsExpr:
		return "BOOLEAN"
	default:
		return "TEXT"
	}
}

// literalType reports the type of a literal value. Integer literals are INTEGER
// when they fit int32 and BIGINT otherwise, matching how the value is carried.
// Non-integral numeric literals are NUMERIC; their text form is decimal text.
func literalType(lit *parser.LiteralExpr) string {
	switch lit.Type {
	case lexer.TokenNumber:
		if strings.ContainsAny(lit.Value, ".eE") {
			return "NUMERIC"
		}
		if n, err := strconv.ParseInt(lit.Value, 10, 64); err == nil && n >= -2147483648 && n <= 2147483647 {
			return "INTEGER"
		}
		return "BIGINT"
	case lexer.TokenString:
		return "TEXT"
	case lexer.TokenBlob:
		return "BLOB"
	case lexer.TokenTRUE, lexer.TokenFALSE:
		return "BOOLEAN"
	default: // NULL and anything unrecognized stay unknown/TEXT.
		return "TEXT"
	}
}

// castResultType reports the result type of CAST(x AS name). Only targets the
// executor actually converts are reported; every other target leaves the value
// untouched, so the value keeps its original encoding and stays TEXT metadata.
func castResultType(name string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "INTEGER", "INT":
		return "INTEGER", true
	case "SMALLINT", "TINYINT":
		return "SMALLINT", true
	case "MEDIUMINT":
		return "INTEGER", true
	case "BIGINT":
		return "BIGINT", true
	case "REAL", "FLOAT":
		return "REAL", true
	case "DOUBLE":
		return "DOUBLE", true
	case "TEXT", "VARCHAR", "CHAR", "CHARACTER", "CLOB", "NCHAR", "NVARCHAR":
		return "TEXT", true
	case "BLOB":
		return "BLOB", true
	default:
		return "", false
	}
}

// functionResultType reports the result type of a function call. Aggregate
// result types mirror the values the executor computes: COUNT is BIGINT, SUM
// over integers is BIGINT, SUM over reals is DOUBLE, and AVG over non-real input
// is NUMERIC because it produces a non-integral decimal.
func functionResultType(fn *parser.FunctionCall, resolve columnTypeResolver) string {
	argType := "TEXT"
	if len(fn.Args) > 0 {
		argType = inferExprType(fn.Args[0], resolve)
	}
	switch strings.ToUpper(fn.Name) {
	case "COUNT":
		return "BIGINT"
	case "SUM":
		return sumResultType(argType)
	case "TOTAL":
		return "DOUBLE"
	case "AVG":
		if isRealTypeName(argType) {
			return "DOUBLE"
		}
		return "NUMERIC"
	case "MIN", "MAX":
		// MIN/MAX return one of the input values unchanged.
		return argType
	default:
		return "TEXT"
	}
}

// sumResultType reports the SUM result type for an input type. The executor
// accumulates integers in int64 and everything else in float64.
func sumResultType(argType string) string {
	switch {
	case isRealTypeName(argType):
		return "DOUBLE"
	case isIntegerTypeName(argType):
		return "BIGINT"
	case isNumericTypeName(argType):
		return "NUMERIC"
	default:
		return "TEXT"
	}
}

// binaryResultType reports the result type of a binary operator. Comparisons and
// boolean operators produce BOOLEAN; arithmetic reports the widest operand type.
func binaryResultType(e *parser.BinaryExpr, resolve columnTypeResolver) string {
	switch e.Op {
	case lexer.TokenEq, lexer.TokenNeq, lexer.TokenLt, lexer.TokenLte, lexer.TokenGt, lexer.TokenGte,
		lexer.TokenAND, lexer.TokenOR:
		return "BOOLEAN"
	case lexer.TokenConcat:
		return "TEXT"
	case lexer.TokenBitAnd, lexer.TokenBitOr, lexer.TokenShiftLeft, lexer.TokenShiftRight:
		return "BIGINT"
	case lexer.TokenPlus, lexer.TokenMinus, lexer.TokenStar, lexer.TokenSlash, lexer.TokenPercent:
		left := inferExprType(e.Left, resolve)
		right := inferExprType(e.Right, resolve)
		switch {
		case isRealTypeName(left) || isRealTypeName(right):
			return "DOUBLE"
		case isIntegerTypeName(left) && isIntegerTypeName(right):
			// Integer arithmetic is accumulated in int64, so report the wider
			// integer type even when both operands are INTEGER.
			return "BIGINT"
		case isNumericTypeName(left) && isNumericTypeName(right):
			return "NUMERIC"
		default:
			return "TEXT"
		}
	default:
		return "TEXT"
	}
}

// unaryResultType reports the result type of a unary operator.
func unaryResultType(e *parser.UnaryExpr, resolve columnTypeResolver) string {
	switch e.Op {
	case lexer.TokenNOT:
		return "BOOLEAN"
	case lexer.TokenBitNot:
		return "BIGINT"
	case lexer.TokenMinus, lexer.TokenPlus:
		operand := inferExprType(e.Operand, resolve)
		if isNumericTypeName(operand) {
			return operand
		}
		return "TEXT"
	default:
		return "TEXT"
	}
}

// caseResultType reports the type of a CASE expression when every branch agrees
// on the same non-TEXT type; otherwise the type is unknown.
func caseResultType(e *parser.CaseExpr, resolve columnTypeResolver) string {
	result := ""
	for _, when := range e.Whens {
		typeName := inferExprType(when.Result, resolve)
		if result == "" {
			result = typeName
			continue
		}
		if !strings.EqualFold(result, typeName) {
			return "TEXT"
		}
	}
	if e.Else != nil {
		typeName := inferExprType(e.Else, resolve)
		if result == "" || !strings.EqualFold(result, typeName) {
			return "TEXT"
		}
	}
	if result == "" {
		return "TEXT"
	}
	return result
}

// isIntegerTypeName reports whether a type name is an integer type.
func isIntegerTypeName(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "INTEGER", "INT", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT":
		return true
	default:
		return false
	}
}

// isRealTypeName reports whether a type name is a floating-point type.
func isRealTypeName(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "REAL", "FLOAT", "DOUBLE":
		return true
	default:
		return false
	}
}

// isNumericTypeName reports whether a type name is any numeric type.
func isNumericTypeName(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "NUMERIC", "DECIMAL":
		return true
	default:
		return isIntegerTypeName(name) || isRealTypeName(name)
	}
}

// isUnknownTypeName reports whether a schema type name carries no type
// information, as used for derived-table and CTE columns.
func isUnknownTypeName(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "", "ANY", "UNKNOWN", "NULL":
		return true
	default:
		return false
	}
}
