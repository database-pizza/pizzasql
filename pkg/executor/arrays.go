package executor

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// Array values are carried as their canonical PostgreSQL array-literal text.
// Storing the text keeps the row codec unchanged (a string is a string), makes
// NULL and the empty array distinguishable (nil vs "{}"), and gives reloads a
// stable encoding: every write path canonicalizes before storage, so equal
// arrays always encode to identical bytes.
//
// The canonical form uses unquoted elements when the element text is unambiguous
// and double-quoted (backslash-escaped) elements otherwise. Element text is
// canonicalized per declared element type: integers to decimal, reals to Go's
// shortest round-trip form, booleans to t/f.

// arrayElementKind classifies an array element type for canonicalization.
type arrayElementKind int

const (
	arrayElementText arrayElementKind = iota
	arrayElementInteger
	arrayElementReal
	arrayElementNumeric
	arrayElementBoolean
)

// arrayElementType reports the base element type of an array type name such as
// "INTEGER[]". It reports false for non-array type names.
func arrayElementType(typeName string) (string, bool) {
	upper := strings.ToUpper(strings.TrimSpace(typeName))
	if !strings.HasSuffix(upper, "[]") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimSuffix(upper, "[]")), true
}

// arrayElementKindOf reports the canonicalization kind for an array type name.
func arrayElementKindOf(typeName string) (arrayElementKind, error) {
	element, ok := arrayElementType(typeName)
	if !ok {
		return arrayElementText, fmt.Errorf("%s is not an array type", typeName)
	}
	return arrayElementKindForName(element)
}

// arrayElementKindForName classifies a scalar element type name.
func arrayElementKindForName(name string) (arrayElementKind, error) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "TEXT", "VARCHAR", "CHAR", "CHARACTER", "CLOB", "NCHAR", "NVARCHAR", "UUID":
		return arrayElementText, nil
	case "INTEGER", "INT", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT":
		return arrayElementInteger, nil
	case "REAL", "FLOAT", "DOUBLE":
		return arrayElementReal, nil
	case "NUMERIC", "DECIMAL":
		// NUMERIC elements keep their exact text: the engine's float64 carrier
		// would lose digits above 2^53 and trailing zeros.
		return arrayElementNumeric, nil
	case "BOOLEAN", "BOOL":
		return arrayElementBoolean, nil
	default:
		return arrayElementText, pgerror.Unsupported("arrays of %s are not supported; supported element types are TEXT, VARCHAR, INTEGER, BIGINT, SMALLINT, BOOLEAN, REAL, DOUBLE PRECISION, and NUMERIC", strings.ToUpper(name))
	}
}

// parseArrayLiteral splits PostgreSQL array-literal text into element strings.
// A nil element is SQL NULL. Nested braces (multidimensional arrays) are
// rejected, as is malformed quoting.
func parseArrayLiteral(text string) ([]*string, error) {
	s := strings.TrimSpace(text)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil, invalidArrayInput(text, "array text must be enclosed in {}")
	}
	inner := s[1 : len(s)-1]
	if strings.TrimSpace(inner) == "" {
		return nil, nil
	}

	var elements []*string
	i := 0
	for {
		for i < len(inner) && isArraySpace(inner[i]) {
			i++
		}
		if i >= len(inner) {
			return nil, invalidArrayInput(text, "missing } after comma")
		}
		var value *string
		if inner[i] == '"' {
			i++
			var b strings.Builder
			closed := false
			for i < len(inner) {
				c := inner[i]
				if c == '\\' {
					if i+1 >= len(inner) {
						return nil, invalidArrayInput(text, "trailing backslash in quoted element")
					}
					b.WriteByte(inner[i+1])
					i += 2
					continue
				}
				if c == '"' {
					i++
					closed = true
					break
				}
				b.WriteByte(c)
				i++
			}
			if !closed {
				return nil, invalidArrayInput(text, "unterminated quoted element")
			}
			quoted := b.String()
			value = &quoted
			for i < len(inner) && isArraySpace(inner[i]) {
				i++
			}
		} else {
			if inner[i] == '{' {
				return nil, pgerror.Unsupported("multidimensional arrays are not supported; array text must be one-dimensional")
			}
			start := i
			for i < len(inner) && inner[i] != ',' {
				if inner[i] == '{' || inner[i] == '}' || inner[i] == '"' {
					return nil, invalidArrayInput(text, "malformed array element")
				}
				i++
			}
			raw := strings.TrimSpace(inner[start:i])
			if raw == "" {
				return nil, invalidArrayInput(text, "empty array element")
			}
			if !strings.EqualFold(raw, "NULL") {
				value = &raw
			}
		}

		elements = append(elements, value)
		if i >= len(inner) {
			return elements, nil
		}
		if inner[i] != ',' {
			return nil, invalidArrayInput(text, "expected , between elements")
		}
		i++
		if i >= len(inner) {
			return nil, invalidArrayInput(text, "trailing comma")
		}
	}
}

func isArraySpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// invalidArrayInput reports malformed array text as a client input error.
func invalidArrayInput(text, reason string) error {
	return pgerror.InvalidText("invalid array input %q: %s", text, reason)
}

// formatArrayLiteral renders elements in canonical array text.
func formatArrayLiteral(elements []*string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, element := range elements {
		if i > 0 {
			b.WriteByte(',')
		}
		if element == nil {
			b.WriteString("NULL")
			continue
		}
		b.WriteString(formatArrayElementText(*element))
	}
	b.WriteByte('}')
	return b.String()
}

// formatArrayElementText quotes an element when leaving it bare would change its
// meaning: empty text, the NULL keyword, or characters that delimit elements.
func formatArrayElementText(text string) string {
	needsQuote := text == "" || strings.EqualFold(text, "NULL")
	for i := 0; i < len(text) && !needsQuote; i++ {
		switch text[i] {
		case '{', '}', ',', '"', '\\', ' ', '\t', '\n', '\r':
			needsQuote = true
		}
	}
	if !needsQuote {
		return text
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(text); i++ {
		if text[i] == '"' || text[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(text[i])
	}
	b.WriteByte('"')
	return b.String()
}

// CanonicalizeArrayText validates PostgreSQL array text for an array type and
// returns its canonical encoding. The protocol layer uses it to reject a
// malformed bound array parameter at Bind time instead of waiting for a row to
// evaluate the predicate.
func CanonicalizeArrayText(text, typeName string) (string, error) {
	return canonicalArrayText(text, typeName)
}

// ParseArrayText splits stored array text into its element strings, with nil for
// a SQL NULL element. The wire layer reuses it to encode the same text in the
// PostgreSQL binary array format, so both formats are produced from one
// canonical representation.
func ParseArrayText(text string) ([]*string, error) {
	return parseArrayLiteral(text)
}

// canonicalArrayText parses array text and re-encodes it for the given array
// type, converting each element to its canonical spelling.
func canonicalArrayText(text, typeName string) (string, error) {
	kind, err := arrayElementKindOf(typeName)
	if err != nil {
		return "", err
	}
	elements, err := parseArrayLiteral(text)
	if err != nil {
		return "", err
	}
	canonical := make([]*string, len(elements))
	for i, element := range elements {
		value, elementErr := canonicalArrayElement(element, kind, typeName)
		if elementErr != nil {
			return "", elementErr
		}
		canonical[i] = value
	}
	return formatArrayLiteral(canonical), nil
}

// canonicalArrayElement converts one element to its canonical text. Element
// types the engine does not carry exactly (integer widths, floating point) are
// parsed and re-formatted so equivalent inputs encode identically.
func canonicalArrayElement(element *string, kind arrayElementKind, typeName string) (*string, error) {
	if element == nil {
		return nil, nil
	}
	raw := *element
	switch kind {
	case arrayElementInteger:
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, pgerror.InvalidText("invalid input syntax for type %s: %q", typeName, raw)
		}
		canonical := strconv.FormatInt(n, 10)
		return &canonical, nil
	case arrayElementReal:
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, pgerror.InvalidText("invalid input syntax for type %s: %q", typeName, raw)
		}
		canonical := strconv.FormatFloat(f, 'g', -1, 64)
		return &canonical, nil
	case arrayElementNumeric:
		canonical, ok := normalizeNumericArrayElement(raw)
		if !ok {
			return nil, pgerror.InvalidText("invalid input syntax for type %s: %q", typeName, raw)
		}
		return &canonical, nil
	case arrayElementBoolean:
		value, ok := parseArrayBoolean(raw)
		if !ok {
			return nil, pgerror.InvalidText("invalid input syntax for type %s: %q", typeName, raw)
		}
		canonical := "f"
		if value {
			canonical = "t"
		}
		return &canonical, nil
	default:
		canonical := raw
		return &canonical, nil
	}
}

