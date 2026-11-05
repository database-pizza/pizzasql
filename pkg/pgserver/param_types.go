package pgserver

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// textParameterOID is the type a parameter falls back to when the SQL gives no
// usable context. PostgreSQL's Describe never reports OID 0 for a parameter:
// drivers such as pgx reject the statement client-side when the described
// parameter count or type disagrees with the arguments they were given.
const textParameterOID = 25

// markerPrefix identifies the string literal substituted for a $n placeholder
// while a statement is parsed for type inference. The parser has no parameter
// node, so each placeholder becomes a uniquely named string literal whose
// position in the AST supplies the surrounding comparison or assignment
// context.
const markerPrefix = "__pizzasql_parameter_"

// maxParameterNumber is PostgreSQL's protocol limit on parameter numbers. A
// Parse message addresses parameters by number and ParameterDescription counts
// them in a 16-bit field, so a larger placeholder can never be described; it is
// rejected before any allocation sized from the query text.
const maxParameterNumber = 65535

// parameterReference records one $n placeholder found by the quote- and
// comment-aware scan that Bind uses. number is clamped to maxParameterNumber+1
// when the digits exceed the protocol limit, and stays 0 for $0; Bind reports
// both as a missing parameter.
type parameterReference struct {
	number int
	start  int
	end    int
}

// negotiateParameterOIDs builds the ParameterDescription type list for query.
// The list length is the highest placeholder number (or the client's declared
// count when that is larger), client-declared non-zero OIDs win, and every
// remaining entry is inferred from the SQL or falls back to TEXT. A placeholder
// number above the protocol limit is an error, so a hostile query can never
// size the list.
func (c *Connection) negotiateParameterOIDs(query string, declared []int32) ([]int32, error) {
	references := scanParameters(query)
	count := len(declared)
	for _, reference := range references {
		if reference.number > maxParameterNumber {
			return nil, fmt.Errorf("parameter %s exceeds PostgreSQL's limit of %d parameters", query[reference.start:reference.end], maxParameterNumber)
		}
		if reference.number > count {
			count = reference.number
		}
	}
	if count > maxParameterNumber {
		return nil, fmt.Errorf("statement declares %d parameters; PostgreSQL's limit is %d", count, maxParameterNumber)
	}
	oids := make([]int32, count)
	copy(oids, declared)
	if count == 0 {
		return oids, nil
	}
	inferred := c.inferParameterTypes(query, references)
	for i := range oids {
		if oids[i] != 0 {
			continue
		}
		oids[i] = textParameterOID
		if typeName, ok := inferred[i+1]; ok {
			oids[i] = c.getOIDForType(typeName)
		}
	}
	return oids, nil
}

// inferParameterTypes returns the type name of every parameter whose SQL
// context names one. Inference is best effort: a statement the parser does not
// accept, or a column the catalog cannot resolve, leaves that parameter out so
// the caller falls back to TEXT.
func (c *Connection) inferParameterTypes(query string, references []parameterReference) map[int]string {
	if len(references) == 0 {
		return nil
	}
	marked, markers := substituteParameterMarkers(query, references)
	stmt, err := parser.New(lexer.New(marked)).Parse()
	if err != nil {
		return nil
	}
	inferrer := &parameterInferrer{
		conn:    c,
		markers: markers,
		types:   make(map[int]string),
	}
	inferrer.walkStatement(nil, stmt)
	return inferrer.types
}

