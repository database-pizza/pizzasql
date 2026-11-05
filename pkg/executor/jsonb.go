package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// JSONB values are carried as canonical JSON text: object keys sorted, duplicate
// keys collapsed last-wins, no insignificant whitespace, and numbers written as
// exact normalized decimals. The canonical form is idempotent and is what every
// write path stores, so a reload returns the same bytes and equality can be
// defined on them.
//
// A JSONB column is distinguished from JSON/TEXT by its declared type. The
// executor resolves that type from the statement's column-type scopes (the same
// mechanism arrays use), so operators and functions know whether a text value is
// a jsonb document or ordinary SQL text.

// maxJSONBNumberDigits bounds the decimal expansion of a jsonb number. It
// matches PostgreSQL's numeric digit limit and keeps a short exponent such as
// 1e1000000 from allocating an enormous canonical string.
const maxJSONBNumberDigits = 131072

// CanonicalizeJSONB validates JSON text and returns its canonical JSONB form.
// It is exported so the protocol layer can canonicalize a text-format jsonb
// parameter at Bind time.
func CanonicalizeJSONB(text string) (string, error) {
	node, err := decodeJSONB(text)
	if err != nil {
		return "", invalidJSONBInput(text)
	}
	canonical, err := encodeJSONB(node, false)
	if err != nil {
		return "", invalidJSONBInput(text)
	}
	return canonical, nil
}

func invalidJSONBInput(text string) error {
	return pgerror.InvalidText("invalid input syntax for type jsonb: %q", text)
}

// decodeJSONB parses one JSON document strictly. Duplicate object keys collapse
// last-wins (encoding/json's map decoding), invalid UTF-8 and trailing content
// are rejected, and numbers stay exact as json.Number.
func decodeJSONB(text string) (interface{}, error) {
	if !utf8.ValidString(text) {
		return nil, invalidJSONBInput(text)
	}
	if err := validateJSONBUnicodeEscapes(text); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var node interface{}
	if err := dec.Decode(&node); err != nil {
		return nil, invalidJSONBInput(text)
	}
	var trailing interface{}
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, invalidJSONBInput(text)
	}
	return node, nil
}

// validateJSONBUnicodeEscapes rejects escapes that encoding/json would accept
// by rewriting them: U+0000 and unpaired UTF-16 surrogates. PostgreSQL rejects
// both in jsonb input with 22P02 instead of silently producing U+FFFD.
func validateJSONBUnicodeEscapes(text string) error {
	inString := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		if c == '"' {
			inString = false
			continue
		}
		if c != '\\' {
			continue
		}
		if i+1 >= len(text) {
			return invalidJSONBInput(text)
		}
		i++
		if text[i] != 'u' {
			continue
		}
		if i+4 >= len(text) {
			return invalidJSONBInput(text)
		}
		value, ok := parseJSONBHex4(text[i+1 : i+5])
		if !ok {
			return invalidJSONBInput(text)
		}
		i += 4
		switch {
		case value == 0:
			return invalidJSONBInput(text)
		case value >= 0xD800 && value <= 0xDBFF:
			// A high surrogate must be followed by a low surrogate escape.
			if i+7 > len(text) || text[i+1] != '\\' || text[i+2] != 'u' {
				return invalidJSONBInput(text)
			}
			low, ok := parseJSONBHex4(text[i+3 : i+7])
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return invalidJSONBInput(text)
			}
			i += 6
		case value >= 0xDC00 && value <= 0xDFFF:
			return invalidJSONBInput(text)
		}
	}
	return nil
}