// parseArrayBoolean accepts the boolean spellings PostgreSQL accepts in a text
// array element and reports the value.
func parseArrayBoolean(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "t", "true", "1", "y", "yes", "on":
		return true, true
	case "f", "false", "0", "n", "no", "off":
		return false, true
	default:
		return false, false
	}
}

// arrayElementValue converts canonical element text to the Go value the engine
// uses for that element type. Text elements stay strings.
func arrayElementValue(element *string, elementType string) (interface{}, error) {
	if element == nil {
		return nil, nil
	}
	kind, err := arrayElementKindForName(elementType)
	if err != nil {
		return nil, err
	}
	raw := *element
	switch kind {
	case arrayElementInteger:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, pgerror.InvalidText("invalid input syntax for type integer: %q", raw)
		}
		return n, nil
	case arrayElementReal:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, pgerror.InvalidText("invalid input syntax for type double precision: %q", raw)
		}
		return f, nil
	case arrayElementNumeric:
		canonical, ok := normalizeNumericArrayElement(raw)
		if !ok {
			return nil, pgerror.InvalidText("invalid input syntax for type numeric: %q", raw)
		}
		// Return the text so a value above 2^53 keeps every digit.
		return canonical, nil
	case arrayElementBoolean:
		value, ok := parseArrayBoolean(raw)
		if !ok {
			return nil, pgerror.InvalidText("invalid input syntax for type boolean: %q", raw)
		}
		return value, nil
	default:
		return raw, nil
	}
}

// normalizeNumericArrayElement validates SQL NUMERIC syntax and returns the
// canonical spelling: surrounding whitespace trimmed and a leading + removed.
// Go's ParseFloat is not used because it also accepts hex floats, NaN, and Inf,
// which NUMERIC has no representation for here.
func normalizeNumericArrayElement(raw string) (string, bool) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", false
	}
	if text[0] == '+' {
		text = text[1:]
	}
	if text == "" {
		return "", false
	}
	i := 0
	if text[i] == '-' {
		i++
	}
	digits := 0
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
		digits++
	}
	if i < len(text) && text[i] == '.' {
		i++
		for i < len(text) && text[i] >= '0' && text[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return "", false
	}
	if i < len(text) && (text[i] == 'e' || text[i] == 'E') {
		i++
		if i < len(text) && (text[i] == '+' || text[i] == '-') {
			i++
		}
		exponentDigits := 0
		for i < len(text) && text[i] >= '0' && text[i] <= '9' {
			i++
			exponentDigits++
		}
		if exponentDigits == 0 {
			return "", false
		}
	}
	if i != len(text) {
		return "", false
	}
	return text, true
}

// numericValueRat converts an evaluated value to an exact rational. Strings and
// floats use their decimal spelling, so a column value "1.10" and the float
// literal 1.1 both compare as 11/10 rather than through binary rounding.
func numericValueRat(value interface{}) (*big.Rat, bool) {
	var text string
	switch v := value.(type) {
	case string:
		normalized, ok := normalizeNumericArrayElement(v)
		if !ok {
			return nil, false
		}
		text = normalized
	case int64:
		return new(big.Rat).SetInt64(v), true
	case int:
		return new(big.Rat).SetInt64(int64(v)), true
	case float64:
		text = strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return nil, false
	}
	rat, ok := new(big.Rat).SetString(text)
	return rat, ok
}

