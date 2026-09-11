package analyzer

import (
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
)

func parse(t *testing.T, sql string) parser.Statement {
	t.Helper()
	l := lexer.New(sql)
	p := parser.New(l)
	stmt, err := p.Parse()
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	return stmt
}

func setupCatalog() *Catalog {
	catalog := NewCatalog()

	// Create users table
	catalog.CreateTable(&TableInfo{
		Name: "users",
		Columns: []ColumnInfo{
			{Name: "id", Type: TypeInteger, PrimaryKey: true},
			{Name: "name", Type: TypeText, Nullable: false},
			{Name: "email", Type: TypeText, Nullable: true},
			{Name: "age", Type: TypeInteger, Nullable: true},
			{Name: "active", Type: TypeBoolean, Nullable: false},
			{Name: "balance", Type: TypeReal, Nullable: true},
		},
	})

	// Create orders table
	catalog.CreateTable(&TableInfo{
		Name: "orders",
		Columns: []ColumnInfo{
			{Name: "id", Type: TypeInteger, PrimaryKey: true},
			{Name: "user_id", Type: TypeInteger, Nullable: false},
			{Name: "amount", Type: TypeReal, Nullable: false},
			{Name: "status", Type: TypeText, Nullable: false},
			{Name: "created_at", Type: TypeText, Nullable: false},
		},
	})

	// Create products table
	catalog.CreateTable(&TableInfo{
		Name: "products",
		Columns: []ColumnInfo{
			{Name: "id", Type: TypeInteger, PrimaryKey: true},
			{Name: "name", Type: TypeText, Nullable: false},
			{Name: "price", Type: TypeReal, Nullable: false},
			{Name: "stock", Type: TypeInteger, Nullable: false},
		},
	})

	return catalog
}

// Type system tests

func TestTypeFromName(t *testing.T) {
	tests := []struct {
		name     string
		expected Type
	}{
		{"INTEGER", TypeInteger},
		{"INT", TypeInteger},
		{"SMALLINT", TypeInteger},
		{"BIGINT", TypeInteger},
		{"TINYINT", TypeInteger},
		{"REAL", TypeReal},
		{"FLOAT", TypeReal},
		{"DOUBLE", TypeReal},
		{"TEXT", TypeText},
		{"VARCHAR", TypeText},
		{"CHAR", TypeText},
		{"CHARACTER", TypeText},
		{"CLOB", TypeText},
		{"BLOB", TypeBlob},
		{"BOOLEAN", TypeBoolean},
		{"NUMERIC", TypeNumeric},
		{"DECIMAL", TypeNumeric},
		{"UUID", TypeText},
		{"", TypeBlob}, // Empty type -> BLOB (SQLite rule)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TypeFromName(tt.name)
			if got != tt.expected {
				t.Errorf("TypeFromName(%q) = %v, want %v", tt.name, got, tt.expected)
			}
		})
	}
}