func parseJSONBHex4(text string) (uint32, bool) {
	var value uint32
	for i := 0; i < 4; i++ {
		digit := text[i]
		var nibble uint32
		switch {
		case digit >= '0' && digit <= '9':
			nibble = uint32(digit - '0')
		case digit >= 'a' && digit <= 'f':
			nibble = uint32(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			nibble = uint32(digit-'A') + 10
		default:
			return 0, false
		}
		value = value<<4 | nibble
	}
	return value, true
}

// encodeJSONB writes a decoded JSON tree in canonical form. pretty selects the
// four-space indented jsonb_pretty rendering.
func encodeJSONB(node interface{}, pretty bool) (string, error) {
	var buf bytes.Buffer
	if err := writeJSONBNode(&buf, node, pretty, 0); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func writeJSONBNode(buf *bytes.Buffer, node interface{}, pretty bool, depth int) error {
	switch value := node.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if value {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		text, err := canonicalJSONBNumber(value)
		if err != nil {
			return err
		}
		buf.WriteString(text)
	case string:
		writeJSONBString(buf, value)
	case []interface{}:
		if len(value) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteByte('[')
		for i, element := range value {
			if i > 0 {
				buf.WriteByte(',')
			}
			if pretty {
				buf.WriteByte('\n')
				writeJSONBIndent(buf, depth+1)
			}
			if err := writeJSONBNode(buf, element, pretty, depth+1); err != nil {
				return err
			}
		}
		if pretty {
			buf.WriteByte('\n')
			writeJSONBIndent(buf, depth)
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		if len(value) == 0 {
			buf.WriteString("{}")
			return nil
		}
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if pretty {
				buf.WriteByte('\n')
				writeJSONBIndent(buf, depth+1)
			}
			writeJSONBString(buf, key)
			buf.WriteByte(':')
			if pretty {
				buf.WriteByte(' ')
			}
			if err := writeJSONBNode(buf, value[key], pretty, depth+1); err != nil {
				return err
			}
		}
		if pretty {
			buf.WriteByte('\n')
			writeJSONBIndent(buf, depth)
		}
		buf.WriteByte('}')
	default:
		return pgerror.InvalidParameter("unsupported jsonb node type %T", node)
	}
	return nil
}

func writeJSONBIndent(buf *bytes.Buffer, depth int) {
	for i := 0; i < depth; i++ {
		buf.WriteString("    ")
	}
}

// writeJSONBString writes a JSON string literal without HTML escaping and
// without rewriting valid UTF-8. Control characters use the short escapes JSON
// defines, or \u00XX for the rest.
func writeJSONBString(buf *bytes.Buffer, value string) {
	buf.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				buf.WriteString(`\u00`)
				buf.WriteByte(hex[byte(r)>>4])
				buf.WriteByte(hex[byte(r)&0x0f])
				continue
			}
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

// canonicalJSONBNumber normalizes a JSON number to an exact decimal spelling:
// no exponent, no trailing fractional zeros, and no negative zero. The input is
// a finite decimal, so the exact expansion always terminates.
func canonicalJSONBNumber(number json.Number) (string, error) {
	text := string(number)
	negative := false
	i := 0
	if i < len(text) && text[i] == '-' {
		negative = true
		i++
	}
	start := i
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	digits := text[start:i]
	if digits == "" {
		return "", invalidJSONBInput(text)
	}
	fraction := ""
	if i < len(text) && text[i] == '.' {
		i++
		fractionStart := i
		for i < len(text) && text[i] >= '0' && text[i] <= '9' {
			i++
		}
		fraction = text[fractionStart:i]
	}
	exponent := 0
	if i < len(text) && (text[i] == 'e' || text[i] == 'E') {
		i++
		parsed, err := strconv.Atoi(text[i:])
		if err != nil {
			return "", invalidJSONBInput(text)
		}
		exponent = parsed
	} else if i != len(text) {
		return "", invalidJSONBInput(text)
	}

	allDigits := digits + fraction
	if len(allDigits) > maxJSONBNumberDigits {
		return "", pgerror.InvalidText("jsonb number field overflow")
	}
	mantissa := new(big.Int)
	if _, ok := mantissa.SetString(allDigits, 10); !ok {
		return "", invalidJSONBInput(text)
	}
	if mantissa.Sign() == 0 {
		return "0", nil
	}

	// value = mantissa / 10^scale, where scale = len(fraction) - exponent.
	scale := len(fraction) - exponent
	rendered := mantissa.String()
	if scale <= 0 {
		if len(rendered)-scale > maxJSONBNumberDigits {
			return "", pgerror.InvalidText("jsonb number field overflow")
		}
		rendered += strings.Repeat("0", -scale)
	} else {
		// Strip trailing zeros in the fractional part before rendering.
		for scale > 0 && len(rendered) > 1 && rendered[len(rendered)-1] == '0' {
			rendered = rendered[:len(rendered)-1]
			scale--
		}
		if scale >= len(rendered) {
			if scale > maxJSONBNumberDigits {
				return "", pgerror.InvalidText("jsonb number field overflow")
			}
			rendered = "0." + strings.Repeat("0", scale-len(rendered)) + rendered
		} else if scale > 0 {
			rendered = rendered[:len(rendered)-scale] + "." + rendered[len(rendered)-scale:]
		}
	}
	if negative {
		return "-" + rendered, nil
	}
	return rendered, nil
}

// jsonbNodeFromArg resolves a jsonb function or operator operand to a decoded
// tree. NULL stays SQL NULL; text and the JSON subtype are parsed as a document.
func jsonbNodeFromArg(value interface{}) (interface{}, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		return decodeJSONB(typed)
	case jsonText:
		return decodeJSONB(string(typed))
	case []byte:
		return decodeJSONB(string(typed))
	default:
		return nil, pgerror.DatatypeMismatch("jsonb value must be JSON text, got %T", value)
	}
}

// jsonbNodeFromSQLValue converts a SQL value into a JSON tree node for
// to_jsonb, jsonb_build_array, and jsonb_build_object values. NULL maps to JSON
// null; callers decide whether a top-level NULL stays SQL NULL.
func jsonbNodeFromSQLValue(value interface{}) (interface{}, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case bool:
		return typed, nil
	case int:
		return json.Number(strconv.FormatInt(int64(typed), 10)), nil
	case int8:
		return json.Number(strconv.FormatInt(int64(typed), 10)), nil
	case int16:
		return json.Number(strconv.FormatInt(int64(typed), 10)), nil
	case int32:
		return json.Number(strconv.FormatInt(int64(typed), 10)), nil
	case int64:
		return json.Number(strconv.FormatInt(typed, 10)), nil
	case uint:
		return json.Number(strconv.FormatUint(uint64(typed), 10)), nil
	case uint32:
		return json.Number(strconv.FormatUint(uint64(typed), 10)), nil
	case uint64:
		return json.Number(strconv.FormatUint(typed, 10)), nil
	case float32:
		return json.Number(strconv.FormatFloat(float64(typed), 'g', -1, 32)), nil
	case float64:
		if typed != typed {
			return nil, pgerror.InvalidParameter("cannot convert NaN to jsonb")
		}
		if typed > 1.7976931348623157e308 || typed < -1.7976931348623157e308 {
			return nil, pgerror.InvalidParameter("cannot convert infinity to jsonb")
		}
		return json.Number(strconv.FormatFloat(typed, 'g', -1, 64)), nil
	case string:
		return typed, nil
	case jsonText:
		return string(typed), nil
	case []byte:
		return string(typed), nil
	default:
		return toString(value), nil
	}
}