// compareNumericValues compares two values exactly when both are numeric.
func compareNumericValues(left, right interface{}) (int, bool) {
	leftRat, ok := numericValueRat(left)
	if !ok {
		return 0, false
	}
	rightRat, ok := numericValueRat(right)
	if !ok {
		return 0, false
	}
	return leftRat.Cmp(rightRat), true
}

// numericArrayTextEqual reports whether two canonical NUMERIC array texts are
// equal element-wise, with NULL elements equal to each other. Other array types
// compare by their canonical bytes.
func numericArrayTextEqual(left, right, typeName string) (bool, error) {
	kind, err := arrayElementKindOf(typeName)
	if err != nil {
		return false, err
	}
	if kind != arrayElementNumeric {
		return left == right, nil
	}
	leftElements, err := parseArrayLiteral(left)
	if err != nil {
		return false, err
	}
	rightElements, err := parseArrayLiteral(right)
	if err != nil {
		return false, err
	}
	if len(leftElements) != len(rightElements) {
		return false, nil
	}
	for i := range leftElements {
		leftElement, rightElement := leftElements[i], rightElements[i]
		if leftElement == nil || rightElement == nil {
			if leftElement != nil || rightElement != nil {
				return false, nil
			}
			continue
		}
		cmp, ok := compareNumericValues(*leftElement, *rightElement)
		if !ok {
			return false, pgerror.InvalidText("invalid input syntax for type numeric: %q", *leftElement)
		}
		if cmp != 0 {
			return false, nil
		}
	}
	return true, nil
}

// arrayValueElement renders one evaluated expression value as a canonical array
// element. Array nesting and non-scalar values are rejected.
func arrayValueElement(value interface{}) (*string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case bool:
		text := "f"
		if v {
			text = "t"
		}
		return &text, nil
	case int64:
		text := strconv.FormatInt(v, 10)
		return &text, nil
	case int:
		text := strconv.FormatInt(int64(v), 10)
		return &text, nil
	case float64:
		text := strconv.FormatFloat(v, 'g', -1, 64)
		return &text, nil
	case string:
		return &v, nil
	case []byte:
		return nil, pgerror.Unsupported("arrays of BLOB are not supported")
	default:
		return nil, pgerror.Unsupported("ARRAY[...] element type %T is not supported", value)
	}
}

// evalArrayExpr evaluates the ARRAY[...] constructor into canonical array text.
func (e *Executor) evalArrayExpr(expr *parser.ArrayExpr, row storage.Row) (interface{}, error) {
	elements := make([]*string, len(expr.Elements))
	for i, element := range expr.Elements {
		value, err := e.evalExpr(element, row)
		if err != nil {
			return nil, err
		}
		text, err := arrayValueElement(value)
		if err != nil {
			return nil, fmt.Errorf("ARRAY element %d: %w", i+1, err)
		}
		elements[i] = text
	}
	return formatArrayLiteral(elements), nil
}

// arrayExpressionIsConstant reports whether evaluating an array expression once
// per statement is safe: it references no columns and no volatile function.
func arrayExpressionIsConstant(expr parser.Expr) bool {
	if expr == nil || len(collectColumnRefs(expr)) != 0 {
		return false
	}
	return ValidateDeterministicExpr(expr) == nil
}

// arrayOperand evaluates an array-valued expression through the per-statement
// value cache. A constant operand is evaluated once for the whole scan instead
// of once per row; a column-dependent operand is re-evaluated every time.
func (e *Executor) arrayOperand(expr parser.Expr, evaluate func() (interface{}, error)) (interface{}, error) {
	if e.arrayValueCache != nil && expr != nil {
		if value, ok := e.arrayValueCache[expr]; ok {
			return value, nil
		}
	}
	value, err := evaluate()
	if err != nil {
		return nil, err
	}
	if e.arrayValueCache != nil && expr != nil && arrayExpressionIsConstant(expr) {
		e.arrayValueCache[expr] = value
	}
	return value, nil
}

