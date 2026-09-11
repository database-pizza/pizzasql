package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// jsonText is a JSON value produced by a JSON1 function. It carries the JSON
// subtype across nested function calls (so json_insert can embed the result of
// json() verbatim) and is normalized to a plain string before it is stored.
type jsonText string

// jsonEncode marshals a decoded JSON tree without HTML escaping.
func jsonEncode(v interface{}) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// jsonDecode parses JSON text, preserving number precision with json.Number.
func jsonDecode(s string) (interface{}, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("malformed JSON")
	}
	// Reject trailing content, matching JSON1's strict parsing.
	if dec.More() {
		return nil, fmt.Errorf("malformed JSON")
	}
	return v, nil
}

// jsonInput resolves a JSON1 input argument to a decoded tree.
func jsonInput(v interface{}) (interface{}, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case jsonText:
		return jsonDecode(string(t))
	case string:
		return jsonDecode(t)
	case []byte:
		return jsonDecode(string(t))
	default:
		return nil, fmt.Errorf("malformed JSON")
	}
}

// jsonArgToNode converts a SQL value into a JSON tree node for embedding.
func jsonArgToNode(v interface{}) interface{} {
	switch t := v.(type) {
	case nil:
		return nil
	case jsonText:
		if node, err := jsonDecode(string(t)); err == nil {
			return node
		}
		return string(t)
	case bool:
		return t
	case int:
		return json.Number(strconv.FormatInt(int64(t), 10))
	case int64:
		return json.Number(strconv.FormatInt(t, 10))
	case uint64:
		return json.Number(strconv.FormatUint(t, 10))
	case float64:
		return t
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// jsonNodeToSQL converts a JSON tree node to the SQL value json_extract returns.
func jsonNodeToSQL(v interface{}) interface{} {
	switch t := v.(type) {
	case nil:
		return nil
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case bool:
		if t {
			return int64(1)
		}
		return int64(0)
	case string:
		return t
	case []interface{}, map[string]interface{}:
		s, err := jsonEncode(t)
		if err != nil {
			return nil
		}
		return jsonText(s)
	default:
		return t
	}
}

// jsonPathSeg is one component of a parsed JSON path.
type jsonPathSeg struct {
	key      string
	isIndex  bool
	index    int
	appendOp bool // [#] — append for insert/set
	fromEnd  bool // negative index
}

// parseJSONPath parses a JSON1 path expression such as $.a.b[0] or $[#].
func parseJSONPath(path string) ([]jsonPathSeg, error) {
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("JSON path error: %s", path)
	}
	var segs []jsonPathSeg
	i := 1
	for i < len(path) {
		switch path[i] {
		case '.':
			i++
			if i >= len(path) {
				return nil, fmt.Errorf("JSON path error: %s", path)
			}
			if path[i] == '"' {
				end := strings.IndexByte(path[i+1:], '"')
				if end < 0 {
					return nil, fmt.Errorf("JSON path error: %s", path)
				}
				segs = append(segs, jsonPathSeg{key: path[i+1 : i+1+end]})
				i += end + 2
				continue
			}
			start := i
			for i < len(path) && path[i] != '.' && path[i] != '[' {
				i++
			}
			segs = append(segs, jsonPathSeg{key: path[start:i]})
		case '[':
			end := strings.IndexByte(path[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("JSON path error: %s", path)
			}
			inner := path[i+1 : i+end]
			i += end + 1
			if inner == "#" {
				segs = append(segs, jsonPathSeg{isIndex: true, appendOp: true})
				continue
			}
			if strings.HasPrefix(inner, "\"") && strings.HasSuffix(inner, "\"") && len(inner) >= 2 {
				segs = append(segs, jsonPathSeg{key: inner[1 : len(inner)-1]})
				continue
			}
			n, err := strconv.Atoi(inner)
			if err != nil {
				return nil, fmt.Errorf("JSON path error: %s", path)
			}
			segs = append(segs, jsonPathSeg{isIndex: true, index: n, fromEnd: n < 0})
		default:
			return nil, fmt.Errorf("JSON path error: %s", path)
		}
	}
	return segs, nil
}

// jsonLookup walks a decoded tree to the node addressed by segs.
func jsonLookup(root interface{}, segs []jsonPathSeg) (interface{}, bool) {
	cur := root
	for _, seg := range segs {
		if seg.isIndex {
			arr, ok := cur.([]interface{})
			if !ok {
				return nil, false
			}
			idx := seg.index
			if seg.fromEnd {
				idx = len(arr) + idx
			}
			if idx < 0 || idx >= len(arr) {
				return nil, false
			}
			cur = arr[idx]
			continue
		}
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		val, ok := obj[seg.key]
		if !ok {
			return nil, false
		}
		cur = val
	}
	return cur, true
}

// jsonApplyPatch applies json_set/insert/replace mutations to a decoded tree.
// mode is "set", "insert", or "replace".
func jsonApplyPatch(root interface{}, segs []jsonPathSeg, value interface{}, mode string) (interface{}, error) {
	if len(segs) == 0 {
		return value, nil
	}
	seg := segs[0]
	last := len(segs) == 1
	childSegs := segs[1:]

	if seg.isIndex {
		arr, ok := root.([]interface{})
		if !ok {
			// A missing container is created only by json_set/json_insert.
			if mode == "replace" {
				return root, nil
			}
			arr = []interface{}{}
		}
		idx := seg.index
		if seg.appendOp {
			if last {
				return append(arr, value), nil
			}
			child, childOK := interface{}(nil), false
			_ = child
			_ = childOK
			// append a new container for nested paths
			container := newJSONContainer(childSegs[0])
			newChild, err := jsonApplyPatch(container, childSegs, value, mode)
			if err != nil {
				return nil, err
			}
			return append(arr, newChild), nil
		}
		if seg.fromEnd {
			idx = len(arr) + idx
		}
		if idx < 0 || idx > len(arr) {
			return root, nil
		}
		if last {
			if idx == len(arr) {
				if mode == "replace" {
					return root, nil
				}
				return append(arr, value), nil
			}
			replaced := append([]interface{}(nil), arr...)
			replaced[idx] = value
			return replaced, nil
		}
		if idx == len(arr) {
			if mode == "replace" {
				return root, nil
			}
			container := newJSONContainer(childSegs[0])
			newChild, err := jsonApplyPatch(container, childSegs, value, mode)
			if err != nil {
				return nil, err
			}
			return append(arr, newChild), nil
		}
		newChild, err := jsonApplyPatch(arr[idx], childSegs, value, mode)
		if err != nil {
			return nil, err
		}
		replaced := append([]interface{}(nil), arr...)
		replaced[idx] = newChild
		return replaced, nil
	}

	obj, ok := root.(map[string]interface{})
	if !ok {
		if mode == "replace" {
			return root, nil
		}
		obj = map[string]interface{}{}
	}
	if last {
		_, exists := obj[seg.key]
		if exists && mode == "insert" {
			return root, nil
		}
		if !exists && mode == "replace" {
			return root, nil
		}
		newObj := make(map[string]interface{}, len(obj)+1)
		for k, v := range obj {
			newObj[k] = v
		}
		newObj[seg.key] = value
		return newObj, nil
	}
	child, exists := obj[seg.key]
	if !exists {
		if mode == "replace" {
			return root, nil
		}
		child = newJSONContainer(childSegs[0])
	}
	newChild, err := jsonApplyPatch(child, childSegs, value, mode)
	if err != nil {
		return nil, err
	}
	newObj := make(map[string]interface{}, len(obj)+1)
	for k, v := range obj {
		newObj[k] = v
	}
	newObj[seg.key] = newChild
	return newObj, nil
}

func newJSONContainer(seg jsonPathSeg) interface{} {
	if seg.isIndex {
		return []interface{}{}
	}
	return map[string]interface{}{}
}

// jsonTypeName returns the JSON1 type name for a decoded node.
func jsonTypeName(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "false" // caller distinguishes true below
	case json.Number:
		s := string(v.(json.Number))
		if !strings.ContainsAny(s, ".eE") {
			return "integer"
		}
		return "real"
	case string:
		return "text"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return "null"
	}
}

// evalJSONFunction evaluates a JSON1 scalar function. It returns handled=false
// for names it does not own.
func evalJSONFunction(name string, args []interface{}) (interface{}, bool, error) {
	upper := strings.ToUpper(name)
	switch upper {
	case "JSON", "JSONB":
		if len(args) != 1 {
			return nil, true, fmt.Errorf("wrong number of arguments to %s()", name)
		}
		if args[0] == nil {
			return nil, true, nil
		}
		node, err := jsonInput(args[0])
		if err != nil {
			return nil, true, err
		}
		s, err := jsonEncode(node)
		if err != nil {
			return nil, true, err
		}
		return jsonText(s), true, nil

	case "JSON_VALID", "JSONB_VALID":
		if len(args) != 1 {
			return nil, true, fmt.Errorf("wrong number of arguments to %s()", name)
		}
		if _, err := jsonInput(args[0]); err != nil {
			return int64(0), true, nil
		}
		return int64(1), true, nil

	case "JSON_TYPE":
		if len(args) < 1 || len(args) > 2 {
			return nil, true, fmt.Errorf("wrong number of arguments to json_type()")
		}
		if args[0] == nil {
			return nil, true, nil
		}
		node, err := jsonInput(args[0])
		if err != nil {
			return nil, true, err
		}
		if len(args) == 2 {
			path, ok := args[1].(string)
			if !ok {
				if jt, is := args[1].(jsonText); is {
					path = string(jt)
				} else {
					return nil, true, fmt.Errorf("JSON path error")
				}
			}
			segs, perr := parseJSONPath(path)
			if perr != nil {
				return nil, true, perr
			}
			found, ok := jsonLookup(node, segs)
			if !ok {
				return nil, true, nil
			}
			node = found
		}
		if b, isBool := node.(bool); isBool {
			if b {
				return "true", true, nil
			}
			return "false", true, nil
		}
		return jsonTypeName(node), true, nil

	case "JSON_EXTRACT", "JSONB_EXTRACT":
		if len(args) < 2 {
			return nil, true, fmt.Errorf("wrong number of arguments to json_extract()")
		}
		if args[0] == nil {
			return nil, true, nil
		}
		node, err := jsonInput(args[0])
		if err != nil {
			return nil, true, err
		}
		for _, p := range args[1:] {
			path, ok := jsonPathArg(p)
			if !ok {
				return nil, true, fmt.Errorf("JSON path error")
			}
			segs, perr := parseJSONPath(path)
			if perr != nil {
				return nil, true, perr
			}
			found, ok := jsonLookup(node, segs)
			if !ok {
				return nil, true, nil
			}
			node = found
		}
		return jsonNodeToSQL(node), true, nil

	case "JSON_SET", "JSONB_SET", "JSON_INSERT", "JSONB_INSERT", "JSON_REPLACE", "JSONB_REPLACE":
		if len(args) < 3 || len(args)%2 == 0 {
			return nil, true, fmt.Errorf("wrong number of arguments to %s()", name)
		}
		if args[0] == nil {
			return nil, true, nil
		}
		mode := "set"
		if strings.Contains(upper, "INSERT") {
			mode = "insert"
		} else if strings.Contains(upper, "REPLACE") {
			mode = "replace"
		}
		node, err := jsonInput(args[0])
		if err != nil {
			return nil, true, err
		}
		for i := 1; i+1 < len(args); i += 2 {
			path, ok := jsonPathArg(args[i])
			if !ok {
				return nil, true, fmt.Errorf("JSON path error")
			}
			segs, perr := parseJSONPath(path)
			if perr != nil {
				return nil, true, perr
			}
			node, err = jsonApplyPatch(node, segs, jsonArgToNode(args[i+1]), mode)
			if err != nil {
				return nil, true, err
			}
		}
		s, err := jsonEncode(node)
		if err != nil {
			return nil, true, err
		}
		return jsonText(s), true, nil

	case "JSON_REMOVE", "JSONB_REMOVE":
		if len(args) < 2 {
			return nil, true, fmt.Errorf("wrong number of arguments to json_remove()")
		}
		if args[0] == nil {
			return nil, true, nil
		}
		node, err := jsonInput(args[0])
		if err != nil {
			return nil, true, err
		}
		for _, p := range args[1:] {
			path, ok := jsonPathArg(p)
			if !ok {
				return nil, true, fmt.Errorf("JSON path error")
			}
			segs, perr := parseJSONPath(path)
			if perr != nil {
				return nil, true, perr
			}
			node, err = jsonRemove(node, segs)
			if err != nil {
				return nil, true, err
			}
		}
		s, err := jsonEncode(node)
		if err != nil {
			return nil, true, err
		}
		return jsonText(s), true, nil

	case "JSON_ARRAY", "JSONB_ARRAY":
		arr := make([]interface{}, len(args))
		for i, a := range args {
			arr[i] = jsonArgToNode(a)
		}
		s, err := jsonEncode(arr)
		if err != nil {
			return nil, true, err
		}
		return jsonText(s), true, nil

	case "JSON_OBJECT", "JSONB_OBJECT":
		if len(args)%2 != 0 {
			return nil, true, fmt.Errorf("json_object() requires an even number of arguments")
		}
		obj := make(map[string]interface{}, len(args)/2)
		for i := 0; i+1 < len(args); i += 2 {
			key, ok := args[i].(string)
			if !ok {
				if jt, is := args[i].(jsonText); is {
					key = string(jt)
				} else {
					return nil, true, fmt.Errorf("json_object() labels must be TEXT")
				}
			}
			obj[key] = jsonArgToNode(args[i+1])
		}
		s, err := jsonEncode(obj)
		if err != nil {
			return nil, true, err
		}
		return jsonText(s), true, nil

	case "JSON_QUOTE", "JSONB_QUOTE":
		if len(args) != 1 {
			return nil, true, fmt.Errorf("json_quote() requires exactly one argument")
		}
		s, err := jsonEncode(jsonArgToNode(args[0]))
		if err != nil {
			return nil, true, err
		}
		return jsonText(s), true, nil
	}
	return nil, false, nil
}

// jsonPathArg extracts a path string from an argument.
func jsonPathArg(v interface{}) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case jsonText:
		return string(t), true
	default:
		return "", false
	}
}