// jsonbNodeToText renders a jsonb node as the SQL text that ->> and the *_text
// set-returning functions return. JSON null becomes SQL NULL.
func jsonbNodeToText(node interface{}) (interface{}, error) {
	switch value := node.(type) {
	case nil:
		return nil, nil
	case bool:
		if value {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		return canonicalJSONBNumber(value)
	case string:
		return value, nil
	default:
		return encodeJSONB(node, false)
	}
}

// isJSONBExpr reports whether an expression produces a jsonb value. It follows
// the statement's column-type scopes, so a JSONB column, a cast, and a jsonb
// function are all recognized even though their runtime carrier is text.
func (e *Executor) isJSONBExpr(expr parser.Expr) bool {
	switch typed := expr.(type) {
	case nil:
		return false
	case *parser.ParenExpr:
		return e.isJSONBExpr(typed.Expr)
	case *parser.CastExpr:
		return strings.EqualFold(strings.TrimSpace(typed.Type.Name), "JSONB")
	case *parser.BinaryExpr:
		switch typed.Op {
		case lexer.TokenArrow, lexer.TokenHashArrow, lexer.TokenHashMinus:
			return e.isJSONBExpr(typed.Left)
		case lexer.TokenConcat:
			return e.isJSONBExpr(typed.Left) || e.isJSONBExpr(typed.Right)
		case lexer.TokenMinus:
			return e.isJSONBExpr(typed.Left)
		}
		return false
	case *parser.FunctionCall:
		if jsonbFunctionReturnsJSONB(typed.Name) {
			return true
		}
	}
	return strings.EqualFold(strings.TrimSpace(inferExprType(expr, e.resolveColumnType)), "JSONB")
}

// jsonbFunctionReturnsJSONB reports whether a scalar function returns jsonb.
func jsonbFunctionReturnsJSONB(name string) bool {
	switch strings.ToUpper(name) {
	case "TO_JSONB", "JSONB_SET", "JSONB_INSERT", "JSONB_REPLACE", "JSONB_REMOVE",
		"JSONB_STRIP_NULLS", "JSONB_BUILD_OBJECT", "JSONB_BUILD_ARRAY":
		return true
	default:
		return false
	}
}

// evalJSONBBinary applies the jsonb operators. handled is false when the operator
// and operands are not a jsonb form, leaving ordinary SQL semantics untouched.
func (e *Executor) evalJSONBBinary(op lexer.TokenType, leftExpr, rightExpr parser.Expr, left, right interface{}) (interface{}, bool, error) {
	leftJSONB := e.isJSONBExpr(leftExpr)
	rightJSONB := e.isJSONBExpr(rightExpr)

	if isComparisonOp(op) && (leftJSONB || rightJSONB) {
		if left == nil || right == nil {
			return nil, true, nil
		}
		if (leftJSONB && !jsonbComparisonValue(right)) || (rightJSONB && !jsonbComparisonValue(left)) {
			return nil, true, pgerror.DatatypeMismatch("jsonb comparison requires jsonb or text operands")
		}
		// jsonb equality and ordering compare the exact canonical bytes. Using
		// the engine's numeric-aware compare here would round through float64 and
		// report 1 = 1.0000000000000000001 or collapse integers above 2^53.
		leftText, err := CanonicalizeJSONB(toString(left))
		if err != nil {
			return nil, true, err
		}
		rightText, err := CanonicalizeJSONB(toString(right))
		if err != nil {
			return nil, true, err
		}
		result, err := comparisonOperatorResult(op, strings.Compare(leftText, rightText))
		return result, true, err
	}

	switch op {
	case lexer.TokenArrow, lexer.TokenArrowText:
		if !leftJSONB {
			return nil, false, nil
		}
		value, err := jsonbArrowExtract(left, right, op == lexer.TokenArrowText)
		return value, true, err

	case lexer.TokenHashArrow, lexer.TokenHashArrowText:
		value, err := jsonbPathExtract(left, right, op == lexer.TokenHashArrowText)
		return value, true, err

	case lexer.TokenHashMinus:
		value, err := jsonbPathDelete(left, right)
		return value, true, err

	case lexer.TokenAtArrow, lexer.TokenLtAt:
		value, err := e.jsonbContainment(op, leftExpr, rightExpr, left, right)
		return value, true, err

	case lexer.TokenQuestion:
		value, err := jsonbKeyExists(left, right)
		return value, true, err

	case lexer.TokenQuestionPipe, lexer.TokenQuestionAmp:
		value, err := jsonbAnyKeysExist(op, left, right)
		return value, true, err

	case lexer.TokenConcat:
		if !leftJSONB && !rightJSONB {
			return nil, false, nil
		}
		if left == nil || right == nil {
			return nil, true, nil
		}
		leftNode, err := jsonbNodeFromArg(left)
		if err != nil {
			return nil, true, err
		}
		rightNode, err := jsonbNodeFromArg(right)
		if err != nil {
			return nil, true, err
		}
		text, err := encodeJSONB(jsonbConcatNodes(leftNode, rightNode), false)
		return text, true, err

	case lexer.TokenMinus:
		if !leftJSONB {
			if rightJSONB {
				return nil, true, pgerror.DatatypeMismatch("cannot subtract a jsonb value from a non-jsonb value")
			}
			return nil, false, nil
		}
		if left == nil || right == nil {
			return nil, true, nil
		}
		value, err := e.jsonbDeleteValue(leftExpr, rightExpr, left, right)
		return value, true, err

	case lexer.TokenPlus, lexer.TokenStar, lexer.TokenSlash, lexer.TokenPercent:
		if leftJSONB || rightJSONB {
			return nil, true, pgerror.DatatypeMismatch("arithmetic operator %s requires numeric operands, got jsonb", op.String())
		}
	}
	return nil, false, nil
}

// jsonbComparisonValue reports whether a runtime value can be read as a jsonb
// document on the other side of a comparison. SQL numbers and booleans are not
// silently reinterpreted as JSON (PostgreSQL has no jsonb = integer operator).
func jsonbComparisonValue(value interface{}) bool {
	switch value.(type) {
	case string, jsonText, []byte:
		return true
	default:
		return false
	}
}

// canonicalizeJSONBReadValue canonicalizes a JSONB-typed value on read so a
// legacy row written before native JSONB still presents canonical text to
// projections, DISTINCT, GROUP BY, and ORDER BY.
func canonicalizeJSONBReadValue(typeName string, value interface{}) (interface{}, error) {
	if value == nil || !strings.EqualFold(strings.TrimSpace(typeName), "JSONB") {
		return value, nil
	}
	text, ok := value.(string)
	if !ok {
		if jsonValue, isJSON := value.(jsonText); isJSON {
			text = string(jsonValue)
		} else {
			return value, nil
		}
	}
	return CanonicalizeJSONB(text)
}

// canonicalizeJSONBColumnValue applies read canonicalization to a column
// reference. It is a no-op for columns whose declared type is not JSONB.
func (e *Executor) canonicalizeJSONBColumnValue(ref *parser.ColumnRef, value interface{}) (interface{}, error) {
	if ref == nil || value == nil {
		return value, nil
	}
	typeName, ok := e.resolveColumnType(ref.Table, ref.Column)
	if !ok {
		return value, nil
	}
	return canonicalizeJSONBReadValue(typeName, value)
}

// jsonbArrowPath resolves the right operand of -> and ->> for a jsonb document.
// A $ path is validated like every other jsonb path, so wildcards and slices
// are rejected with 0A000 rather than misinterpreted as member names.
func jsonbArrowPath(key interface{}) ([]jsonPathSeg, error) {
	switch typed := key.(type) {
	case string:
		if strings.HasPrefix(strings.TrimSpace(typed), "$") {
			return parseJSONBJSONPath(typed)
		}
		return jsonArrowPath(key)
	case jsonText:
		if strings.HasPrefix(strings.TrimSpace(string(typed)), "$") {
			return parseJSONBJSONPath(string(typed))
		}
		return jsonArrowPath(key)
	default:
		return jsonArrowPath(key)
	}
}

// jsonbArrowExtract evaluates -> and ->> against a jsonb document. A missing
// path is SQL NULL; a JSON null member is the jsonb value "null" for -> and SQL
// NULL for ->>.
func jsonbArrowExtract(document, key interface{}, scalar bool) (interface{}, error) {
	if document == nil || key == nil {
		return nil, nil
	}
	node, err := jsonbNodeFromArg(document)
	if err != nil {
		return nil, err
	}
	segs, err := jsonbArrowPath(key)
	if err != nil {
		return nil, err
	}
	found, ok := jsonLookup(node, segs)
	if !ok {
		return nil, nil
	}
	if scalar {
		return jsonbNodeToText(found)
	}
	return encodeJSONB(found, false)
}

// jsonbPathExtract evaluates #> and #>>. The path operand is a PostgreSQL
// text[] (canonical array text) or a $ path.
func jsonbPathExtract(document, path interface{}, scalar bool) (interface{}, error) {
	if document == nil || path == nil {
		return nil, nil
	}
	node, err := jsonbNodeFromArg(document)
	if err != nil {
		return nil, err
	}
	segs, err := jsonbPathArg(path)
	if err != nil {
		return nil, err
	}
	found, ok, err := jsonLookupE(node, segs)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if scalar {
		return jsonbNodeToText(found)
	}
	return encodeJSONB(found, false)
}

// jsonbPathDelete evaluates #-. A missing path leaves the document unchanged.
func jsonbPathDelete(document, path interface{}) (interface{}, error) {
	if document == nil || path == nil {
		return nil, nil
	}
	node, err := jsonbNodeFromArg(document)
	if err != nil {
		return nil, err
	}
	segs, err := jsonbPathArg(path)
	if err != nil {
		return nil, err
	}
	updated, err := jsonRemove(node, segs)
	if err != nil {
		return nil, err
	}
	return encodeJSONB(updated, false)
}

// jsonbContainment evaluates @> and <@. The jsonb operand must be present on at
// least one side so a text-only comparison is not silently reinterpreted.
func (e *Executor) jsonbContainment(op lexer.TokenType, leftExpr, rightExpr parser.Expr, left, right interface{}) (interface{}, error) {
	leftJSONB := e.isJSONBExpr(leftExpr)
	rightJSONB := e.isJSONBExpr(rightExpr)
	if !leftJSONB && !rightJSONB {
		return nil, pgerror.DatatypeMismatch("jsonb containment requires a jsonb operand; cast one side with ::jsonb")
	}
	if left == nil || right == nil {
		return nil, nil
	}
	leftNode, err := jsonbNodeFromArg(left)
	if err != nil {
		return nil, err
	}
	rightNode, err := jsonbNodeFromArg(right)
	if err != nil {
		return nil, err
	}
	switch op {
	case lexer.TokenAtArrow:
		return jsonbContains(leftNode, rightNode), nil
	case lexer.TokenLtAt:
		return jsonbContains(rightNode, leftNode), nil
	}
	return nil, pgerror.InvalidParameter("unsupported jsonb containment operator")
}

// jsonbKeyExists evaluates the ? operator: an object key or a top-level string
// array element.
func jsonbKeyExists(left, right interface{}) (interface{}, error) {
	if left == nil || right == nil {
		return nil, nil
	}
	node, err := jsonbNodeFromArg(left)
	if err != nil {
		return nil, err
	}
	key := toString(right)
	switch value := node.(type) {
	case map[string]interface{}:
		_, exists := value[key]
		return exists, nil
	case []interface{}:
		for _, element := range value {
			if text, ok := element.(string); ok && text == key {
				return true, nil
			}
		}
		return false, nil
	case string:
		// A scalar JSON string counts as the key itself: '"x"' ? 'x' is true.
		return value == key, nil
	default:
		return false, nil
	}
}

// jsonbAnyKeysExist evaluates ?| and ?& against a text[] of keys.
func jsonbAnyKeysExist(op lexer.TokenType, left, right interface{}) (interface{}, error) {
	if left == nil || right == nil {
		return nil, nil
	}
	node, err := jsonbNodeFromArg(left)
	if err != nil {
		return nil, err
	}
	keys, err := jsonbKeyListArg(right)
	if err != nil {
		return nil, err
	}
	matches := 0
	switch value := node.(type) {
	case map[string]interface{}:
		for _, key := range keys {
			if _, exists := value[key]; exists {
				matches++
			}
		}
	case []interface{}:
		for _, key := range keys {
			for _, element := range value {
				if text, ok := element.(string); ok && text == key {
					matches++
					break
				}
			}
		}
	default:
		return false, nil
	}
	if op == lexer.TokenQuestionAmp {
		return matches == len(keys), nil
	}
	return matches > 0, nil
}

// jsonbDeleteValue evaluates jsonb - text, jsonb - integer, and jsonb - text[].
func (e *Executor) jsonbDeleteValue(leftExpr, rightExpr parser.Expr, left, right interface{}) (interface{}, error) {
	if e.isJSONBExpr(rightExpr) {
		return nil, pgerror.DatatypeMismatch("jsonb deletion requires a text key, integer index, or text[] path, not a jsonb operand")
	}
	node, err := jsonbNodeFromArg(left)
	if err != nil {
		return nil, err
	}
	if _, isArray := e.arrayTypeOf(rightExpr); isArray {
		elements, parseErr := parseArrayLiteral(toString(right))
		if parseErr != nil {
			return nil, parseErr
		}
		keys := make([]string, 0, len(elements))
		for _, element := range elements {
			if element == nil {
				return nil, pgerror.InvalidParameter("jsonb delete path contains a NULL element")
			}
			keys = append(keys, *element)
		}
		return encodeJSONB(jsonbDeleteKeys(node, keys), false)
	}
	switch value := right.(type) {
	case int64:
		array, ok := node.([]interface{})
		if !ok {
			if _, isObject := node.(map[string]interface{}); isObject {
				return nil, pgerror.InvalidParameter("cannot delete from object using integer index")
			}
			return nil, pgerror.InvalidParameter("cannot delete an array element from a jsonb %s", jsonbNodeTypeName(node))
		}
		return encodeJSONB(jsonbDeleteIndex(array, int(value)), false)
	case int:
		return e.jsonbDeleteValue(leftExpr, rightExpr, left, int64(value))
	case string, jsonText:
		// jsonb - text deletes an object key, or every string element equal to
		// the text when the document is an array.
		key := toString(value)
		return encodeJSONB(jsonbDeleteKeys(node, []string{key}), false)
	default:
		return nil, pgerror.DatatypeMismatch("cannot delete %T from jsonb", right)
	}
}

func jsonbDeleteKeys(node interface{}, keys []string) interface{} {
	switch container := node.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(container))
		for key, value := range container {
			out[key] = value
		}
		for _, key := range keys {
			delete(out, key)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, len(container))
		for _, element := range container {
			remove := false
			if text, ok := element.(string); ok {
				for _, key := range keys {
					if text == key {
						remove = true
						break
					}
				}
			}
			if !remove {
				out = append(out, element)
			}
		}
		return out
	default:
		return node
	}
}