// arrayParseEntry memoizes the parsed elements of one array expression for the
// current statement. The evaluated text is compared before a hit is used, so a
// value that changes between rows or between executions is re-parsed.
type arrayParseEntry struct {
	text     string
	elements []*string
	err      error
}

// parsedArray parses an evaluated array value for one expression. The
// per-statement cache keyed by AST node keeps `= ANY($1)` to one parse for the
// whole scan instead of one parse per row.
func (e *Executor) parsedArray(expr parser.Expr, value interface{}) ([]*string, error) {
	text := toString(value)
	if e.arrayCache != nil && expr != nil {
		if entry, ok := e.arrayCache[expr]; ok && entry.text == text {
			return entry.elements, entry.err
		}
	}
	elements, err := parseArrayLiteral(text)
	if e.arrayCache != nil && expr != nil {
		e.arrayCache[expr] = &arrayParseEntry{text: text, elements: elements, err: err}
	}
	return elements, err
}

// isNumericExprType reports whether an expression's declared type is NUMERIC or
// DECIMAL, which compares exactly instead of through float64. CAST(... AS
// NUMERIC) is included even though its result metadata stays unchanged: the
// cast leaves the value's carrier alone, but comparisons still mean decimal.
func (e *Executor) isNumericExprType(expr parser.Expr) bool {
	switch x := expr.(type) {
	case nil:
		return false
	case *parser.ParenExpr:
		return e.isNumericExprType(x.Expr)
	case *parser.CastExpr:
		switch strings.ToUpper(strings.TrimSpace(x.Type.Name)) {
		case "NUMERIC", "DECIMAL":
			return true
		}
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(inferExprType(expr, e.resolveColumnType))) {
	case "NUMERIC", "DECIMAL":
		return true
	default:
		return false
	}
}

// comparisonOperatorResult applies a comparison operator to a three-way result.
func comparisonOperatorResult(op lexer.TokenType, cmp int) (bool, error) {
	switch op {
	case lexer.TokenEq:
		return cmp == 0, nil
	case lexer.TokenNeq:
		return cmp != 0, nil
	case lexer.TokenLt:
		return cmp < 0, nil
	case lexer.TokenLte:
		return cmp <= 0, nil
	case lexer.TokenGt:
		return cmp > 0, nil
	case lexer.TokenGte:
		return cmp >= 0, nil
	default:
		return false, fmt.Errorf("unsupported comparison operator: %v", op)
	}
}

// arrayEqualityType reports the array type used for a whole-array equality
// comparison. The left side wins when both are arrays.
func (e *Executor) arrayEqualityType(leftExpr, rightExpr parser.Expr) (string, bool) {
	if typeName, ok := e.arrayTypeOf(leftExpr); ok {
		return typeName, true
	}
	return e.arrayTypeOf(rightExpr)
}

