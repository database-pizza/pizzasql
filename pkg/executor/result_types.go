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

// snapshotTypeResolver builds a column-type resolver from the FROM clause. It
// reads every table schema up front, before the statement takes the session lock
// for its scan, so value-time lookups never re-enter the session lock (which
// would deadlock the scan that is asking).
func (e *Executor) snapshotTypeResolver(refs []parser.TableRef) columnTypeResolver {
	type tableSnapshot struct {
		name  string
		alias string
		types map[string]string
	}
	var tables []tableSnapshot
	for _, ref := range collectAllTableRefs(refs) {
		if ref.Subquery != nil || ref.Function != nil || ref.Name == "" {
			continue
		}
		schema, err := e.session.GetSchema(ref.Name)
		if err != nil {
			continue
		}
		snapshot := tableSnapshot{name: ref.Name, alias: ref.Alias, types: make(map[string]string, len(schema.Columns))}
		for _, column := range schema.Columns {
			snapshot.types[strings.ToUpper(column.Name)] = column.Type
		}
		tables = append(tables, snapshot)
	}
	return func(table, column string) (string, bool) {
		upper := strings.ToUpper(column)
		for _, snapshot := range tables {
			if table != "" && !strings.EqualFold(table, snapshot.alias) && !strings.EqualFold(table, snapshot.name) {
				continue
			}
			if typeName, ok := snapshot.types[upper]; ok {
				return typeName, true
			}
			if table != "" {
				break
			}
		}
		return "", false
	}
}

// pushTypeScope adds the column-type resolver of the SELECT or DML statement
// being evaluated. Scopes nest: the innermost resolver wins, and an outer
// query's resolver still answers for correlated references.
func (e *Executor) pushTypeScope(resolve columnTypeResolver) {
	e.typeScopes = append(e.typeScopes, resolve)
}

// popTypeScope removes the innermost resolver pushed by pushTypeScope.
func (e *Executor) popTypeScope() {
	if len(e.typeScopes) == 0 {
		return
	}
	e.typeScopes = e.typeScopes[:len(e.typeScopes)-1]
}