func jsonbDeleteIndex(array []interface{}, index int) []interface{} {
	if index < 0 {
		index += len(array)
	}
	if index < 0 || index >= len(array) {
		return array
	}
	out := make([]interface{}, 0, len(array)-1)
	out = append(out, array[:index]...)
	out = append(out, array[index+1:]...)
	return out
}

// jsonbConcatNodes implements jsonb || jsonb: objects merge right-wins, arrays
// concatenate, and any non-array operand becomes a single-element array.
func jsonbConcatNodes(left, right interface{}) interface{} {
	leftObject, leftIsObject := left.(map[string]interface{})
	rightObject, rightIsObject := right.(map[string]interface{})
	if leftIsObject && rightIsObject {
		out := make(map[string]interface{}, len(leftObject)+len(rightObject))
		for key, value := range leftObject {
			out[key] = value
		}
		for key, value := range rightObject {
			out[key] = value
		}
		return out
	}
	asArray := func(node interface{}) []interface{} {
		if array, ok := node.([]interface{}); ok {
			return array
		}
		return []interface{}{node}
	}
	out := append([]interface{}{}, asArray(left)...)
	return append(out, asArray(right)...)
}

// jsonbContains implements jsonb containment: object keys/values recurse, array
// elements are order-independent, top-level arrays contain scalars, and scalars
// compare by type.
func jsonbContains(container, candidate interface{}) bool {
	// A top-level array contains a scalar candidate when any element does.
	switch candidate.(type) {
	case nil, bool, string, json.Number:
		if containerArray, ok := container.([]interface{}); ok {
			for _, element := range containerArray {
				if jsonbContains(element, candidate) {
					return true
				}
			}
			return false
		}
	}

	switch candidateValue := candidate.(type) {
	case map[string]interface{}:
		containerObject, ok := container.(map[string]interface{})
		if !ok {
			return false
		}
		for key, value := range candidateValue {
			other, exists := containerObject[key]
			if !exists || !jsonbContains(other, value) {
				return false
			}
		}
		return true
	case []interface{}:
		containerArray, ok := container.([]interface{})
		if ok {
			used := make([]bool, len(containerArray))
			for _, candidateElement := range candidateValue {
				matched := false
				for i, containerElement := range containerArray {
					if used[i] {
						continue
					}
					if jsonbContains(containerElement, candidateElement) {
						used[i] = true
						matched = true
						break
					}
				}
				if !matched {
					return false
				}
			}
			return true
		}
		// A non-array container can still contain a nested empty array? No.
		return false
	case nil:
		return container == nil
	case bool:
		other, ok := container.(bool)
		return ok && other == candidateValue
	case string:
		other, ok := container.(string)
		return ok && other == candidateValue
	case json.Number:
		candidateText, err := canonicalJSONBNumber(candidateValue)
		if err != nil {
			return false
		}
		other, ok := container.(json.Number)
		if !ok {
			return false
		}
		containerText, err := canonicalJSONBNumber(other)
		if err != nil {
			return false
		}
		return containerText == candidateText
	}
	return false
}