// compareExprValues applies a comparison operator to two evaluated values. A
// NUMERIC-typed operand compares exactly (including element-wise numeric array
// equality); everything else keeps the engine's existing comparison rules.
func (e *Executor) compareExprValues(op lexer.TokenType, leftExpr, rightExpr parser.Expr, left, right interface{}) (bool, error) {
	if e.isJSONBExpr(leftExpr) || e.isJSONBExpr(rightExpr) {
		// A jsonb comparison must not silently reinterpret a SQL number or
		// boolean as a JSON value; PostgreSQL has no such operator.
		if leftExpr != nil && e.isJSONBExpr(leftExpr) && !jsonbComparisonValue(left) {
			return false, pgerror.DatatypeMismatch("jsonb comparison requires jsonb or text operands")
		}
		if rightExpr != nil && e.isJSONBExpr(rightExpr) && !jsonbComparisonValue(right) {
			return false, pgerror.DatatypeMismatch("jsonb comparison requires jsonb or text operands")
		}
		// jsonb equality and ordering are defined on the canonical form, so a
		// document with a different key order or number spelling compares equal.
		leftText, err := CanonicalizeJSONB(toString(left))
		if err != nil {
			return false, err
		}
		rightText, err := CanonicalizeJSONB(toString(right))
		if err != nil {
			return false, err
		}
		// Compare exact canonical bytes; the general compare() helper would round
		// numbers through float64 and disagree with =, DISTINCT, and @>.
		return comparisonOperatorResult(op, strings.Compare(leftText, rightText))
	}
	if op == lexer.TokenEq || op == lexer.TokenNeq {
		if typeName, ok := e.arrayEqualityType(leftExpr, rightExpr); ok {
			equal, err := numericArrayTextEqual(toString(left), toString(right), typeName)
			if err != nil {
				return false, err
			}
			if op == lexer.TokenNeq {
				return !equal, nil
			}
			return equal, nil
		}
	}
	if e.isNumericExprType(leftExpr) || e.isNumericExprType(rightExpr) {
		if cmp, ok := compareNumericValues(left, right); ok {
			return comparisonOperatorResult(op, cmp)
		}
	}
	return comparisonOperatorResult(op, compare(left, right))
}

// arrayElementTypeOf reports the element type of an expression whose value is an
// array, resolved against the declared column types of the query currently
// executing. An unknown type (a derived table, a bare literal) reports false so
// the caller keeps the element text.
func (e *Executor) arrayElementTypeOf(expr parser.Expr) (string, bool) {
	if expr == nil {
		return "", false
	}
	return arrayElementType(inferExprType(expr, e.resolveColumnType))
}

// arrayIndexValue validates an array subscript or array_length dimension.
// PostgreSQL's signatures take integers; floats, booleans, and text are a
// datatype mismatch, not an implicit truncation.
func arrayIndexValue(value interface{}, what string) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int8:
		return int64(v), nil
	default:
		return 0, pgerror.DatatypeMismatch("%s must be an integer, got %T", what, value)
	}
}

// evalSubscriptExpr evaluates arr[index] with PostgreSQL's 1-based indexing. An
// out-of-range subscript yields NULL, and a NULL array, index, or element yields
// NULL. The element is decoded to the array's declared element type so a
// boolean subscript is a Go bool rather than the canonical t/f text.
func (e *Executor) evalSubscriptExpr(expr *parser.SubscriptExpr, row storage.Row) (interface{}, error) {
	value, err := e.arrayOperand(expr.Expr, func() (interface{}, error) {
		return e.evalExpr(expr.Expr, row)
	})
	if err != nil {
		return nil, err
	}
	index, err := e.evalExpr(expr.Index, row)
	if err != nil {
		return nil, err
	}
	return e.subscriptArrayValue(expr.Expr, value, index)
}

// subscriptArrayValue applies a 1-based subscript to an evaluated array value.
func (e *Executor) subscriptArrayValue(arrayExpr parser.Expr, value, index interface{}) (interface{}, error) {
	if value == nil || index == nil {
		return nil, nil
	}
	position, err := arrayIndexValue(index, "array subscript")
	if err != nil {
		return nil, err
	}
	elements, err := e.parsedArray(arrayExpr, value)
	if err != nil {
		return nil, fmt.Errorf("array subscript requires an array value: %w", err)
	}
	if position < 1 || position > int64(len(elements)) {
		return nil, nil
	}
	element := elements[position-1]
	if element == nil {
		return nil, nil
	}
	elementType, ok := e.arrayElementTypeOf(arrayExpr)
	if !ok {
		return *element, nil
	}
	return arrayElementValue(element, elementType)
}

