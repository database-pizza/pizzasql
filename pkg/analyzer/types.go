package analyzer

import (
	"sort"
	"strings"
)

// Type represents a SQL type with SQLite affinity rules.
type Type int

const (
	TypeUnknown Type = iota // Unresolved type
	TypeNull                // NULL value
	TypeInteger             // INTEGER affinity
	TypeReal                // REAL affinity
	TypeText                // TEXT affinity
	TypeBlob                // BLOB affinity
	TypeNumeric             // NUMERIC affinity (flexible)
	TypeBoolean             // Boolean (stored as INTEGER in SQLite)
	TypeAny                 // Any type (for polymorphic functions)
)

func (t Type) String() string {
	switch t {
	case TypeUnknown:
		return "UNKNOWN"
	case TypeNull:
		return "NULL"
	case TypeInteger:
		return "INTEGER"
	case TypeReal:
		return "REAL"
	case TypeText:
		return "TEXT"
	case TypeBlob:
		return "BLOB"
	case TypeNumeric:
		return "NUMERIC"
	case TypeBoolean:
		return "BOOLEAN"
	case TypeAny:
		return "ANY"
	default:
		return "UNKNOWN"
	}
}

// TypeFromName returns the Type for a SQL type name using SQLite affinity rules.
// See: https://www.sqlite.org/datatype3.html
func TypeFromName(name string) Type {
	upper := strings.ToUpper(name)

	// Rule 1: If the type contains "INT" -> INTEGER
	if strings.Contains(upper, "INT") {
		return TypeInteger
	}

	// Rule 2: If the type contains "CHAR", "CLOB", or "TEXT" -> TEXT
	if strings.Contains(upper, "CHAR") ||
		strings.Contains(upper, "CLOB") ||
		strings.Contains(upper, "TEXT") {
		return TypeText
	}

	// UUID is stored as text (SQLite-style), not as a native PostgreSQL UUID.
	if upper == "UUID" {
		return TypeText
	}

	// Rule 3: If the type contains "BLOB" or is empty -> BLOB
	if strings.Contains(upper, "BLOB") || upper == "" {
		return TypeBlob
	}

	// Rule 4: If the type contains "REAL", "FLOA", or "DOUB" -> REAL
	if strings.Contains(upper, "REAL") ||
		strings.Contains(upper, "FLOA") ||
		strings.Contains(upper, "DOUB") {
		return TypeReal
	}

	// Rule 5: Otherwise -> NUMERIC
	// This includes NUMERIC, DECIMAL, BOOLEAN, DATE, DATETIME
	switch upper {
	case "BOOLEAN", "BOOL":
		return TypeBoolean
	default:
		return TypeNumeric
	}
}

// IsNumeric returns true if the type can hold numeric values.
func (t Type) IsNumeric() bool {
	switch t {
	case TypeInteger, TypeReal, TypeNumeric, TypeBoolean:
		return true
	default:
		return false
	}
}

// IsComparable returns true if two types can be compared.
func (t Type) IsComparable(other Type) bool {
	// NULL is comparable to anything
	if t == TypeNull || other == TypeNull {
		return true
	}
	// ANY matches anything
	if t == TypeAny || other == TypeAny {
		return true
	}
	// Same type
	if t == other {
		return true
	}
	// Numeric types are inter-comparable
	if t.IsNumeric() && other.IsNumeric() {
		return true
	}
	// TEXT and BLOB can be compared
	if (t == TypeText || t == TypeBlob) && (other == TypeText || other == TypeBlob) {
		return true
	}
	// NUMERIC/BOOLEAN accepts TEXT (SQLite-compatible: dates stored as text in numeric columns)
	if (t == TypeNumeric || t == TypeBoolean) && (other == TypeText || other == TypeBlob) {
		return true
	}
	if (other == TypeNumeric || other == TypeBoolean) && (t == TypeText || t == TypeBlob) {
		return true
	}
	return false
}

// CommonType returns the common type for binary operations.
func CommonType(a, b Type) Type {
	if a == TypeUnknown {
		return b
	}
	if b == TypeUnknown {
		return a
	}
	if a == TypeNull {
		return b
	}
	if b == TypeNull {
		return a
	}
	if a == TypeAny {
		return b
	}
	if b == TypeAny {
		return a
	}
	if a == b {
		return a
	}

	// Numeric promotion
	if a.IsNumeric() && b.IsNumeric() {
		if a == TypeReal || b == TypeReal {
			return TypeReal
		}
		if a == TypeNumeric || b == TypeNumeric {
			return TypeNumeric
		}
		return TypeInteger
	}

	// Text/Blob coercion
	if (a == TypeText || a == TypeBlob) && (b == TypeText || b == TypeBlob) {
		return TypeText
	}

	return TypeText // Default to TEXT for mixed types
}