// resolveColumnType resolves a column reference against the active statement
// scopes, innermost first. It reports false when no scope declares the column,
// which leaves the caller on the TEXT fallback.
func (e *Executor) resolveColumnType(table, column string) (string, bool) {
	for i := len(e.typeScopes) - 1; i >= 0; i-- {
		if e.typeScopes[i] == nil {
			continue
		}
		if typeName, ok := e.typeScopes[i](table, column); ok {
			return typeName, true
		}
	}
	return "", false
}

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
				if schema, err := e.session.GetSchema(ref.Name); err == nil {
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
			schema, err := e.session.GetSchema(ref.Name)
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
		if typeName, ok := castResultType(e.Type.SQLTypeName()); ok {
			return typeName
		}
		return "TEXT"
	case *parser.ArrayExpr:
		return arrayConstructorType(e, resolve)
	case *parser.SubscriptExpr:
		return subscriptResultType(e, resolve)
	case *parser.AnyAllExpr:
		return "BOOLEAN"
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
// Non-integral numeric literals are carried as float64, so they report DOUBLE
// rather than NUMERIC: the engine has no exact decimal carrier.
func literalType(lit *parser.LiteralExpr) string {
	switch lit.Type {
	case lexer.TokenNumber:
		if strings.ContainsAny(lit.Value, ".eE") {
			return "DOUBLE"
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
//
// Casts report the carrier type of the converted value, not the target width:
// evalCastExpr always produces int64 for integer targets and float64 for
// real targets, and it does not range-check them. A narrower advertised type
// (int2/int4/float4) could otherwise fail to decode a value the engine accepted.
func castResultType(name string) (string, bool) {
	if element, isArray := arrayElementType(name); isArray {
		return arrayResultTypeName(element), true
	}
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "INTEGER", "INT", "SMALLINT", "TINYINT", "MEDIUMINT", "BIGINT":
		return "BIGINT", true
	case "REAL", "FLOAT", "DOUBLE":
		return "DOUBLE", true
	case "TEXT", "VARCHAR", "CHAR", "CHARACTER", "CLOB", "NCHAR", "NVARCHAR":
		return "TEXT", true
	case "BLOB":
		return "BLOB", true
	default:
		return "", false
	}
}

// arrayConstructorType reports the array type of an ARRAY[...] constructor. The
// element type follows the first non-NULL element; a constructor mixing element
// types falls back to TEXT[].
func arrayConstructorType(expr *parser.ArrayExpr, resolve columnTypeResolver) string {
	elementType := ""
	for _, element := range expr.Elements {
		typeName := inferExprType(element, resolve)
		if isUnknownTypeName(typeName) {
			continue
		}
		if elementType == "" {
			elementType = typeName
			continue
		}
		if !strings.EqualFold(elementType, typeName) {
			elementType = "TEXT"
		}
	}
	if elementType == "" {
		elementType = "TEXT"
	}
	return arrayResultTypeName(elementType)
}

// subscriptResultType reports the element type of an array subscript. A
// subscript of a non-array expression keeps the TEXT fallback.
func subscriptResultType(expr *parser.SubscriptExpr, resolve columnTypeResolver) string {
	if element, ok := arrayElementType(inferExprType(expr.Expr, resolve)); ok {
		return element
	}
	return "TEXT"
}

// arrayResultTypeName maps an element type name to the type name the wire layer
// reports for the array. The mapping follows the element's carrier: INTEGER is
// int4 and REAL is float4, matching the scalar mappings, and narrower integer
// types report int8[] because values are not range-checked. NUMERIC/DECIMAL keep
// their own array type: array elements are validated text, so their digits are
// not rounded through float64.
func arrayResultTypeName(elementType string) string {
	switch strings.ToUpper(strings.TrimSpace(elementType)) {
	case "INTEGER", "INT", "INT4":
		return "INTEGER[]"
	case "BIGINT", "INT8", "SMALLINT", "INT2", "TINYINT", "MEDIUMINT":
		return "BIGINT[]"
	case "REAL", "FLOAT", "FLOAT4":
		return "REAL[]"
	case "DOUBLE", "FLOAT8":
		return "DOUBLE[]"
	case "NUMERIC", "DECIMAL":
		return "NUMERIC[]"
	case "BOOLEAN", "BOOL":
		return "BOOLEAN[]"
	case "VARCHAR", "NVARCHAR", "CHARACTER VARYING":
		return "VARCHAR[]"
	default:
		return "TEXT[]"
	}
}

// functionResultType reports the result type of a function call. Aggregate
// result types mirror the values the executor computes: COUNT is BIGINT, SUM
// over integers is BIGINT, SUM over float carriers is DOUBLE, and AVG is always
// DOUBLE because evalAggregateExpr divides in float64.
func functionResultType(fn *parser.FunctionCall, resolve columnTypeResolver) string {
	switch strings.ToUpper(fn.Name) {
	case "COUNT":
		return "BIGINT"
	case "SUM":
		return sumResultType(firstArgType(fn, resolve))
	case "TOTAL":
		return "DOUBLE"
	case "AVG":
		return "DOUBLE"
	case "ARRAY_LENGTH":
		return "INTEGER"
	case "MIN", "MAX":
		// MIN/MAX return one of the input values unchanged.
		return firstArgType(fn, resolve)
	default:
		return "TEXT"
	}
}

// firstArgType reports the inferred type of a function's first argument.
func firstArgType(fn *parser.FunctionCall, resolve columnTypeResolver) string {
	if len(fn.Args) == 0 {
		return "TEXT"
	}
	return inferExprType(fn.Args[0], resolve)
}

// sumResultType reports the SUM result type for an input type. The executor
// accumulates integers in int64 and everything else in float64.
func sumResultType(argType string) string {
	switch {
	case isFloatCarrierTypeName(argType):
		return "DOUBLE"
	case isIntegerTypeName(argType):
		return "BIGINT"
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
		case isFloatCarrierTypeName(left) || isFloatCarrierTypeName(right):
			return "DOUBLE"
		case isIntegerTypeName(left) && isIntegerTypeName(right):
			// Integer arithmetic is accumulated in int64, so report the wider
			// integer type even when both operands are INTEGER.
			return "BIGINT"
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
		switch {
		case isFloatCarrierTypeName(operand):
			return "DOUBLE"
		case isIntegerTypeName(operand):
			return operand
		default:
			return "TEXT"
		}
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

// isFloatCarrierTypeName reports whether a type name is carried as float64.
// NUMERIC and DECIMAL have no exact decimal carrier in the engine: their values
// round-trip through JSON as float64, so they behave like floating-point types.
func isFloatCarrierTypeName(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "REAL", "FLOAT", "DOUBLE", "NUMERIC", "DECIMAL":
		return true
	default:
		return false
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