func TestTypeComparable(t *testing.T) {
	tests := []struct {
		a, b     Type
		expected bool
	}{
		{TypeInteger, TypeInteger, true},
		{TypeInteger, TypeReal, true},
		{TypeInteger, TypeNumeric, true},
		{TypeReal, TypeNumeric, true},
		{TypeText, TypeText, true},
		{TypeText, TypeBlob, true},
		{TypeNull, TypeInteger, true},
		{TypeNull, TypeText, true},
		{TypeAny, TypeInteger, true},
		{TypeInteger, TypeText, false},
		{TypeReal, TypeBlob, false},
	}

	for _, tt := range tests {
		t.Run(tt.a.String()+"_"+tt.b.String(), func(t *testing.T) {
			got := tt.a.IsComparable(tt.b)
			if got != tt.expected {
				t.Errorf("%v.IsComparable(%v) = %v, want %v", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}

func TestCommonType(t *testing.T) {
	tests := []struct {
		a, b     Type
		expected Type
	}{
		{TypeInteger, TypeInteger, TypeInteger},
		{TypeInteger, TypeReal, TypeReal},
		{TypeReal, TypeInteger, TypeReal},
		{TypeInteger, TypeNumeric, TypeNumeric},
		{TypeNull, TypeInteger, TypeInteger},
		{TypeText, TypeText, TypeText},
		{TypeText, TypeBlob, TypeText},
	}

	for _, tt := range tests {
		t.Run(tt.a.String()+"_"+tt.b.String(), func(t *testing.T) {
			got := CommonType(tt.a, tt.b)
			if got != tt.expected {
				t.Errorf("CommonType(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}

// Function lookup tests

func TestLookupFunction(t *testing.T) {
	tests := []struct {
		name        string
		exists      bool
		isAggregate bool
	}{
		{"COUNT", true, true},
		{"SUM", true, true},
		{"AVG", true, true},
		{"MIN", true, true},
		{"MAX", true, true},
		{"UPPER", true, false},
		{"LOWER", true, false},
		{"LENGTH", true, false},
		{"COALESCE", true, false},
		{"UNKNOWN_FUNC", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig, ok := LookupFunction(tt.name)
			if ok != tt.exists {
				t.Errorf("LookupFunction(%q) exists = %v, want %v", tt.name, ok, tt.exists)
			}
			if ok && sig.IsAggregate != tt.isAggregate {
				t.Errorf("LookupFunction(%q).IsAggregate = %v, want %v", tt.name, sig.IsAggregate, tt.isAggregate)
			}
		})
	}
}

// SELECT analysis tests

func TestAnalyzeSelectBasic(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name string
		sql  string
	}{
		{"select star", "SELECT * FROM users"},
		{"select columns", "SELECT id, name FROM users"},
		{"select with alias", "SELECT id AS user_id, name AS full_name FROM users"},
		{"select with where", "SELECT * FROM users WHERE id = 1"},
		{"select with complex where", "SELECT * FROM users WHERE id = 1 AND name = 'John'"},
		{"select with order by", "SELECT * FROM users ORDER BY name ASC"},
		{"select with limit", "SELECT * FROM users LIMIT 10"},
		{"select with limit offset", "SELECT * FROM users LIMIT 10 OFFSET 5"},
		{"select distinct", "SELECT DISTINCT name FROM users"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}
}

func TestAnalyzeSelectJoin(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name string
		sql  string
	}{
		{"inner join", "SELECT * FROM users JOIN orders ON users.id = orders.user_id"},
		{"left join", "SELECT * FROM users LEFT JOIN orders ON users.id = orders.user_id"},
		{"join with alias", "SELECT u.id, o.amount FROM users u JOIN orders o ON u.id = o.user_id"},
		{"multiple joins", "SELECT * FROM users u JOIN orders o ON u.id = o.user_id JOIN products p ON p.id = 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}
}

func TestAnalyzeSelectQualifiedWildcard(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	valid := []struct {
		name string
		sql  string
	}{
		{"alias wildcard", "SELECT u.* FROM users u"},
		{"table name wildcard", "SELECT users.* FROM users"},
		{"join alias wildcard", "SELECT u.* FROM users u JOIN orders o ON u.id = o.user_id"},
		{"mixed wildcard and column", "SELECT u.*, o.amount FROM users u JOIN orders o ON u.id = o.user_id"},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			err := analyzer.Analyze(parse(t, tt.sql))
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}

	invalid := []struct {
		name    string
		sql     string
		errType ErrorType
	}{
		{"unknown qualifier", "SELECT nope.* FROM users u", ErrTableNotFound},
		{"aggregate without group by", "SELECT u.*, COUNT(*) FROM users u", ErrNonAggregateInSelect},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			err := analyzer.Analyze(parse(t, tt.sql))
			if err == nil {
				t.Fatalf("Analyze(%q) expected error, got nil", tt.sql)
			}
			analysisErr, ok := err.(*AnalysisError)
			if !ok {
				t.Fatalf("expected AnalysisError, got %T", err)
			}
			if analysisErr.Type != tt.errType {
				t.Errorf("expected error type %v, got %v (%s)", tt.errType, analysisErr.Type, analysisErr.Message)
			}
		})
	}
}

func TestAnalyzeSelectAggregate(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name string
		sql  string
	}{
		{"count star", "SELECT COUNT(*) FROM users"},
		{"count column", "SELECT COUNT(id) FROM users"},
		{"sum", "SELECT SUM(age) FROM users"},
		{"avg", "SELECT AVG(balance) FROM users"},
		{"min max", "SELECT MIN(age), MAX(age) FROM users"},
		{"group by", "SELECT name, COUNT(*) FROM users GROUP BY name"},
		{"group by having", "SELECT name, COUNT(*) FROM users GROUP BY name HAVING COUNT(*) > 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}
}

func TestAnalyzeSelectErrors(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name    string
		sql     string
		errType ErrorType
	}{
		{"table not found", "SELECT * FROM nonexistent", ErrTableNotFound},
		{"column not found", "SELECT nonexistent FROM users", ErrColumnNotFound},
		{"aggregate in where", "SELECT * FROM users WHERE COUNT(*) > 0", ErrAggregateInWhere},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err == nil {
				t.Errorf("Analyze(%q) expected error, got nil", tt.sql)
				return
			}
			if ae, ok := err.(*AnalysisError); ok {
				if ae.Type != tt.errType {
					t.Errorf("Analyze(%q) error type = %v, want %v", tt.sql, ae.Type, tt.errType)
				}
			}
		})
	}
}

// INSERT analysis tests

func TestAnalyzeInsert(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name string
		sql  string
	}{
		{"insert all columns", "INSERT INTO users VALUES (1, 'John', 'john@example.com', 30, TRUE, 100.50)"},
		{"insert with columns", "INSERT INTO users (id, name, active) VALUES (1, 'John', TRUE)"},
		{"insert multiple rows", "INSERT INTO users (id, name, active) VALUES (1, 'John', TRUE), (2, 'Jane', FALSE)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}
}

func TestAnalyzeInsertErrors(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name    string
		sql     string
		errType ErrorType
	}{
		{"table not found", "INSERT INTO nonexistent VALUES (1)", ErrTableNotFound},
		{"column not found", "INSERT INTO users (nonexistent) VALUES (1)", ErrColumnNotFound},
		{"wrong column count", "INSERT INTO users (id, name) VALUES (1)", ErrTypeMismatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err == nil {
				t.Errorf("Analyze(%q) expected error, got nil", tt.sql)
				return
			}
			if ae, ok := err.(*AnalysisError); ok {
				if ae.Type != tt.errType {
					t.Errorf("Analyze(%q) error type = %v, want %v", tt.sql, ae.Type, tt.errType)
				}
			}
		})
	}
}

// UPDATE analysis tests

func TestAnalyzeUpdate(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name string
		sql  string
	}{
		{"update single column", "UPDATE users SET name = 'John' WHERE id = 1"},
		{"update multiple columns", "UPDATE users SET name = 'John', age = 30 WHERE id = 1"},
		{"update with expression", "UPDATE users SET age = age + 1 WHERE active = TRUE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}
}

func TestAnalyzeUpdateErrors(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name    string
		sql     string
		errType ErrorType
	}{
		{"table not found", "UPDATE nonexistent SET x = 1", ErrTableNotFound},
		{"column not found", "UPDATE users SET nonexistent = 1", ErrColumnNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err == nil {
				t.Errorf("Analyze(%q) expected error, got nil", tt.sql)
				return
			}
			if ae, ok := err.(*AnalysisError); ok {
				if ae.Type != tt.errType {
					t.Errorf("Analyze(%q) error type = %v, want %v", tt.sql, ae.Type, tt.errType)
				}
			}
		})
	}
}

// DELETE analysis tests

func TestAnalyzeDelete(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name string
		sql  string
	}{
		{"delete all", "DELETE FROM users"},
		{"delete with where", "DELETE FROM users WHERE id = 1"},
		{"delete with complex where", "DELETE FROM users WHERE active = FALSE AND age < 18"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}
}

// CREATE TABLE analysis tests

func TestAnalyzeCreateTable(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{"basic table", "CREATE TABLE test (id INTEGER PRIMARY KEY, name TEXT)"},
		{"with constraints", "CREATE TABLE test (id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT UNIQUE)"},
		{"with default", "CREATE TABLE test (id INTEGER PRIMARY KEY, active BOOLEAN DEFAULT TRUE)"},
		{"if not exists", "CREATE TABLE IF NOT EXISTS test (id INTEGER)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use fresh catalog for each test
			c := NewCatalog()
			a := New(c)
			stmt := parse(t, tt.sql)
			err := a.Analyze(stmt)
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}
}

func TestAnalyzeCreateTableErrors(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name    string
		sql     string
		errType ErrorType
	}{
		{"table exists", "CREATE TABLE users (id INTEGER)", ErrTableExists},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err == nil {
				t.Errorf("Analyze(%q) expected error, got nil", tt.sql)
				return
			}
			if ae, ok := err.(*AnalysisError); ok {
				if ae.Type != tt.errType {
					t.Errorf("Analyze(%q) error type = %v, want %v", tt.sql, ae.Type, tt.errType)
				}
			}
		})
	}
}

// DROP TABLE analysis tests

func TestAnalyzeDropTable(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{"drop existing", "DROP TABLE products"},
		{"drop if exists", "DROP TABLE IF EXISTS nonexistent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use fresh catalog for each test
			c := setupCatalog()
			a := New(c)
			stmt := parse(t, tt.sql)
			err := a.Analyze(stmt)
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}
}

func TestAnalyzeDropTableErrors(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name    string
		sql     string
		errType ErrorType
	}{
		{"table not found", "DROP TABLE nonexistent", ErrTableNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err == nil {
				t.Errorf("Analyze(%q) expected error, got nil", tt.sql)
				return
			}
			if ae, ok := err.(*AnalysisError); ok {
				if ae.Type != tt.errType {
					t.Errorf("Analyze(%q) error type = %v, want %v", tt.sql, ae.Type, tt.errType)
				}
			}
		})
	}
}