// FunctionSignature describes a SQL function.
type FunctionSignature struct {
	Name        string
	MinArgs     int
	MaxArgs     int    // -1 for variadic
	ArgTypes    []Type // Expected argument types (TypeAny for flexible)
	ReturnType  Type
	IsAggregate bool
}

// builtinFunctions contains all built-in SQL functions.
var builtinFunctions = map[string]FunctionSignature{
	// Aggregate functions
	"COUNT":        {Name: "COUNT", MinArgs: 0, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeInteger, IsAggregate: true},
	"SUM":          {Name: "SUM", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeNumeric}, ReturnType: TypeNumeric, IsAggregate: true},
	"AVG":          {Name: "AVG", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeNumeric}, ReturnType: TypeReal, IsAggregate: true},
	"MIN":          {Name: "MIN", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeAny, IsAggregate: true},
	"MAX":          {Name: "MAX", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeAny, IsAggregate: true},
	"TOTAL":        {Name: "TOTAL", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeNumeric}, ReturnType: TypeReal, IsAggregate: true},
	"GROUP_CONCAT": {Name: "GROUP_CONCAT", MinArgs: 1, MaxArgs: 2, ArgTypes: []Type{TypeAny, TypeText}, ReturnType: TypeText, IsAggregate: true},

	// String functions
	"LENGTH":  {Name: "LENGTH", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeText}, ReturnType: TypeInteger, IsAggregate: false},
	"UPPER":   {Name: "UPPER", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeText}, ReturnType: TypeText, IsAggregate: false},
	"LOWER":   {Name: "LOWER", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeText}, ReturnType: TypeText, IsAggregate: false},
	"TRIM":    {Name: "TRIM", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeText}, ReturnType: TypeText, IsAggregate: false},
	"LTRIM":   {Name: "LTRIM", MinArgs: 1, MaxArgs: 2, ArgTypes: []Type{TypeText, TypeText}, ReturnType: TypeText, IsAggregate: false},
	"RTRIM":   {Name: "RTRIM", MinArgs: 1, MaxArgs: 2, ArgTypes: []Type{TypeText, TypeText}, ReturnType: TypeText, IsAggregate: false},
	"SUBSTR":  {Name: "SUBSTR", MinArgs: 2, MaxArgs: 3, ArgTypes: []Type{TypeText, TypeInteger, TypeInteger}, ReturnType: TypeText, IsAggregate: false},
	"REPLACE": {Name: "REPLACE", MinArgs: 3, MaxArgs: 3, ArgTypes: []Type{TypeText, TypeText, TypeText}, ReturnType: TypeText, IsAggregate: false},
	"INSTR":   {Name: "INSTR", MinArgs: 2, MaxArgs: 2, ArgTypes: []Type{TypeText, TypeText}, ReturnType: TypeInteger, IsAggregate: false},
	"PRINTF":  {Name: "PRINTF", MinArgs: 1, MaxArgs: -1, ArgTypes: []Type{TypeText}, ReturnType: TypeText, IsAggregate: false},
	"CONCAT":  {Name: "CONCAT", MinArgs: 1, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},

	// Numeric functions
	"ABS":    {Name: "ABS", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeNumeric}, ReturnType: TypeNumeric, IsAggregate: false},
	"ROUND":  {Name: "ROUND", MinArgs: 1, MaxArgs: 2, ArgTypes: []Type{TypeNumeric, TypeInteger}, ReturnType: TypeNumeric, IsAggregate: false},
	"CEIL":   {Name: "CEIL", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeNumeric}, ReturnType: TypeInteger, IsAggregate: false},
	"FLOOR":  {Name: "FLOOR", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeNumeric}, ReturnType: TypeInteger, IsAggregate: false},
	"MOD":    {Name: "MOD", MinArgs: 2, MaxArgs: 2, ArgTypes: []Type{TypeInteger, TypeInteger}, ReturnType: TypeInteger, IsAggregate: false},
	"RANDOM": {Name: "RANDOM", MinArgs: 0, MaxArgs: 0, ArgTypes: []Type{}, ReturnType: TypeInteger, IsAggregate: false},

	// Null handling
	"COALESCE": {Name: "COALESCE", MinArgs: 1, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeAny, IsAggregate: false},
	"NULLIF":   {Name: "NULLIF", MinArgs: 2, MaxArgs: 2, ArgTypes: []Type{TypeAny, TypeAny}, ReturnType: TypeAny, IsAggregate: false},
	"IFNULL":   {Name: "IFNULL", MinArgs: 2, MaxArgs: 2, ArgTypes: []Type{TypeAny, TypeAny}, ReturnType: TypeAny, IsAggregate: false},
	"IIF":      {Name: "IIF", MinArgs: 3, MaxArgs: 3, ArgTypes: []Type{TypeBoolean, TypeAny, TypeAny}, ReturnType: TypeAny, IsAggregate: false},

	// Type functions
	"TYPEOF": {Name: "TYPEOF", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"CAST":   {Name: "CAST", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeAny, IsAggregate: false},

	// Date/Time functions
	"DATE":      {Name: "DATE", MinArgs: 0, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"TIME":      {Name: "TIME", MinArgs: 0, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"DATETIME":  {Name: "DATETIME", MinArgs: 0, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JULIANDAY": {Name: "JULIANDAY", MinArgs: 0, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeReal, IsAggregate: false},
	"UNIXEPOCH": {Name: "UNIXEPOCH", MinArgs: 0, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeInteger, IsAggregate: false},
	"STRFTIME":  {Name: "STRFTIME", MinArgs: 1, MaxArgs: -1, ArgTypes: []Type{TypeText, TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"TIMEDIFF":  {Name: "TIMEDIFF", MinArgs: 2, MaxArgs: 2, ArgTypes: []Type{TypeAny, TypeAny}, ReturnType: TypeText, IsAggregate: false},

	// SQLite specific
	"SQLITE_VERSION":    {Name: "SQLITE_VERSION", MinArgs: 0, MaxArgs: 0, ArgTypes: []Type{}, ReturnType: TypeText, IsAggregate: false},
	"PIZZASQL_VERSION":  {Name: "PIZZASQL_VERSION", MinArgs: 0, MaxArgs: 0, ArgTypes: []Type{}, ReturnType: TypeText, IsAggregate: false},
	"LAST_INSERT_ROWID": {Name: "LAST_INSERT_ROWID", MinArgs: 0, MaxArgs: 0, ArgTypes: []Type{}, ReturnType: TypeInteger, IsAggregate: false},
	"CHANGES":           {Name: "CHANGES", MinArgs: 0, MaxArgs: 0, ArgTypes: []Type{}, ReturnType: TypeInteger, IsAggregate: false},
	"TOTAL_CHANGES":     {Name: "TOTAL_CHANGES", MinArgs: 0, MaxArgs: 0, ArgTypes: []Type{}, ReturnType: TypeInteger, IsAggregate: false},

	// Other
	"HEX":      {Name: "HEX", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeBlob}, ReturnType: TypeText, IsAggregate: false},
	"UNHEX":    {Name: "UNHEX", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeText}, ReturnType: TypeBlob, IsAggregate: false},
	"ZEROBLOB": {Name: "ZEROBLOB", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeInteger}, ReturnType: TypeBlob, IsAggregate: false},
	"QUOTE":    {Name: "QUOTE", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},

	// GoatCounter compatibility: scalar percentage difference.
	"PERCENT_DIFF": {Name: "PERCENT_DIFF", MinArgs: 2, MaxArgs: 2, ArgTypes: []Type{TypeNumeric, TypeNumeric}, ReturnType: TypeReal, IsAggregate: false},

	// JSON1 scalar functions.
	"JSON":          {Name: "JSON", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSONB":         {Name: "JSONB", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSON_VALID":    {Name: "JSON_VALID", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeInteger, IsAggregate: false},
	"JSONB_VALID":   {Name: "JSONB_VALID", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeInteger, IsAggregate: false},
	"JSON_TYPE":     {Name: "JSON_TYPE", MinArgs: 1, MaxArgs: 2, ArgTypes: []Type{TypeAny, TypeText}, ReturnType: TypeText, IsAggregate: false},
	"JSON_EXTRACT":  {Name: "JSON_EXTRACT", MinArgs: 2, MaxArgs: -1, ArgTypes: []Type{TypeAny, TypeText}, ReturnType: TypeAny, IsAggregate: false},
	"JSONB_EXTRACT": {Name: "JSONB_EXTRACT", MinArgs: 2, MaxArgs: -1, ArgTypes: []Type{TypeAny, TypeText}, ReturnType: TypeAny, IsAggregate: false},
	"JSON_SET":      {Name: "JSON_SET", MinArgs: 3, MaxArgs: -1, ArgTypes: []Type{TypeAny, TypeText, TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSONB_SET":     {Name: "JSONB_SET", MinArgs: 3, MaxArgs: -1, ArgTypes: []Type{TypeAny, TypeText, TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSON_INSERT":   {Name: "JSON_INSERT", MinArgs: 3, MaxArgs: -1, ArgTypes: []Type{TypeAny, TypeText, TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSONB_INSERT":  {Name: "JSONB_INSERT", MinArgs: 3, MaxArgs: -1, ArgTypes: []Type{TypeAny, TypeText, TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSON_REPLACE":  {Name: "JSON_REPLACE", MinArgs: 3, MaxArgs: -1, ArgTypes: []Type{TypeAny, TypeText, TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSONB_REPLACE": {Name: "JSONB_REPLACE", MinArgs: 3, MaxArgs: -1, ArgTypes: []Type{TypeAny, TypeText, TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSON_REMOVE":   {Name: "JSON_REMOVE", MinArgs: 2, MaxArgs: -1, ArgTypes: []Type{TypeAny, TypeText}, ReturnType: TypeText, IsAggregate: false},
	"JSONB_REMOVE":  {Name: "JSONB_REMOVE", MinArgs: 2, MaxArgs: -1, ArgTypes: []Type{TypeAny, TypeText}, ReturnType: TypeText, IsAggregate: false},
	"JSON_ARRAY":    {Name: "JSON_ARRAY", MinArgs: 0, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSONB_ARRAY":   {Name: "JSONB_ARRAY", MinArgs: 0, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSON_OBJECT":   {Name: "JSON_OBJECT", MinArgs: 0, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSONB_OBJECT":  {Name: "JSONB_OBJECT", MinArgs: 0, MaxArgs: -1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSON_QUOTE":    {Name: "JSON_QUOTE", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},
	"JSONB_QUOTE":   {Name: "JSONB_QUOTE", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: false},

	// JSON1 aggregate.
	"JSON_GROUP_ARRAY":  {Name: "JSON_GROUP_ARRAY", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: true},
	"JSONB_GROUP_ARRAY": {Name: "JSONB_GROUP_ARRAY", MinArgs: 1, MaxArgs: 1, ArgTypes: []Type{TypeAny}, ReturnType: TypeText, IsAggregate: true},
}

// LookupFunction returns the function signature for a function name.
func LookupFunction(name string) (FunctionSignature, bool) {
	sig, ok := builtinFunctions[strings.ToUpper(name)]
	return sig, ok
}

// IsAggregateFunction returns true if the function is an aggregate.
func IsAggregateFunction(name string) bool {
	sig, ok := LookupFunction(name)
	return ok && sig.IsAggregate
}

// BuiltinFunctions returns all built-in functions sorted by name.
func BuiltinFunctions() []FunctionSignature {
	functions := make([]FunctionSignature, 0, len(builtinFunctions))
	for _, sig := range builtinFunctions {
		functions = append(functions, sig)
	}
	sort.Slice(functions, func(i, j int) bool {
		return functions[i].Name < functions[j].Name
	})
	return functions
}

// ColumnInfo describes a column in a table.
type ColumnInfo struct {
	Name       string
	Type       Type
	Nullable   bool
	PrimaryKey bool
	Default    interface{}
	TableName  string // For qualified references
	Generated  bool   // generated columns cannot be written by the user
}

// TableInfo describes a table schema.
type TableInfo struct {
	Name    string
	Columns []ColumnInfo
	Alias   string // For query-local aliases
	IsView  bool   // Views accept any column reference
}

// GetColumn returns a column by name.
// For views (IsView=true), returns a wildcard ColumnInfo so column analysis passes.
func (t *TableInfo) GetColumn(name string) (*ColumnInfo, bool) {
	if t.IsView {
		return &ColumnInfo{Name: name, TableName: t.Name, Type: TypeAny}, true
	}
	upper := strings.ToUpper(name)
	for i := range t.Columns {
		if strings.ToUpper(t.Columns[i].Name) == upper {
			return &t.Columns[i], true
		}
	}
	return nil, false
}

// ExprInfo contains analysis results for an expression.
type ExprInfo struct {
	Type        Type
	IsAggregate bool
	IsConstant  bool
	Nullable    bool
}