// jsonbNodeTypeName returns the PostgreSQL jsonb type name.
func jsonbNodeTypeName(node interface{}) string {
	switch node.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return "unknown"
	}
}

// jsonbPathArg resolves a path operand to path steps. A text[] value (canonical
// array text) is PostgreSQL's path form; a $string is the JSON1 path form this
// engine already uses for -> and json_extract.
func jsonbPathArg(value interface{}) ([]jsonPathSeg, error) {
	text := strings.TrimSpace(toString(value))
	switch {
	case strings.HasPrefix(text, "{"):
		return parseJSONBArrayPath(text)
	case strings.HasPrefix(text, "$"):
		return parseJSONBJSONPath(text)
	default:
		return nil, pgerror.InvalidParameter("jsonb path must be a text[] array literal such as '{a,0}' or a $ path, got %q", text)
	}
}

// jsonbKeyListArg parses a text[] of object keys. Unlike a path, every element
// is a key: a numeric-looking key such as "1" must not become an array index.
func jsonbKeyListArg(value interface{}) ([]string, error) {
	text := strings.TrimSpace(toString(value))
	if !strings.HasPrefix(text, "{") {
		return nil, pgerror.InvalidParameter("jsonb key list must be a text[] array literal, got %q", text)
	}
	elements, err := parseArrayLiteral(text)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(elements))
	for _, element := range elements {
		if element == nil {
			return nil, pgerror.InvalidParameter("jsonb key list element must not be NULL")
		}
		keys = append(keys, *element)
	}
	return keys, nil
}

// parseJSONBArrayPath parses PostgreSQL text[] path text into steps. A path
// element is kept as raw text because its meaning depends on the container it
// is applied to: an object key, or an integer array index. Numeric-looking
// object keys such as "1", "01", "-1", and "1.5" therefore stay keys when the
// container is an object.
func parseJSONBArrayPath(text string) ([]jsonPathSeg, error) {
	elements, err := parseArrayLiteral(text)
	if err != nil {
		return nil, err
	}
	segs := make([]jsonPathSeg, 0, len(elements))
	for _, element := range elements {
		if element == nil {
			return nil, pgerror.InvalidParameter("jsonb path element must not be NULL")
		}
		segs = append(segs, jsonPathSeg{dynamic: true, element: *element})
	}
	return segs, nil
}

// resolveJSONBPathSegment converts a PostgreSQL path element into a concrete
// object key or array index for the container it is applied to. Concrete steps
// (JSON1 $ paths and index/key steps) pass through unchanged. resolved is false
// when container is a scalar or missing; callers decide whether that is a
// not-found, an unchanged target, or an error. strict selects whether a
// non-integer element on an array is an error (jsonb path mutation and #-) or a
// non-match (jsonb #>).
func resolveJSONBPathSegment(seg jsonPathSeg, container interface{}, strict bool) (jsonPathSeg, bool, error) {
	if !seg.dynamic {
		return seg, true, nil
	}
	switch container.(type) {
	case map[string]interface{}:
		return jsonPathSeg{key: seg.element}, true, nil
	case []interface{}:
		index, err := strconv.Atoi(seg.element)
		if err != nil {
			if strict {
				return seg, false, pgerror.InvalidText("path element %q is not an integer", seg.element)
			}
			return seg, false, nil
		}
		return jsonPathSeg{isIndex: true, index: index, fromEnd: index < 0}, true, nil
	default:
		return seg, false, nil
	}
}

// parseJSONBJSONPath parses a JSON1 $ path for jsonb use and rejects the
// jsonpath shapes the engine cannot execute (wildcards, slices, unbounded
// expansion) with SQLSTATE 0A000.
func parseJSONBJSONPath(path string) ([]jsonPathSeg, error) {
	if unsupportedJSONPathShape(path) {
		return nil, pgerror.Unsupported("jsonpath wildcards, slices, and unbounded expansion are not supported; pass an explicit path")
	}
	segs, err := parseJSONPath(path)
	if err != nil {
		return nil, err
	}
	for _, segment := range segs {
		if !segment.isIndex && (segment.key == "*" || strings.HasPrefix(segment.key, "**")) {
			return nil, pgerror.Unsupported("jsonpath wildcards and unbounded expansion are not supported; pass an explicit path")
		}
	}
	return segs, nil
}

// unsupportedJSONPathShape reports whether path text contains a jsonpath
// wildcard, slice, or deep-expansion marker inside brackets or as a bare member.
func unsupportedJSONPathShape(path string) bool {
	for i := 0; i < len(path); i++ {
		if path[i] != '[' {
			continue
		}
		end := strings.IndexByte(path[i:], ']')
		if end < 0 {
			return false
		}
		inner := path[i+1 : i+end]
		if strings.ContainsAny(inner, "*:") {
			return true
		}
		i += end
	}
	return false
}

// jsonbSetPG implements PostgreSQL's jsonb_set path semantics. An empty path
// leaves the target unchanged. All steps except the last must exist and be
// containers; missing keys or a scalar on the way leave the target unchanged. A
// missing array position at the last step is appended when create is true, and
// an index before the start is clamped to the first position.
func jsonbSetPG(node interface{}, segs []jsonPathSeg, value interface{}, create bool) (interface{}, error) {
	if len(segs) == 0 {
		return node, nil
	}
	segment, resolved, err := resolveJSONBPathSegment(segs[0], node, true)
	if err != nil {
		return nil, err
	}
	if !resolved {
		return nil, pgerror.InvalidParameter("cannot set path in scalar")
	}
	if len(segs) > 1 {
		child, exists, err := jsonbPathChild(node, segment)
		if err != nil {
			return nil, err
		}
		if !exists {
			return node, nil
		}
		switch child.(type) {
		case map[string]interface{}, []interface{}:
		default:
			// PostgreSQL leaves the target unchanged when an intermediate step
			// names a scalar rather than a container.
			return node, nil
		}
		updated, err := jsonbSetPG(child, segs[1:], value, create)
		if err != nil {
			return nil, err
		}
		return jsonbReplaceChild(node, segment, updated)
	}

	switch container := node.(type) {
	case map[string]interface{}:
		if segment.isIndex {
			return nil, pgerror.InvalidText("path element is not an object key")
		}
		if _, exists := container[segment.key]; !exists && !create {
			return node, nil
		}
		out := make(map[string]interface{}, len(container)+1)
		for key, item := range container {
			out[key] = item
		}
		out[segment.key] = value
		return out, nil
	case []interface{}:
		if !segment.isIndex {
			return nil, pgerror.InvalidText("path element %q is not an integer", segment.key)
		}
		index := segment.index
		if index < 0 {
			index += len(container)
		}
		if index < 0 {
			if !create {
				return node, nil
			}
			out := make([]interface{}, 0, len(container)+1)
			out = append(out, value)
			return append(out, container...), nil
		}
		if index >= len(container) {
			if !create {
				return node, nil
			}
			out := append([]interface{}{}, container...)
			return append(out, value), nil
		}
		out := append([]interface{}{}, container...)
		out[index] = value
		return out, nil
	default:
		return nil, pgerror.InvalidParameter("cannot set path in scalar")
	}
}