// scanParameters locates $n placeholders outside string literals, quoted
// identifiers, and comments. Bind and parameter inference share it, so Describe
// sees exactly the placeholders Bind replaces.
func scanParameters(query string) []parameterReference {
	var references []parameterReference
	var quote byte
	for i := 0; i < len(query); {
		ch := query[i]
		if quote != 0 {
			i++
			if ch == quote {
				if i < len(query) && query[i] == quote {
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		if i+1 < len(query) && ch == '-' && query[i+1] == '-' {
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				break
			}
			i += end + 1
			continue
		}
		if i+1 < len(query) && ch == '/' && query[i+1] == '*' {
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				break
			}
			i += end + 4
			continue
		}
		// Brackets are ordinary syntax: a subscript such as tags[$1] binds $1.
		if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			i++
			continue
		}
		if ch == '$' && i+1 < len(query) && query[i+1] >= '0' && query[i+1] <= '9' {
			end := i + 1
			for end < len(query) && query[end] >= '0' && query[end] <= '9' {
				end++
			}
			// Clamp at the protocol limit instead of accumulating every digit:
			// a hostile $999... must not overflow or size an allocation.
			number := 0
			for _, digit := range query[i+1 : end] {
				if number > maxParameterNumber {
					break
				}
				number = number*10 + int(digit-'0')
				if number > maxParameterNumber {
					number = maxParameterNumber + 1
				}
			}
			references = append(references, parameterReference{number: number, start: i, end: end})
			i = end
			continue
		}
		i++
	}
	return references
}

// substituteParameterMarkers replaces each placeholder with a distinct string
// literal and reports marker text -> parameter number.
func substituteParameterMarkers(query string, references []parameterReference) (string, map[string]int) {
	var marked strings.Builder
	markers := make(map[string]int, len(references))
	last := 0
	for i, reference := range references {
		marker := markerPrefix + strconv.Itoa(i) + "__"
		markers[marker] = reference.number
		marked.WriteString(query[last:reference.start])
		marked.WriteString("'")
		marked.WriteString(marker)
		marked.WriteString("'")
		last = reference.end
	}
	marked.WriteString(query[last:])
	return marked.String(), markers
}

// parameterInferrer walks one parsed statement and records the type name each
// parameter occurrence is compared with or assigned to. The first resolved
// occurrence of a parameter wins.
type parameterInferrer struct {
	conn    *Connection
	markers map[string]int
	types   map[int]string
}

func (in *parameterInferrer) note(marker, typeName string) {
	number, ok := in.markers[marker]
	if !ok || typeName == "" {
		return
	}
	if _, recorded := in.types[number]; recorded {
		return
	}
	in.types[number] = typeName
}

// parameterScope is one query level's table set. Column types come from the
// storage catalog through the connection's session, so DDL staged by the
// current transaction is visible.
type parameterScope struct {
	parent *parameterScope
	tables []parameterTable
}

type parameterTable struct {
	name   string
	alias  string
	schema *storage.Schema
}

func (c *Connection) parameterTableSchema(name string) *storage.Schema {
	if name == "" {
		return nil
	}
	if c.executor != nil {
		if schema, err := c.executor.Session().GetSchema(name); err == nil {
			return schema
		}
		return nil
	}
	if c.schema != nil {
		if schema, err := c.schema.GetSchema(name); err == nil {
			return schema
		}
	}
	return nil
}

func (c *Connection) newParameterScope(parent *parameterScope, refs []parser.TableRef) *parameterScope {
	scope := &parameterScope{parent: parent}
	for _, ref := range refs {
		scope.addTable(c, ref)
	}
	return scope
}

func (s *parameterScope) addTable(c *Connection, ref parser.TableRef) {
	if ref.Subquery == nil && ref.Function == nil {
		if schema := c.parameterTableSchema(ref.Name); schema != nil {
			s.tables = append(s.tables, parameterTable{name: ref.Name, alias: ref.Alias, schema: schema})
		}
	}
	if ref.Join != nil && ref.Join.Table != nil {
		s.addTable(c, *ref.Join.Table)
	}
}

// columnType resolves a column reference to its catalog type name. An
// unqualified name is only used when exactly one table in the level has it; a
// qualified name that names a table without the column stops the search rather
// than falling through to an outer query.
func (s *parameterScope) columnType(ref *parser.ColumnRef) (string, bool) {
	for scope := s; scope != nil; scope = scope.parent {
		var found string
		matches, matchedTable := 0, false
		for _, table := range scope.tables {
			if ref.Table != "" && !strings.EqualFold(table.name, ref.Table) && !strings.EqualFold(table.alias, ref.Table) {
				continue
			}
			matchedTable = true
			for _, column := range table.schema.Columns {
				if !strings.EqualFold(column.Name, ref.Column) {
					continue
				}
				matches++
				found = column.Type
			}
		}
		if matches > 0 {
			return found, matches == 1
		}
		if ref.Table != "" && matchedTable {
			return "", false
		}
	}
	return "", false
}