// Expression analysis tests

func TestAnalyzeExpressions(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name string
		sql  string
	}{
		{"arithmetic", "SELECT 1 + 2 * 3 FROM users"},
		{"comparison", "SELECT * FROM users WHERE age > 18"},
		{"logical", "SELECT * FROM users WHERE active = TRUE AND age >= 21"},
		{"is null", "SELECT * FROM users WHERE email IS NULL"},
		{"is not null", "SELECT * FROM users WHERE email IS NOT NULL"},
		{"in list", "SELECT * FROM users WHERE id IN (1, 2, 3)"},
		{"not in", "SELECT * FROM users WHERE id NOT IN (1, 2, 3)"},
		{"between", "SELECT * FROM users WHERE age BETWEEN 18 AND 65"},
		{"like", "SELECT * FROM users WHERE name LIKE 'J%'"},
		{"case when", "SELECT CASE WHEN age >= 18 THEN 'adult' ELSE 'minor' END FROM users"},
		{"cast", "SELECT CAST(age AS TEXT) FROM users"},
		{"coalesce", "SELECT COALESCE(email, 'no email') FROM users"},
		{"function", "SELECT UPPER(name), LENGTH(email) FROM users"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}
}

func TestAnalyzeFunctionErrors(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name    string
		sql     string
		errType ErrorType
	}{
		{"unknown function", "SELECT UNKNOWN_FUNC(id) FROM users", ErrInvalidFunction},
		{"wrong arg count", "SELECT UPPER() FROM users", ErrInvalidArgCount},
		{"too many args", "SELECT LENGTH(name, 1) FROM users", ErrInvalidArgCount},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err == nil {
				t.Errorf("Analyze(%q) expected error, got nil", tt.sql)
				return
			}
			if ae, ok := err.(*AnalysisError); ok {
				if ae.Type != tt.errType {
					t.Errorf("Analyze(%q) error type = %v, want %v", tt.sql, ae.Type, tt.errType)
				}
			}
		})
	}
}