// jsonbInsertPG implements PostgreSQL's jsonb_insert path semantics: array
// elements are inserted before or after an index (clamped into range), and an
// object key is inserted only when it is absent. An existing object key is an
// error, matching PostgreSQL.
func jsonbInsertPG(node interface{}, segs []jsonPathSeg, value interface{}, after bool) (interface{}, error) {
	if len(segs) == 0 {
		return node, nil
	}
	segment, resolved, err := resolveJSONBPathSegment(segs[0], node, true)
	if err != nil {
		return nil, err
	}
	if !resolved {
		return nil, pgerror.InvalidParameter("cannot set path in scalar")
	}
	if len(segs) > 1 {
		child, exists, err := jsonbPathChild(node, segment)
		if err != nil {
			return nil, err
		}
		if !exists {
			return node, nil
		}
		switch child.(type) {
		case map[string]interface{}, []interface{}:
		default:
			return node, nil
		}
		updated, err := jsonbInsertPG(child, segs[1:], value, after)
		if err != nil {
			return nil, err
		}
		return jsonbReplaceChild(node, segment, updated)
	}

	switch container := node.(type) {
	case map[string]interface{}:
		if segment.isIndex {
			return nil, pgerror.InvalidText("path element is not an object key")
		}
		if _, exists := container[segment.key]; exists {
			return nil, pgerror.InvalidParameter("cannot replace existing key")
		}
		out := make(map[string]interface{}, len(container)+1)
		for key, item := range container {
			out[key] = item
		}
		out[segment.key] = value
		return out, nil
	case []interface{}:
		if !segment.isIndex {
			return nil, pgerror.InvalidText("path element %q is not an integer", segment.key)
		}
		index := segment.index
		if index < 0 {
			index += len(container)
		}
		if index < 0 {
			index = 0
		}
		if index > len(container) {
			index = len(container)
		}
		if after {
			index++
		}
		if index > len(container) {
			index = len(container)
		}
		out := make([]interface{}, 0, len(container)+1)
		out = append(out, container[:index]...)
		out = append(out, value)
		out = append(out, container[index:]...)
		return out, nil
	default:
		return nil, pgerror.InvalidParameter("cannot set path in scalar")
	}
}

// jsonbPathChild resolves one non-final path step to an existing child.
func jsonbPathChild(node interface{}, segment jsonPathSeg) (interface{}, bool, error) {
	switch container := node.(type) {
	case map[string]interface{}:
		if segment.isIndex {
			return nil, false, pgerror.InvalidParameter("jsonb path element %d does not identify an object key", segment.index)
		}
		child, exists := container[segment.key]
		return child, exists, nil
	case []interface{}:
		if !segment.isIndex {
			return nil, false, pgerror.InvalidParameter("jsonb path element %q does not identify an array index", segment.key)
		}
		index := segment.index
		if index < 0 {
			index += len(container)
		}
		if index < 0 || index >= len(container) {
			return nil, false, nil
		}
		return container[index], true, nil
	default:
		return nil, false, pgerror.InvalidParameter("cannot walk a jsonb path through a %s", jsonbNodeTypeName(node))
	}
}

// jsonbReplaceChild writes an updated child back into its container.
func jsonbReplaceChild(node interface{}, segment jsonPathSeg, child interface{}) (interface{}, error) {
	switch container := node.(type) {
	case map[string]interface{}:
		if segment.isIndex {
			return nil, pgerror.InvalidParameter("jsonb path element %d does not identify an object key", segment.index)
		}
		out := make(map[string]interface{}, len(container)+1)
		for key, item := range container {
			out[key] = item
		}
		out[segment.key] = child
		return out, nil
	case []interface{}:
		if !segment.isIndex {
			return nil, pgerror.InvalidParameter("jsonb path element %q does not identify an array index", segment.key)
		}
		index := segment.index
		if index < 0 {
			index += len(container)
		}
		if index < 0 || index >= len(container) {
			return nil, pgerror.InvalidParameter("jsonb path array index %d is out of range", segment.index)
		}
		out := append([]interface{}{}, container...)
		out[index] = child
		return out, nil
	default:
		return nil, pgerror.InvalidParameter("cannot write a jsonb path through a %s", jsonbNodeTypeName(node))
	}
}

// jsonbStripNulls removes object members whose value is JSON null. Array
// elements are kept, matching jsonb_strip_nulls.
func jsonbStripNulls(node interface{}) interface{} {
	switch value := node.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(value))
		for key, item := range value {
			if item == nil {
				continue
			}
			out[key] = jsonbStripNulls(item)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(value))
		for i, item := range value {
			out[i] = jsonbStripNulls(item)
		}
		return out
	default:
		return node
	}
}

// jsonbEncodeNode canonicalizes a tree that has already been validated.
func jsonbEncodeNode(node interface{}) (string, error) {
	return encodeJSONB(node, false)
}