func tableRefName(ref *parser.TableRef) string {
	if ref == nil {
		return ""
	}
	return ref.Name
}

func schemaColumnType(schema *storage.Schema, name string) string {
	if schema == nil {
		return ""
	}
	for _, column := range schema.Columns {
		if strings.EqualFold(column.Name, name) {
			return column.Type
		}
	}
	return ""
}

func (in *parameterInferrer) walkStatement(scope *parameterScope, stmt parser.Statement) {
	switch s := stmt.(type) {
	case *parser.SelectStmt:
		in.walkSelect(scope, s)
	case *parser.CompoundSelect:
		in.walkSelect(scope, s.Left)
		in.walkSelect(scope, s.Right)
		in.walkExpr(scope, s.Limit, "BIGINT")
		in.walkExpr(scope, s.Offset, "BIGINT")
	case *parser.InsertStmt:
		in.walkInsert(scope, s)
	case *parser.UpdateStmt:
		in.walkUpdate(scope, s)
	case *parser.DeleteStmt:
		in.walkDelete(scope, s)
	}
}

// walkTableRefs visits the bodies and join conditions of a FROM list. The scope
// already contains every table in the query level, so a join condition resolves
// columns from both sides.
func (in *parameterInferrer) walkTableRefs(scope *parameterScope, refs []parser.TableRef) {
	for _, ref := range refs {
		in.walkTableRef(scope, ref)
	}
}

func (in *parameterInferrer) walkTableRef(scope *parameterScope, ref parser.TableRef) {
	if ref.Subquery != nil {
		in.walkSelect(scope, ref.Subquery)
	}
	if ref.Function != nil {
		for _, arg := range ref.Function.Args {
			in.walkExpr(scope, arg, "")
		}
	}
	if ref.Join != nil {
		if ref.Join.Table != nil {
			in.walkTableRef(scope, *ref.Join.Table)
		}
		in.walkExpr(scope, ref.Join.Condition, "")
	}
}

func (in *parameterInferrer) walkSelect(parent *parameterScope, stmt *parser.SelectStmt) {
	if stmt == nil {
		return
	}
	for _, cte := range stmt.With {
		in.walkSelect(parent, cte.Query)
	}
	scope := in.conn.newParameterScope(parent, stmt.From)
	in.walkTableRefs(scope, stmt.From)
	for _, column := range stmt.Columns {
		in.walkExpr(scope, column.Expr, "")
	}
	in.walkExpr(scope, stmt.Where, "")
	for _, expr := range stmt.GroupBy {
		in.walkExpr(scope, expr, "")
	}
	in.walkExpr(scope, stmt.Having, "")
	for _, item := range stmt.OrderBy {
		in.walkExpr(scope, item.Expr, "")
	}
	in.walkExpr(scope, stmt.Limit, "BIGINT")
	in.walkExpr(scope, stmt.Offset, "BIGINT")
	if stmt.Compound != nil {
		in.walkStatement(parent, stmt.Compound)
	}
}

func (in *parameterInferrer) walkInsert(parent *parameterScope, stmt *parser.InsertStmt) {
	var target []parser.TableRef
	if stmt.Table != nil {
		target = append(target, *stmt.Table)
	}
	schema := in.conn.parameterTableSchema(tableRefName(stmt.Table))
	scope := in.conn.newParameterScope(parent, target)

	// Without an explicit column list, VALUES positions map to the table's
	// columns in declaration order.
	columnTypes := make([]string, 0, len(stmt.Columns))
	if len(stmt.Columns) == 0 {
		if schema != nil {
			for _, column := range schema.Columns {
				// Generated columns are not writable and are skipped when
				// positional values map to the table's columns.
				if column.GeneratedExpr != "" {
					continue
				}
				columnTypes = append(columnTypes, column.Type)
			}
		}
	} else {
		for _, name := range stmt.Columns {
			columnTypes = append(columnTypes, schemaColumnType(schema, name))
		}
	}

	for _, row := range stmt.Values {
		for i, value := range row {
			expected := ""
			if i < len(columnTypes) {
				expected = columnTypes[i]
			}
			in.walkExpr(scope, value, expected)
		}
	}
	in.walkSelect(parent, stmt.Select)
	for _, assignment := range stmt.ConflictUpdate {
		in.walkExpr(scope, assignment.Value, schemaColumnType(schema, assignment.Column))
	}
	for _, column := range stmt.Returning {
		in.walkExpr(scope, column.Expr, "")
	}
}