// jsonRemove removes the node addressed by segs from a decoded tree.
func jsonRemove(root interface{}, segs []jsonPathSeg) (interface{}, error) {
	if len(segs) == 0 {
		return root, nil
	}
	seg := segs[0]
	last := len(segs) == 1
	if seg.isIndex {
		arr, ok := root.([]interface{})
		if !ok {
			return root, nil
		}
		idx := seg.index
		if seg.fromEnd {
			idx = len(arr) + idx
		}
		if idx < 0 || idx >= len(arr) {
			return root, nil
		}
		if last {
			out := make([]interface{}, 0, len(arr)-1)
			out = append(out, arr[:idx]...)
			out = append(out, arr[idx+1:]...)
			return out, nil
		}
		child, err := jsonRemove(arr[idx], segs[1:])
		if err != nil {
			return nil, err
		}
		out := append([]interface{}(nil), arr...)
		out[idx] = child
		return out, nil
	}
	obj, ok := root.(map[string]interface{})
	if !ok {
		return root, nil
	}
	if last {
		out := make(map[string]interface{}, len(obj))
		for k, v := range obj {
			if k != seg.key {
				out[k] = v
			}
		}
		return out, nil
	}
	child, exists := obj[seg.key]
	if !exists {
		return root, nil
	}
	newChild, err := jsonRemove(child, segs[1:])
	if err != nil {
		return nil, err
	}
	out := make(map[string]interface{}, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	out[seg.key] = newChild
	return out, nil
}