// evalJSONBFunction evaluates a native jsonb scalar function. handled is false
// for names it does not own, keeping the JSON1 surface untouched.
func (e *Executor) evalJSONBFunction(name string, fn *parser.FunctionCall, args []interface{}) (interface{}, bool, error) {
	upper := strings.ToUpper(name)
	switch upper {
	case "TO_JSONB":
		if len(args) != 1 {
			return nil, true, pgerror.InvalidParameter("to_jsonb() requires exactly one argument")
		}
		if args[0] == nil {
			return nil, true, nil
		}
		node, err := e.toJSONBNode(firstFunctionArg(fn), args[0])
		if err != nil {
			return nil, true, err
		}
		text, err := jsonbEncodeNode(node)
		return text, true, err

	case "JSONB_TYPEOF":
		if len(args) != 1 {
			return nil, true, pgerror.InvalidParameter("jsonb_typeof() requires exactly one argument")
		}
		node, err := jsonbNodeFromArg(args[0])
		if err != nil {
			return nil, true, err
		}
		if node == nil && args[0] == nil {
			return nil, true, nil
		}
		return jsonbNodeTypeName(node), true, nil

	case "JSONB_ARRAY_LENGTH":
		if len(args) != 1 {
			return nil, true, pgerror.InvalidParameter("jsonb_array_length() requires exactly one argument")
		}
		node, err := jsonbNodeFromArg(args[0])
		if err != nil {
			return nil, true, err
		}
		if node == nil && args[0] == nil {
			return nil, true, nil
		}
		array, ok := node.([]interface{})
		if !ok {
			return nil, true, pgerror.InvalidParameter("cannot get array length of a jsonb %s", jsonbNodeTypeName(node))
		}
		return int64(len(array)), true, nil

	case "JSONB_PRETTY":
		if len(args) != 1 {
			return nil, true, pgerror.InvalidParameter("jsonb_pretty() requires exactly one argument")
		}
		if args[0] == nil {
			return nil, true, nil
		}
		node, err := jsonbNodeFromArg(args[0])
		if err != nil {
			return nil, true, err
		}
		text, err := encodeJSONB(node, true)
		return text, true, err

	case "JSONB_STRIP_NULLS":
		if len(args) != 1 {
			return nil, true, pgerror.InvalidParameter("jsonb_strip_nulls() requires exactly one argument")
		}
		if args[0] == nil {
			return nil, true, nil
		}
		node, err := jsonbNodeFromArg(args[0])
		if err != nil {
			return nil, true, err
		}
		text, err := jsonbEncodeNode(jsonbStripNulls(node))
		return text, true, err

	case "JSONB_BUILD_ARRAY":
		values := make([]interface{}, len(args))
		for i, arg := range args {
			node, err := e.toJSONBNode(functionArgAt(fn, i), arg)
			if err != nil {
				return nil, true, err
			}
			values[i] = node
		}
		text, err := jsonbEncodeNode(values)
		return text, true, err

	case "JSONB_BUILD_OBJECT":
		if len(args)%2 != 0 {
			return nil, true, pgerror.InvalidParameter("jsonb_build_object() argument list must have an even number of elements")
		}
		object := make(map[string]interface{}, len(args)/2)
		for i := 0; i+1 < len(args); i += 2 {
			if args[i] == nil {
				return nil, true, pgerror.InvalidParameter("jsonb_build_object() argument %d: key must not be null", i+1)
			}
			key := toString(args[i])
			node, err := e.toJSONBNode(functionArgAt(fn, i+1), args[i+1])
			if err != nil {
				return nil, true, err
			}
			object[key] = node
		}
		text, err := jsonbEncodeNode(object)
		return text, true, err

	case "JSONB_SET", "JSONB_INSERT", "JSONB_REPLACE", "JSONB_REMOVE":
		value, err := e.evalJSONBPatch(upper, fn, args)
		return value, true, err

	case "JSONB_EACH", "JSONB_EACH_TEXT", "JSONB_ARRAY_ELEMENTS", "JSONB_ARRAY_ELEMENTS_TEXT", "JSONB_OBJECT_KEYS":
		return nil, true, pgerror.Unsupported("%s() is a set-returning function and is only supported as the single FROM item, as in SELECT * FROM %s(jsonb_column)", name, strings.ToLower(name))

	case "JSONB_PATH_EXISTS", "JSONB_PATH_EXISTS_TZ", "JSONB_PATH_MATCH", "JSONB_PATH_MATCH_TZ",
		"JSONB_PATH_QUERY", "JSONB_PATH_QUERY_TZ", "JSONB_PATH_QUERY_ARRAY", "JSONB_PATH_QUERY_ARRAY_TZ",
		"JSONB_PATH_QUERY_FIRST", "JSONB_PATH_QUERY_FIRST_TZ":
		return nil, true, pgerror.Unsupported("jsonpath (%s) is not supported", strings.ToLower(name))
	}
	return nil, false, nil
}

// firstFunctionArg returns the first argument expression of a call, or nil when
// the call has no arguments.
func firstFunctionArg(fn *parser.FunctionCall) parser.Expr {
	if fn == nil || len(fn.Args) == 0 {
		return nil
	}
	return fn.Args[0]
}

func functionArgAt(fn *parser.FunctionCall, index int) parser.Expr {
	if fn == nil || index < 0 || index >= len(fn.Args) {
		return nil
	}
	return fn.Args[index]
}

// toJSONBNode converts a SQL value to a JSON tree node, decoding arrays into
// JSON arrays so to_jsonb(jsonb_build_array(...)) and to_jsonb(array_column)
// keep their element types.
func (e *Executor) toJSONBNode(expr parser.Expr, value interface{}) (interface{}, error) {
	if value == nil {
		return nil, nil
	}
	// to_jsonb(jsonb) is the identity: a jsonb argument is embedded as its
	// document, not as a JSON string.
	if e.isJSONBExpr(expr) {
		return jsonbNodeFromArg(value)
	}
	if typeName, ok := e.arrayTypeOf(expr); ok {
		elements, err := parseArrayLiteral(toString(value))
		if err != nil {
			return nil, err
		}
		elementType, _ := arrayElementType(typeName)
		nodes := make([]interface{}, len(elements))
		for i, element := range elements {
			typed, valueErr := arrayElementValue(element, elementType)
			if valueErr != nil {
				return nil, valueErr
			}
			node, nodeErr := jsonbNodeFromSQLValue(typed)
			if nodeErr != nil {
				return nil, nodeErr
			}
			nodes[i] = node
		}
		return nodes, nil
	}
	return jsonbNodeFromSQLValue(value)
}

