package executor

import (
	"math"
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
		// A materialized CTE shadows a real table with the same name; never
		// read the table's schema for a name the query resolves as a CTE.
		if e.cteTableExists(ref.Name) {
			continue
		}
		// A virtual catalog relation has no durable schema: its columns come from
		// catalog metadata, and asking the session for them from inside a locked
		// scan would deadlock on the session lock the scan already holds.
		if _, ok := e.catalogRelationFor(ref); ok {
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

// joinTypeScope collects the column types of relations materialized while
// executing a join (derived tables and CTEs on the right side). The statement's
// WHERE and projection resolve through it after the join rows are built, so
// array subscripts and comparisons still decode their elements.
type joinTypeScope struct {
	qualified   map[string]string
	unqualified map[string]string
}

func newJoinTypeScope() *joinTypeScope {
	return &joinTypeScope{qualified: make(map[string]string), unqualified: make(map[string]string)}
}

// record stores one column's type. Unqualified lookups keep the first relation
// that declared the name, matching how merged join rows keep the left value.
func (s *joinTypeScope) record(qualifier, name, typeName string) {
	if s == nil {
		return
	}
	if qualifier != "" {
		s.qualified[strings.ToUpper(qualifier+"."+name)] = typeName
	}
	key := strings.ToUpper(name)
	if _, exists := s.unqualified[key]; !exists {
		s.unqualified[key] = typeName
	}
}

// resolve implements columnTypeResolver over the recorded join columns.
func (s *joinTypeScope) resolve(table, column string) (string, bool) {
	if s == nil {
		return "", false
	}
	if table != "" {
		if typeName, ok := s.qualified[strings.ToUpper(table+"."+column)]; ok {
			return typeName, true
		}
		return "", false
	}
	if typeName, ok := s.unqualified[strings.ToUpper(column)]; ok {
		return typeName, true
	}
	return "", false
}

// recordJoinColumns adds a materialized right relation's columns to the current
// statement's join scope.
func (e *Executor) recordJoinColumns(qualifier string, columns, types []string) {
	if len(e.joinTypeScopes) == 0 {
		return
	}
	scope := e.joinTypeScopes[len(e.joinTypeScopes)-1]
	for i, column := range columns {
		scope.record(qualifier, column, typeAt(types, i))
	}
}

// materializedTypeResolver builds a resolver over a materialized relation's
// columns. columns and types are positionally aligned; qualifiers holds the
// table name or alias each column can be referenced through (one entry per
// column). A known column with no recorded type returns ("", true) so an outer
// scope cannot supply a type for a name that is shadowed.
func materializedTypeResolver(columns, types, qualifiers []string) columnTypeResolver {
	if len(columns) == 0 {
		return nil
	}
	return func(table, column string) (string, bool) {
		for i, name := range columns {
			if !strings.EqualFold(name, column) {
				continue
			}
			qualifier := ""
			if i < len(qualifiers) {
				qualifier = qualifiers[i]
			}
			if table != "" && !strings.EqualFold(table, qualifier) {
				continue
			}
			if i < len(types) && types[i] != "" {
				return types[i], true
			}
			return "", true
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
		if typeName, ok := castResultType(e.Type.SQLTypeName(), e.Expr); ok {
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

// exactInt2TypeName is the result type name for a SMALLINT cast whose constant
// operand fits int2. A SMALLINT column keeps reporting the int8 carrier because
// stored values are not range-checked, so the exact width needs its own spelling
// for the wire layer to map to OID 21 (int2).
const exactInt2TypeName = "INT2"

// castResultType reports the result type of CAST(x AS name). Only targets the
// executor actually converts are reported; every other target leaves the value
// untouched, so the value keeps its original encoding and stays TEXT metadata.
//
// evalCastExpr always produces int64 for integer targets and float64 for real
// targets and does not range-check them, so a narrower advertised type could
// fail to decode a value the engine accepted. A cast therefore reports its
// declared width only when the operand is a constant that fits that width:
// SMALLINT and INTEGER use the int64 result range, and REAL the float32 range.
// A cast over a column, parameter, or other expression reports the carrier type
// instead (int8 or float8).
func castResultType(name string, operand parser.Expr) (string, bool) {
	if element, isArray := arrayElementType(name); isArray {
		return arrayResultTypeName(element), true
	}
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "SMALLINT":
		if value, ok := constantNumericValue(operand); ok && value >= math.MinInt16 && value <= math.MaxInt16 {
			return exactInt2TypeName, true
		}
		return "BIGINT", true
	case "INTEGER", "INT":
		if value, ok := constantNumericValue(operand); ok && value >= math.MinInt32 && value <= math.MaxInt32 {
			return "INTEGER", true
		}
		return "BIGINT", true
	case "TINYINT", "MEDIUMINT", "BIGINT":
		return "BIGINT", true
	case "REAL", "FLOAT":
		// float4 cannot represent a magnitude above MaxFloat32; the binary
		// encoder rejects such a value rather than sending an infinity.
		if value, ok := constantNumericValue(operand); ok && math.Abs(value) <= math.MaxFloat32 {
			return "REAL", true
		}
		return "DOUBLE", true
	case "DOUBLE":
		return "DOUBLE", true
	case "NUMERIC", "DECIMAL":
		// A scalar NUMERIC is float64-backed: the engine keeps no exact decimal
		// carrier and the wire reports float8 (701). NUMERIC[] keeps exact text.
		return "DOUBLE", true
	case "TEXT", "VARCHAR", "CHAR", "CHARACTER", "CLOB", "NCHAR", "NVARCHAR":
		return "TEXT", true
	case "BLOB":
		return "BLOB", true
	case "JSONB":
		return "JSONB", true
	case "REGCLASS":
		return "REGCLASS", true
	case "REGTYPE":
		return "REGTYPE", true
	case "OID":
		return "OID", true
	case "NAME":
		return "NAME", true
	default:
		return "", false
	}
}

// constantNumericValue reports the numeric value of a cast operand that is
// statically known: a numeric or string literal, optionally parenthesized or
// signed. It reports false for every other expression, including a NULL literal,
// so the caller keeps the carrier type when the value is not proven to fit.
func constantNumericValue(expr parser.Expr) (float64, bool) {
	switch e := expr.(type) {
	case *parser.ParenExpr:
		return constantNumericValue(e.Expr)
	case *parser.UnaryExpr:
		if e.Op != lexer.TokenMinus && e.Op != lexer.TokenPlus {
			return 0, false
		}
		value, ok := constantNumericValue(e.Operand)
		if !ok {
			return 0, false
		}
		if e.Op == lexer.TokenMinus {
			return -value, true
		}
		return value, true
	case *parser.LiteralExpr:
		switch e.Type {
		case lexer.TokenNumber, lexer.TokenString:
			// evalCastExpr converts through toFloat, which parses the same
			// decimal text; a value that does not parse is not proven either.
			value, err := strconv.ParseFloat(e.Value, 64)
			if err != nil {
				return 0, false
			}
			return value, true
		case lexer.TokenTRUE:
			return 1, true
		case lexer.TokenFALSE:
			return 0, true
		default:
			return 0, false
		}
	default:
		return 0, false
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
	case "BIGINT", "INT8", "SMALLINT", exactInt2TypeName, "TINYINT", "MEDIUMINT":
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
	case "ARRAY_LENGTH", "JSONB_ARRAY_LENGTH":
		return "INTEGER"
	case "JSONB_TYPEOF", "JSONB_PRETTY":
		return "TEXT"
	case "TO_JSONB", "JSONB_SET", "JSONB_INSERT", "JSONB_REPLACE", "JSONB_REMOVE",
		"JSONB_STRIP_NULLS", "JSONB_BUILD_OBJECT", "JSONB_BUILD_ARRAY":
		return "JSONB"
	case "MIN", "MAX", "COALESCE", "IFNULL", "NVL", "NULLIF":
		// These return one of the input values unchanged, so the result type
		// follows the first argument (and therefore keeps JSONB).
		return firstArgType(fn, resolve)
	case "TO_REGCLASS":
		return "REGCLASS"
	case "TO_REGTYPE":
		return "REGTYPE"
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
		lexer.TokenAND, lexer.TokenOR,
		lexer.TokenAtArrow, lexer.TokenLtAt,
		lexer.TokenQuestion, lexer.TokenQuestionPipe, lexer.TokenQuestionAmp:
		return "BOOLEAN"
	case lexer.TokenConcat:
		// jsonb || jsonb merges or concatenates; text || text concatenates.
		if strings.EqualFold(inferExprType(e.Left, resolve), "JSONB") ||
			strings.EqualFold(inferExprType(e.Right, resolve), "JSONB") {
			return "JSONB"
		}
		return "TEXT"
	case lexer.TokenArrow, lexer.TokenHashArrow, lexer.TokenHashMinus:
		// ->, #>, and #- keep the jsonb type when the document is jsonb; JSON1
		// text extraction reports text.
		if strings.EqualFold(inferExprType(e.Left, resolve), "JSONB") {
			return "JSONB"
		}
		return "TEXT"
	case lexer.TokenArrowText, lexer.TokenHashArrowText:
		return "TEXT"
	case lexer.TokenBitAnd, lexer.TokenBitOr, lexer.TokenShiftLeft, lexer.TokenShiftRight:
		return "BIGINT"
	case lexer.TokenMinus:
		if strings.EqualFold(inferExprType(e.Left, resolve), "JSONB") {
			return "JSONB"
		}
		fallthrough
	case lexer.TokenPlus, lexer.TokenStar, lexer.TokenSlash, lexer.TokenPercent:
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
	case "INTEGER", "INT", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT", exactInt2TypeName:
		return true
	default:
		return false
	}
}

// carrierColumnTypes widens exact-width result types to their carrier. A compound
// SELECT reports the left branch's metadata but also contains the right branch's
// values, which were never checked against the narrower type the left branch
// proved.
func carrierColumnTypes(types []string) []string {
	wides := make([]string, len(types))
	for i, typeName := range types {
		if strings.EqualFold(typeName, exactInt2TypeName) {
			wides[i] = "BIGINT"
			continue
		}
		wides[i] = typeName
	}
	return wides
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