func (in *parameterInferrer) walkUpdate(parent *parameterScope, stmt *parser.UpdateStmt) {
	var target []parser.TableRef
	if stmt.Table != nil {
		target = append(target, *stmt.Table)
	}
	schema := in.conn.parameterTableSchema(tableRefName(stmt.Table))
	scope := in.conn.newParameterScope(parent, append(target, stmt.From...))
	in.walkTableRefs(scope, stmt.From)
	for _, assignment := range stmt.Set {
		in.walkExpr(scope, assignment.Value, schemaColumnType(schema, assignment.Column))
	}
	in.walkExpr(scope, stmt.Where, "")
	for _, column := range stmt.Returning {
		in.walkExpr(scope, column.Expr, "")
	}
}

func (in *parameterInferrer) walkDelete(parent *parameterScope, stmt *parser.DeleteStmt) {
	var target []parser.TableRef
	if stmt.Table != nil {
		target = append(target, *stmt.Table)
	}
	scope := in.conn.newParameterScope(parent, target)
	in.walkExpr(scope, stmt.Where, "")
	for _, column := range stmt.Returning {
		in.walkExpr(scope, column.Expr, "")
	}
}

// walkExpr visits expr with the type expected from its surrounding context.
// Only the operands of comparison and containment operators, assignment
// targets, and ANY/ALL operands supply a context; everything else recurses
// without one.
func (in *parameterInferrer) walkExpr(scope *parameterScope, expr parser.Expr, expected string) {
	switch e := expr.(type) {
	case nil:
		return
	case *parser.ParenExpr:
		in.walkExpr(scope, e.Expr, expected)
	case *parser.LiteralExpr:
		if e.Type == lexer.TokenString {
			in.note(e.Value, expected)
		}
	case *parser.CastExpr:
		if literal, ok := markerLiteral(e.Expr); ok {
			in.note(literal, e.Type.SQLTypeName())
			return
		}
		in.walkExpr(scope, e.Expr, "")
	case *parser.BinaryExpr:
		if isParameterTypeOperator(e.Op) {
			leftType, leftOK := in.exprType(scope, e.Left)
			rightType, rightOK := in.exprType(scope, e.Right)
			if leftOK {
				in.walkExpr(scope, e.Right, leftType)
			} else {
				in.walkExpr(scope, e.Right, "")
			}
			if rightOK {
				in.walkExpr(scope, e.Left, rightType)
			} else {
				in.walkExpr(scope, e.Left, "")
			}
			return
		}
		in.walkExpr(scope, e.Left, "")
		in.walkExpr(scope, e.Right, "")
	case *parser.AnyAllExpr:
		if columnType, ok := in.exprType(scope, e.Left); ok {
			in.walkExpr(scope, e.Array, parameterArrayType(columnType))
		} else {
			in.walkExpr(scope, e.Array, "")
		}
		in.walkExpr(scope, e.Left, "")
	case *parser.InExpr:
		columnType, ok := in.exprType(scope, e.Left)
		for _, value := range e.Values {
			if ok {
				in.walkExpr(scope, value, columnType)
			} else {
				in.walkExpr(scope, value, "")
			}
		}
		in.walkExpr(scope, e.Left, "")
		in.walkSelect(scope, e.Subquery)
	case *parser.BetweenExpr:
		if columnType, ok := in.exprType(scope, e.Left); ok {
			in.walkExpr(scope, e.Low, columnType)
			in.walkExpr(scope, e.High, columnType)
		} else {
			in.walkExpr(scope, e.Low, "")
			in.walkExpr(scope, e.High, "")
		}
		in.walkExpr(scope, e.Left, "")
	case *parser.IsDistinctExpr:
		leftType, leftOK := in.exprType(scope, e.Left)
		rightType, rightOK := in.exprType(scope, e.Right)
		if leftOK {
			in.walkExpr(scope, e.Right, leftType)
		} else {
			in.walkExpr(scope, e.Right, "")
		}
		if rightOK {
			in.walkExpr(scope, e.Left, rightType)
		} else {
			in.walkExpr(scope, e.Left, "")
		}
	case *parser.IsNullExpr:
		in.walkExpr(scope, e.Left, "")
	case *parser.LikeExpr:
		in.walkExpr(scope, e.Left, "")
		in.walkExpr(scope, e.Pattern, "TEXT")
		in.walkExpr(scope, e.Escape, "TEXT")
	case *parser.UnaryExpr:
		in.walkExpr(scope, e.Operand, "")
	case *parser.FunctionCall:
		for _, arg := range e.Args {
			in.walkExpr(scope, arg, "")
		}
	case *parser.WindowExpr:
		if e.Func != nil {
			for _, arg := range e.Func.Args {
				in.walkExpr(scope, arg, "")
			}
		}
		for _, expr := range e.PartitionBy {
			in.walkExpr(scope, expr, "")
		}
		for _, item := range e.OrderBy {
			in.walkExpr(scope, item.Expr, "")
		}
	case *parser.CaseExpr:
		in.walkExpr(scope, e.Operand, "")
		for _, when := range e.Whens {
			in.walkExpr(scope, when.Condition, "")
			in.walkExpr(scope, when.Result, expected)
		}
		in.walkExpr(scope, e.Else, expected)
	case *parser.ArrayExpr:
		for _, element := range e.Elements {
			in.walkExpr(scope, element, "")
		}
	case *parser.SubscriptExpr:
		in.walkExpr(scope, e.Expr, "")
		// An array subscript is an integer position regardless of the array's
		// element type.
		in.walkExpr(scope, e.Index, "INTEGER")
	case *parser.SubqueryExpr:
		in.walkSelect(scope, e.Query)
	case *parser.ExistsExpr:
		in.walkSelect(scope, e.Subquery)
	}
}