// evalJSONBPatch evaluates the jsonb_set/jsonb_insert/jsonb_replace/
// jsonb_remove family. A $ path keeps the JSON1 semantics the engine already
// exposed; a PostgreSQL text[] path uses PostgreSQL's jsonb_set/jsonb_insert
// rules and accepts the optional boolean fourth argument.
func (e *Executor) evalJSONBPatch(name string, fn *parser.FunctionCall, args []interface{}) (interface{}, error) {
	minArgs := 2
	if name != "JSONB_REMOVE" {
		minArgs = 3
	}
	if len(args) < minArgs {
		return nil, pgerror.InvalidParameter("%s() requires at least %d arguments", strings.ToLower(name), minArgs)
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	node, err := jsonbNodeFromArg(args[0])
	if err != nil {
		return nil, err
	}

	if name == "JSONB_REMOVE" {
		for _, path := range args[1:] {
			if path == nil {
				return nil, nil
			}
			segs, pathErr := jsonbPathArg(path)
			if pathErr != nil {
				return nil, pathErr
			}
			node, err = jsonRemove(node, segs)
			if err != nil {
				return nil, err
			}
		}
		return jsonbEncodeNode(node)
	}

	// JSON1 multi-path form: jsonb_set(doc, path, value, path, value, ...).
	// PostgreSQL's form has exactly three or four arguments.
	pathText := strings.TrimSpace(toString(args[1]))
	pgPath := strings.HasPrefix(pathText, "{")
	if pgPath && len(args) > 4 {
		return nil, pgerror.InvalidParameter("%s() with a PostgreSQL path accepts at most four arguments", strings.ToLower(name))
	}
	if len(args) == 4 && !pgPath {
		return nil, pgerror.Unsupported("%s() accepts the optional insert_after/create_if_missing argument only with a PostgreSQL text[] path; a $ path uses the JSON1 form", strings.ToLower(name))
	}

	flag := false
	if len(args) == 4 {
		if args[3] == nil {
			return nil, nil
		}
		flag = toBool(args[3])
	}

	if !pgPath {
		// JSON1 form: every path/value pair is applied in order. A plain SQL
		// string becomes a JSON string, which is the behavior this engine exposed
		// before JSONB became native; a jsonb-typed value embeds its document.
		mode := "set"
		switch name {
		case "JSONB_INSERT":
			mode = "insert"
		case "JSONB_REPLACE":
			mode = "replace"
		}
		for i := 1; i+1 < len(args); i += 2 {
			if args[i] == nil || args[i+1] == nil {
				return nil, nil
			}
			segs, pathErr := jsonbPathArg(args[i])
			if pathErr != nil {
				return nil, pathErr
			}
			var patchValue interface{}
			if e.isJSONBExpr(functionArgAt(fn, i+1)) {
				patchValue, err = jsonbNodeFromArg(args[i+1])
			} else {
				patchValue = jsonArgToNode(args[i+1])
			}
			if err != nil {
				return nil, err
			}
			node, err = jsonApplyPatch(node, segs, patchValue, mode)
			if err != nil {
				return nil, err
			}
		}
		return jsonbEncodeNode(node)
	}

	if args[2] == nil {
		return nil, nil
	}
	newValueNode, err := jsonbNodeFromArg(args[2])
	if err != nil {
		return nil, err
	}
	segs, err := jsonbPathArg(args[1])
	if err != nil {
		return nil, err
	}
	switch name {
	case "JSONB_SET":
		create := true
		if len(args) == 4 {
			create = flag
		}
		node, err = jsonbSetPG(node, segs, newValueNode, create)
	case "JSONB_INSERT":
		node, err = jsonbInsertPG(node, segs, newValueNode, flag)
	case "JSONB_REPLACE":
		return nil, pgerror.Unsupported("jsonb_replace() with a PostgreSQL path is not supported; use jsonb_set or the JSON1 $ path form")
	}
	if err != nil {
		return nil, err
	}
	return jsonbEncodeNode(node)
}

// jsonbTableFunction materializes a set-returning jsonb function as a relation.
// The returned types are the SQL type names used for result metadata.
func jsonbTableFunction(name string, value interface{}) (columns, types []string, rows [][]interface{}, err error) {
	node, err := jsonbNodeFromArg(value)
	if err != nil {
		return nil, nil, nil, err
	}
	if value == nil {
		switch strings.ToLower(name) {
		case "jsonb_each":
			return []string{"key", "value"}, []string{"TEXT", "JSONB"}, nil, nil
		case "jsonb_each_text":
			return []string{"key", "value"}, []string{"TEXT", "TEXT"}, nil, nil
		case "jsonb_object_keys":
			return []string{"jsonb_object_keys"}, []string{"TEXT"}, nil, nil
		case "jsonb_array_elements_text":
			return []string{"value"}, []string{"TEXT"}, nil, nil
		default:
			return []string{"value"}, []string{"JSONB"}, nil, nil
		}
	}

	switch strings.ToLower(name) {
	case "jsonb_each", "jsonb_each_text", "jsonb_object_keys":
		object, ok := node.(map[string]interface{})
		if !ok {
			return nil, nil, nil, pgerror.InvalidParameter("cannot call %s() on a jsonb %s", strings.ToLower(name), jsonbNodeTypeName(node))
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if strings.EqualFold(name, "jsonb_object_keys") {
			rows = make([][]interface{}, 0, len(keys))
			for _, key := range keys {
				rows = append(rows, []interface{}{key})
			}
			return []string{"jsonb_object_keys"}, []string{"TEXT"}, rows, nil
		}
		textValues := strings.EqualFold(name, "jsonb_each_text")
		valueType := "JSONB"
		if textValues {
			valueType = "TEXT"
		}
		rows = make([][]interface{}, 0, len(keys))
		for _, key := range keys {
			var rendered interface{}
			if textValues {
				rendered, err = jsonbNodeToText(object[key])
			} else {
				rendered, err = encodeJSONB(object[key], false)
			}
			if err != nil {
				return nil, nil, nil, err
			}
			rows = append(rows, []interface{}{key, rendered})
		}
		return []string{"key", "value"}, []string{"TEXT", valueType}, rows, nil

	case "jsonb_array_elements", "jsonb_array_elements_text":
		array, ok := node.([]interface{})
		if !ok {
			return nil, nil, nil, pgerror.InvalidParameter("cannot call %s() on a jsonb %s", strings.ToLower(name), jsonbNodeTypeName(node))
		}
		textValues := strings.EqualFold(name, "jsonb_array_elements_text")
		valueType := "JSONB"
		if textValues {
			valueType = "TEXT"
		}
		rows = make([][]interface{}, 0, len(array))
		for _, element := range array {
			var rendered interface{}
			if textValues {
				rendered, err = jsonbNodeToText(element)
			} else {
				rendered, err = encodeJSONB(element, false)
			}
			if err != nil {
				return nil, nil, nil, err
			}
			rows = append(rows, []interface{}{rendered})
		}
		return []string{"value"}, []string{valueType}, rows, nil
	}
	return nil, nil, nil, pgerror.Unsupported("table function %s() is not supported", name)
}

// normalizeJSONBRow canonicalizes every JSONB-typed column value before it is
// written, mirroring normalizeArrayRow. A value is canonicalized once, so a
// reload reads back the same bytes.
func (e *Executor) normalizeJSONBRow(schema *storage.Schema, row storage.Row) error {
	if schema == nil {
		return nil
	}
	for _, column := range schema.Columns {
		if !strings.EqualFold(strings.TrimSpace(column.Type), "JSONB") {
			continue
		}
		value, ok := lookupRowValue(row, column.Name)
		if !ok || value == nil {
			continue
		}
		text, isText := value.(string)
		if !isText {
			if jsonValue, isJSON := value.(jsonText); isJSON {
				text = string(jsonValue)
			} else {
				return pgerror.InvalidText("column %q is %s and accepts JSON text, got %T", column.Name, column.Type, value)
			}
		}
		canonical, err := CanonicalizeJSONB(text)
		if err != nil {
			return fmt.Errorf("column %q: %w", column.Name, err)
		}
		row[column.Name] = canonical
	}
	return nil
}

// canonicalJSONBDefault canonicalizes a constant DEFAULT for a JSONB column so
// the catalog and any ALTER TABLE backfill store the same bytes an INSERT would.
func canonicalJSONBDefault(typeName string, value interface{}, exprText string) (interface{}, string, error) {
	if !strings.EqualFold(strings.TrimSpace(typeName), "JSONB") || exprText != "" || value == nil {
		return value, exprText, nil
	}
	text, ok := value.(string)
	if !ok {
		if jsonValue, isJSON := value.(jsonText); isJSON {
			text = string(jsonValue)
		} else {
			return nil, "", pgerror.InvalidText("DEFAULT for %s must be JSON text, got %T", typeName, value)
		}
	}
	canonical, err := CanonicalizeJSONB(text)
	if err != nil {
		return nil, "", err
	}
	return canonical, "", nil
}