// evalAnyAllExpr evaluates `left op ANY(array)` / `left op ALL(array)` with SQL
// three-valued logic. ANY is true when any comparison is true, false when none
// is true and no comparison is NULL, and NULL otherwise; ALL is its dual.
func (e *Executor) evalAnyAllExpr(expr *parser.AnyAllExpr, row storage.Row) (interface{}, error) {
	left, err := e.evalExpr(expr.Left, row)
	if err != nil {
		return nil, err
	}
	arrayValue, err := e.arrayOperand(expr.Array, func() (interface{}, error) {
		return e.evalExpr(expr.Array, row)
	})
	if err != nil {
		return nil, err
	}
	if arrayValue == nil {
		return nil, nil
	}
	elements, err := e.parsedArray(expr.Array, arrayValue)
	if err != nil {
		return nil, fmt.Errorf("ANY/ALL requires an array operand: %w", err)
	}
	elementType, _ := e.arrayElementTypeOf(expr.Array)
	return evalQuantifiedComparison(expr.Op, expr.All, left, elements, elementType)
}

// evalQuantifiedComparison applies a comparison to each array element. The
// empty array is decided before the left operand: ANY over no elements is
// false and ALL over no elements is true, even when the left operand is NULL.
func evalQuantifiedComparison(op lexer.TokenType, all bool, left interface{}, elements []*string, elementType string) (interface{}, error) {
	if len(elements) == 0 {
		return all, nil
	}
	if left == nil {
		return nil, nil
	}
	sawNull := false
	for _, element := range elements {
		if element == nil {
			sawNull = true
			continue
		}
		matched, err := compareArrayElement(op, left, *element, elementType)
		if err != nil {
			return nil, err
		}
		if all && !matched {
			return false, nil
		}
		if !all && matched {
			return true, nil
		}
	}
	if sawNull {
		return nil, nil
	}
	return all, nil
}

// compareArrayElement applies a comparison operator between a scalar left value
// and one array element's text. Booleans are decoded so `true = ANY('{t,f}')`
// compares as booleans; every other type compares through the engine's numeric
// or textual comparison rules.
func compareArrayElement(op lexer.TokenType, left interface{}, element, elementType string) (bool, error) {
	if kind, err := arrayElementKindForName(elementType); err == nil && kind == arrayElementNumeric {
		cmp, ok := compareNumericValues(left, element)
		if !ok {
			return false, pgerror.InvalidText("invalid input syntax for type numeric: %q", element)
		}
		return comparisonOperatorResult(op, cmp)
	}
	var right interface{} = element
	if _, isBool := left.(bool); isBool {
		value, ok := parseArrayBoolean(element)
		if !ok {
			return false, pgerror.InvalidText("invalid input syntax for type boolean: %q", element)
		}
		right = value
	}
	return comparisonOperatorResult(op, compare(left, right))
}

// evalArrayLength implements array_length(array, dimension). Only dimension 1 is
// meaningful for a one-dimensional array; any other dimension yields NULL, as
// PostgreSQL does for an out-of-range dimension of a non-nested array.
func (e *Executor) evalArrayLength(arrayExpr parser.Expr, args []interface{}) (interface{}, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("array_length requires two arguments")
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	dimension, err := arrayIndexValue(args[1], "array dimension")
	if err != nil {
		return nil, err
	}
	if dimension != 1 {
		return nil, nil
	}
	elements, err := e.parsedArray(arrayExpr, args[0])
	if err != nil {
		return nil, fmt.Errorf("array_length requires an array value: %w", err)
	}
	if len(elements) == 0 {
		// PostgreSQL reports NULL for the length of an empty array.
		return nil, nil
	}
	return int64(len(elements)), nil
}

// arrayTypeOf reports the declared array type of an expression, if any.
func (e *Executor) arrayTypeOf(expr parser.Expr) (string, bool) {
	if expr == nil {
		return "", false
	}
	typeName := inferExprType(expr, e.resolveColumnType)
	if _, ok := arrayElementType(typeName); !ok {
		return "", false
	}
	return typeName, true
}

// arrayCanonicalEntry memoizes one canonicalized array comparison operand for
// the current statement, validated by the source text and target type.
type arrayCanonicalEntry struct {
	text      string
	typeName  string
	canonical string
	err       error
}