// exprType reports the type of an expression that names one directly: a column
// from the catalog or an explicit cast.
func (in *parameterInferrer) exprType(scope *parameterScope, expr parser.Expr) (string, bool) {
	switch e := expr.(type) {
	case *parser.ParenExpr:
		return in.exprType(scope, e.Expr)
	case *parser.ColumnRef:
		if scope == nil {
			return "", false
		}
		return scope.columnType(e)
	case *parser.CastExpr:
		return e.Type.SQLTypeName(), true
	case *parser.SubscriptExpr:
		// An array subscript has the column's element type, so
		// `nums[1] = $1` infers INTEGER from an INTEGER[] column.
		if baseType, ok := in.exprType(scope, e.Expr); ok {
			if element, isArray := arrayElementTypeName(baseType); isArray {
				return element, true
			}
		}
	}
	return "", false
}

// markerLiteral reports the parameter marker text when expr is a marker,
// looking through parentheses.
func markerLiteral(expr parser.Expr) (string, bool) {
	for {
		paren, ok := expr.(*parser.ParenExpr)
		if !ok {
			break
		}
		expr = paren.Expr
	}
	literal, ok := expr.(*parser.LiteralExpr)
	if !ok || literal.Type != lexer.TokenString || !strings.HasPrefix(literal.Value, markerPrefix) {
		return "", false
	}
	return literal.Value, true
}

// isParameterTypeOperator reports whether an operator's operands share a type,
// so one side can supply the other side's parameter type.
func isParameterTypeOperator(op lexer.TokenType) bool {
	switch op {
	case lexer.TokenEq, lexer.TokenNeq, lexer.TokenLt, lexer.TokenLte,
		lexer.TokenGt, lexer.TokenGte, lexer.TokenAtArrow, lexer.TokenLtAt:
		return true
	}
	return false
}

// parameterArrayType returns the array type of a column's element type. An
// array column keeps its declared array type: ANY/ALL over an array column is
// the engine's supported form.
func parameterArrayType(columnType string) string {
	if _, isArray := arrayElementTypeName(columnType); isArray {
		return columnType
	}
	return columnType + "[]"
}