// Subquery tests

func TestAnalyzeSubqueries(t *testing.T) {
	catalog := setupCatalog()
	analyzer := New(catalog)

	tests := []struct {
		name string
		sql  string
	}{
		{"in subquery", "SELECT * FROM users WHERE id IN (SELECT user_id FROM orders)"},
		{"exists subquery", "SELECT * FROM users WHERE EXISTS (SELECT 1 FROM orders WHERE orders.user_id = users.id)"},
		{"scalar subquery", "SELECT (SELECT COUNT(*) FROM orders) FROM users"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parse(t, tt.sql)
			err := analyzer.Analyze(stmt)
			if err != nil {
				t.Errorf("Analyze(%q) error: %v", tt.sql, err)
			}
		})
	}
}

// Scope tests

func TestScope(t *testing.T) {
	scope := NewScope(nil)

	table := &TableInfo{
		Name: "users",
		Columns: []ColumnInfo{
			{Name: "id", Type: TypeInteger},
			{Name: "name", Type: TypeText},
		},
	}
	scope.DefineTable(table)

	// Test table lookup
	if _, ok := scope.LookupTable("users"); !ok {
		t.Error("expected to find table 'users'")
	}
	if _, ok := scope.LookupTable("USERS"); !ok {
		t.Error("expected case-insensitive table lookup")
	}
	if _, ok := scope.LookupTable("nonexistent"); ok {
		t.Error("expected not to find table 'nonexistent'")
	}

	// Test column lookup
	if col, _, ok := scope.LookupColumn("", "id"); !ok || col.Type != TypeInteger {
		t.Error("expected to find column 'id' with type INTEGER")
	}
	if col, _, ok := scope.LookupColumn("users", "name"); !ok || col.Type != TypeText {
		t.Error("expected to find column 'users.name' with type TEXT")
	}
	if _, _, ok := scope.LookupColumn("", "nonexistent"); ok {
		t.Error("expected not to find column 'nonexistent'")
	}
}