// canonicalArrayValue canonicalizes a value against the array type of expr. It
// is a no-op for non-array expressions, NULL, and non-string values. The result
// is memoized per AST node and source text, so a constant literal on one side of
// a scan predicate is canonicalized once per statement.
func (e *Executor) canonicalArrayValue(arrayExpr parser.Expr, value interface{}) (interface{}, error) {
	typeName, ok := e.arrayTypeOf(arrayExpr)
	if !ok || value == nil {
		return value, nil
	}
	text, isText := value.(string)
	if !isText {
		return value, nil
	}
	if e.arrayCanonicalCache != nil && arrayExpr != nil {
		if entry, cached := e.arrayCanonicalCache[arrayExpr]; cached && entry.text == text && entry.typeName == typeName {
			if entry.err != nil {
				return nil, entry.err
			}
			return entry.canonical, nil
		}
	}
	canonical, err := canonicalArrayText(text, typeName)
	if e.arrayCanonicalCache != nil && arrayExpr != nil {
		e.arrayCanonicalCache[arrayExpr] = &arrayCanonicalEntry{text: text, typeName: typeName, canonical: canonical, err: err}
	}
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

// canonicalizeArrayComparison canonicalizes the string operand of a comparison
// when the other operand is array-typed, so a text literal such as '{a, b}' or
// '{"a","b"}' compares equal to the canonical stored '{a,b}'.
func (e *Executor) canonicalizeArrayComparison(leftExpr, rightExpr parser.Expr, left, right interface{}) (interface{}, interface{}, error) {
	if _, ok := e.arrayTypeOf(leftExpr); ok {
		canonical, err := e.canonicalArrayValue(leftExpr, right)
		if err != nil {
			return nil, nil, err
		}
		right = canonical
	}
	if _, ok := e.arrayTypeOf(rightExpr); ok {
		canonical, err := e.canonicalArrayValue(rightExpr, left)
		if err != nil {
			return nil, nil, err
		}
		left = canonical
	}
	return left, right, nil
}

// evalArrayCast converts a value to an array type. The input must already be
// array text (a literal, a parameter, or another array value); every element is
// validated and re-encoded canonically for the target element type.
func evalArrayCast(target parser.DataType, value interface{}) (interface{}, error) {
	if value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, pgerror.InvalidText("cannot cast %T to %s", value, target.SQLTypeName())
	}
	return canonicalArrayText(text, target.SQLTypeName())
}

// canonicalArrayDefault canonicalizes a constant DEFAULT for an array column so
// the catalog and any ALTER TABLE backfill store the same bytes an INSERT would.
func canonicalArrayDefault(typeName string, value interface{}, exprText string) (interface{}, string, error) {
	if _, isArray := arrayElementType(typeName); !isArray || exprText != "" || value == nil {
		return value, exprText, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, "", pgerror.InvalidText("DEFAULT for %s must be array text, got %T", typeName, value)
	}
	canonical, err := canonicalArrayText(text, typeName)
	if err != nil {
		return nil, "", err
	}
	return canonical, "", nil
}

// normalizeArrayRow canonicalizes every array-typed column value in a row before
// it is written. A string value is parsed as array text; an already-canonical
// value is re-encoded identically, so the encoding is stable across reloads and
// every write path shares one representation.
func (e *Executor) normalizeArrayRow(schema *storage.Schema, row storage.Row) error {
	if schema == nil {
		return nil
	}
	for _, column := range schema.Columns {
		if _, isArray := arrayElementType(column.Type); !isArray {
			continue
		}
		value, ok := lookupRowValue(row, column.Name)
		if !ok || value == nil {
			continue
		}
		text, isText := value.(string)
		if !isText {
			return pgerror.InvalidText("column %q is %s and accepts array text, got %T", column.Name, column.Type, value)
		}
		canonical, err := canonicalArrayText(text, column.Type)
		if err != nil {
			return fmt.Errorf("column %q: %w", column.Name, err)
		}
		row[column.Name] = canonical
	}
	return nil
}
