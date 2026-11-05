package executor

import (
	"fmt"
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
	case "REAL", "FLOAT", "DOUBLE", "NUMERIC", "DECIMAL":
		return arrayElementReal, nil
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
		return strconv.ParseInt(raw, 10, 64)
	case arrayElementReal:
		return strconv.ParseFloat(raw, 64)
	case arrayElementBoolean:
		value, _ := parseArrayBoolean(raw)
		return value, nil
	default:
		return raw, nil
	}
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

// evalSubscriptExpr evaluates arr[index] with PostgreSQL's 1-based indexing. An
// out-of-range subscript yields NULL, and a NULL array, index, or element yields
// NULL.
func (e *Executor) evalSubscriptExpr(expr *parser.SubscriptExpr, row storage.Row) (interface{}, error) {
	value, err := e.evalExpr(expr.Expr, row)
	if err != nil {
		return nil, err
	}
	index, err := e.evalExpr(expr.Index, row)
	if err != nil {
		return nil, err
	}
	return subscriptArrayValue(value, index)
}

// subscriptArrayValue applies a 1-based subscript to an evaluated array value.
func subscriptArrayValue(value, index interface{}) (interface{}, error) {
	if value == nil || index == nil {
		return nil, nil
	}
	elements, err := parseArrayLiteral(toString(value))
	if err != nil {
		return nil, fmt.Errorf("array subscript requires an array value: %w", err)
	}
	position := toInt64(index)
	if position < 1 || position > int64(len(elements)) {
		return nil, nil
	}
	element := elements[position-1]
	if element == nil {
		return nil, nil
	}
	return *element, nil
}

// evalAnyAllExpr evaluates `left op ANY(array)` / `left op ALL(array)` with SQL
// three-valued logic. ANY is true when any comparison is true, false when none
// is true and no comparison is NULL, and NULL otherwise; ALL is its dual.
func (e *Executor) evalAnyAllExpr(expr *parser.AnyAllExpr, row storage.Row) (interface{}, error) {
	left, err := e.evalExpr(expr.Left, row)
	if err != nil {
		return nil, err
	}
	arrayValue, err := e.evalExpr(expr.Array, row)
	if err != nil {
		return nil, err
	}
	if arrayValue == nil {
		return nil, nil
	}
	elements, err := parseArrayLiteral(toString(arrayValue))
	if err != nil {
		return nil, fmt.Errorf("ANY/ALL requires an array operand: %w", err)
	}
	return evalQuantifiedComparison(expr.Op, expr.All, left, elements)
}

// evalQuantifiedComparison applies a comparison to each array element. The
// empty array is decided before the left operand: ANY over no elements is
// false and ALL over no elements is true, even when the left operand is NULL.
func evalQuantifiedComparison(op lexer.TokenType, all bool, left interface{}, elements []*string) (interface{}, error) {
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
		matched, err := compareArrayElement(op, left, *element)
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
func compareArrayElement(op lexer.TokenType, left interface{}, element string) (bool, error) {
	var right interface{} = element
	if _, isBool := left.(bool); isBool {
		value, ok := parseArrayBoolean(element)
		if !ok {
			return false, pgerror.InvalidText("invalid input syntax for type boolean: %q", element)
		}
		right = value
	}
	cmp := compare(left, right)
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
		return false, fmt.Errorf("unsupported ANY/ALL comparison operator: %v", op)
	}
}

// evalArrayLength implements array_length(array, dimension). Only dimension 1 is
// meaningful for a one-dimensional array; any other dimension yields NULL, as
// PostgreSQL does for an out-of-range dimension of a non-nested array.
func evalArrayLength(args []interface{}) (interface{}, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("array_length requires two arguments")
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	if toInt64(args[1]) != 1 {
		return nil, nil
	}
	elements, err := parseArrayLiteral(toString(args[0]))
	if err != nil {
		return nil, fmt.Errorf("array_length requires an array value: %w", err)
	}
	if len(elements) == 0 {
		// PostgreSQL reports NULL for the length of an empty array.
		return nil, nil
	}
	return int64(len(elements)), nil
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