func TestCatalog(t *testing.T) {
	catalog := NewCatalog()

	// Create table
	err := catalog.CreateTable(&TableInfo{
		Name: "test",
		Columns: []ColumnInfo{
			{Name: "id", Type: TypeInteger},
		},
	})
	if err != nil {
		t.Errorf("CreateTable error: %v", err)
	}

	// Check exists
	if !catalog.TableExists("test") {
		t.Error("expected table 'test' to exist")
	}

	// Duplicate create should fail
	err = catalog.CreateTable(&TableInfo{Name: "test"})
	if err == nil {
		t.Error("expected error for duplicate table")
	}

	// Drop table
	err = catalog.DropTable("test")
	if err != nil {
		t.Errorf("DropTable error: %v", err)
	}

	// Check not exists
	if catalog.TableExists("test") {
		t.Error("expected table 'test' to not exist after drop")
	}

	// Drop non-existent should fail
	err = catalog.DropTable("test")
	if err == nil {
		t.Error("expected error for dropping non-existent table")
	}
}

// Benchmark

func BenchmarkAnalyzeSelect(b *testing.B) {
	catalog := setupCatalog()
	analyzer := New(catalog)
	sql := "SELECT u.id, u.name, COUNT(o.id) FROM users u LEFT JOIN orders o ON u.id = o.user_id WHERE u.active = TRUE GROUP BY u.id, u.name HAVING COUNT(o.id) > 0 ORDER BY u.name LIMIT 100"

	l := lexer.New(sql)
	p := parser.New(l)
	stmt, _ := p.Parse()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = analyzer.Analyze(stmt)
	}
}
