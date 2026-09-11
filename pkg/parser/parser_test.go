package parser

import (
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
)

func parse(t *testing.T, input string) Statement {
	t.Helper()
	l := lexer.New(input)
	p := New(l)
	stmt, err := p.Parse()
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	return stmt
}

func parseExpr(t *testing.T, input string) Expr {
	t.Helper()
	// Wrap in SELECT to parse as expression
	l := lexer.New("SELECT " + input)
	p := New(l)
	stmt, err := p.Parse()
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	sel := stmt.(*SelectStmt)
	return sel.Columns[0].Expr
}

// SELECT statement tests

func TestParseSelectStar(t *testing.T) {
	stmt := parse(t, "SELECT * FROM users")
	sel, ok := stmt.(*SelectStmt)
	if !ok {
		t.Fatalf("expected SelectStmt, got %T", stmt)
	}

	if len(sel.Columns) != 1 || !sel.Columns[0].Star {
		t.Error("expected SELECT *")
	}

	if len(sel.From) != 1 || sel.From[0].Name != "users" {
		t.Error("expected FROM users")
	}
}

func TestParseSelectColumns(t *testing.T) {
	stmt := parse(t, "SELECT id, name, email FROM users")
	sel := stmt.(*SelectStmt)

	if len(sel.Columns) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(sel.Columns))
	}

	cols := []string{"id", "name", "email"}
	for i, col := range sel.Columns {
		ref, ok := col.Expr.(*ColumnRef)
		if !ok {
			t.Errorf("column %d: expected ColumnRef", i)
			continue
		}
		if ref.Column != cols[i] {
			t.Errorf("column %d: expected %s, got %s", i, cols[i], ref.Column)
		}
	}
}

func TestParseSelectQualifiedWildcard(t *testing.T) {
	stmt := parse(t, "SELECT DISTINCT repo.* FROM repository AS repo LEFT JOIN access ON access.repo_id = repo.id")
	sel := stmt.(*SelectStmt)

	if !sel.Distinct {
		t.Error("expected DISTINCT")
	}
	if len(sel.Columns) != 1 {
		t.Fatalf("expected 1 column, got %d", len(sel.Columns))
	}
	col := sel.Columns[0]
	if col.TableStar != "repo" {
		t.Errorf("expected TableStar=repo, got %q", col.TableStar)
	}
	if col.Star || col.Expr != nil || col.Alias != "" {
		t.Errorf("qualified wildcard should not set Star/Expr/Alias: %+v", col)
	}
}

func TestParseSelectQualifiedWildcardMixed(t *testing.T) {
	stmt := parse(t, "SELECT repo.*, access.mode FROM repository AS repo LEFT JOIN access ON access.repo_id = repo.id")
	sel := stmt.(*SelectStmt)

	if len(sel.Columns) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(sel.Columns))
	}
	if sel.Columns[0].TableStar != "repo" {
		t.Errorf("column 0 TableStar = %q, want repo", sel.Columns[0].TableStar)
	}
	ref, ok := sel.Columns[1].Expr.(*ColumnRef)
	if !ok || ref.Table != "access" || ref.Column != "mode" {
		t.Errorf("column 1 = %+v, want access.mode ColumnRef", sel.Columns[1].Expr)
	}
}

func TestParseSelectCountStarStillWorks(t *testing.T) {
	stmt := parse(t, "SELECT COUNT(*) FROM users")
	sel := stmt.(*SelectStmt)

	if len(sel.Columns) != 1 {
		t.Fatalf("expected 1 column, got %d", len(sel.Columns))
	}
	col := sel.Columns[0]
	if col.TableStar != "" || col.Star {
		t.Errorf("COUNT(*) should not be a wildcard: %+v", col)
	}
	fn, ok := col.Expr.(*FunctionCall)
	if !ok || !fn.Star || fn.Name != "COUNT" {
		t.Errorf("expected COUNT(*) FunctionCall, got %+v", col.Expr)
	}
}

func TestParseSelectWithAlias(t *testing.T) {
	stmt := parse(t, "SELECT id AS user_id, name AS full_name FROM users u")
	sel := stmt.(*SelectStmt)

	if sel.Columns[0].Alias != "user_id" {
		t.Errorf("expected alias user_id, got %s", sel.Columns[0].Alias)
	}
	if sel.Columns[1].Alias != "full_name" {
		t.Errorf("expected alias full_name, got %s", sel.Columns[1].Alias)
	}
	if sel.From[0].Alias != "u" {
		t.Errorf("expected table alias u, got %s", sel.From[0].Alias)
	}
}

func TestParseSelectDistinct(t *testing.T) {
	stmt := parse(t, "SELECT DISTINCT name FROM users")
	sel := stmt.(*SelectStmt)

	if !sel.Distinct {
		t.Error("expected DISTINCT")
	}
}

func TestParseSelectWhere(t *testing.T) {
	stmt := parse(t, "SELECT * FROM users WHERE id = 1")
	sel := stmt.(*SelectStmt)

	if sel.Where == nil {
		t.Fatal("expected WHERE clause")
	}

	binary, ok := sel.Where.(*BinaryExpr)
	if !ok {
		t.Fatalf("expected BinaryExpr, got %T", sel.Where)
	}

	if binary.Op != lexer.TokenEq {
		t.Errorf("expected =, got %v", binary.Op)
	}
}

func TestParseSelectWhereComplex(t *testing.T) {
	stmt := parse(t, "SELECT * FROM users WHERE id = 1 AND name = 'John' OR active = TRUE")
	sel := stmt.(*SelectStmt)

	if sel.Where == nil {
		t.Fatal("expected WHERE clause")
	}

	// Should be: (id = 1 AND name = 'John') OR active = TRUE
	or, ok := sel.Where.(*BinaryExpr)
	if !ok || or.Op != lexer.TokenOR {
		t.Fatal("expected OR at top level")
	}
}

func TestParseSelectOrderBy(t *testing.T) {
	stmt := parse(t, "SELECT * FROM users ORDER BY name ASC, id DESC")
	sel := stmt.(*SelectStmt)

	if len(sel.OrderBy) != 2 {
		t.Fatalf("expected 2 ORDER BY items, got %d", len(sel.OrderBy))
	}

	if sel.OrderBy[0].Desc {
		t.Error("first item should be ASC")
	}
	if !sel.OrderBy[1].Desc {
		t.Error("second item should be DESC")
	}
}

func TestParseSelectLimitOffset(t *testing.T) {
	stmt := parse(t, "SELECT * FROM users LIMIT 10 OFFSET 20")
	sel := stmt.(*SelectStmt)

	if sel.Limit == nil {
		t.Error("expected LIMIT")
	}
	if sel.Offset == nil {
		t.Error("expected OFFSET")
	}

	limit := sel.Limit.(*LiteralExpr)
	if limit.Value != "10" {
		t.Errorf("expected LIMIT 10, got %s", limit.Value)
	}

	offset := sel.Offset.(*LiteralExpr)
	if offset.Value != "20" {
		t.Errorf("expected OFFSET 20, got %s", offset.Value)
	}
}

func TestParseSelectGroupBy(t *testing.T) {
	stmt := parse(t, "SELECT name, COUNT(*) FROM users GROUP BY name")
	sel := stmt.(*SelectStmt)

	if len(sel.GroupBy) != 1 {
		t.Fatalf("expected 1 GROUP BY column, got %d", len(sel.GroupBy))
	}
}

func TestParseSelectHaving(t *testing.T) {
	stmt := parse(t, "SELECT name, COUNT(*) as cnt FROM users GROUP BY name HAVING COUNT(*) > 5")
	sel := stmt.(*SelectStmt)

	if sel.Having == nil {
		t.Fatal("expected HAVING clause")
	}
}

func TestParseSelectJoin(t *testing.T) {
	tests := []struct {
		input    string
		joinType JoinType
	}{
		{"SELECT * FROM a JOIN b ON a.id = b.id", JoinInner},
		{"SELECT * FROM a INNER JOIN b ON a.id = b.id", JoinInner},
		{"SELECT * FROM a LEFT JOIN b ON a.id = b.id", JoinLeft},
		{"SELECT * FROM a LEFT OUTER JOIN b ON a.id = b.id", JoinLeft},
		{"SELECT * FROM a RIGHT JOIN b ON a.id = b.id", JoinRight},
		{"SELECT * FROM a CROSS JOIN b", JoinCross},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			stmt := parse(t, tt.input)
			sel := stmt.(*SelectStmt)

			if sel.From[0].Join == nil {
				t.Fatal("expected JOIN")
			}
			if sel.From[0].Join.Type != tt.joinType {
				t.Errorf("expected join type %v, got %v", tt.joinType, sel.From[0].Join.Type)
			}
		})
	}
}

// INSERT statement tests

func TestParseInsertValues(t *testing.T) {
	stmt := parse(t, "INSERT INTO users (name, age) VALUES ('John', 30)")
	ins, ok := stmt.(*InsertStmt)
	if !ok {
		t.Fatalf("expected InsertStmt, got %T", stmt)
	}

	if ins.Table.Name != "users" {
		t.Errorf("expected table users, got %s", ins.Table.Name)
	}

	if len(ins.Columns) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(ins.Columns))
	}

	if len(ins.Values) != 1 || len(ins.Values[0]) != 2 {
		t.Error("expected 1 row with 2 values")
	}
}

func TestParseInsertMultipleRows(t *testing.T) {
	stmt := parse(t, "INSERT INTO users VALUES (1, 'John'), (2, 'Jane')")
	ins := stmt.(*InsertStmt)

	if len(ins.Values) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(ins.Values))
	}
}

func TestParseInsertOrReplace(t *testing.T) {
	stmt := parse(t, "INSERT OR REPLACE INTO users (id, name) VALUES (1, 'John')")
	ins := stmt.(*InsertStmt)

	if ins.OnConflict != ConflictReplace {
		t.Errorf("expected ConflictReplace, got %v", ins.OnConflict)
	}
	if ins.Table.Name != "users" {
		t.Errorf("expected table users, got %s", ins.Table.Name)
	}
}

func TestParseInsertOrIgnore(t *testing.T) {
	stmt := parse(t, "INSERT OR IGNORE INTO users (id, name) VALUES (1, 'John')")
	ins := stmt.(*InsertStmt)

	if ins.OnConflict != ConflictIgnore {
		t.Errorf("expected ConflictIgnore, got %v", ins.OnConflict)
	}
}

func TestParseInsertOrFail(t *testing.T) {
	stmt := parse(t, "INSERT OR FAIL INTO users (id, name) VALUES (1, 'John')")
	ins := stmt.(*InsertStmt)

	if ins.OnConflict != ConflictFail {
		t.Errorf("expected ConflictFail, got %v", ins.OnConflict)
	}
}

func TestParseInsertOrAbort(t *testing.T) {
	stmt := parse(t, "INSERT OR ABORT INTO users (id, name) VALUES (1, 'John')")
	ins := stmt.(*InsertStmt)

	if ins.OnConflict != ConflictAbort {
		t.Errorf("expected ConflictAbort, got %v", ins.OnConflict)
	}
}

// UPDATE statement tests

func TestParseUpdate(t *testing.T) {
	stmt := parse(t, "UPDATE users SET name = 'John', age = 30 WHERE id = 1")
	upd, ok := stmt.(*UpdateStmt)
	if !ok {
		t.Fatalf("expected UpdateStmt, got %T", stmt)
	}

	if upd.Table.Name != "users" {
		t.Errorf("expected table users, got %s", upd.Table.Name)
	}

	if len(upd.Set) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(upd.Set))
	}

	if upd.Where == nil {
		t.Error("expected WHERE clause")
	}
}

// DELETE statement tests

func TestParseDelete(t *testing.T) {
	stmt := parse(t, "DELETE FROM users WHERE id = 1")
	del, ok := stmt.(*DeleteStmt)
	if !ok {
		t.Fatalf("expected DeleteStmt, got %T", stmt)
	}

	if del.Table.Name != "users" {
		t.Errorf("expected table users, got %s", del.Table.Name)
	}

	if del.Where == nil {
		t.Error("expected WHERE clause")
	}
}

func TestParseDeleteAll(t *testing.T) {
	stmt := parse(t, "DELETE FROM users")
	del := stmt.(*DeleteStmt)

	if del.Where != nil {
		t.Error("expected no WHERE clause")
	}
}

// CREATE TABLE tests

func TestParseCreateTable(t *testing.T) {
	stmt := parse(t, `CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		email VARCHAR(255) UNIQUE,
		age INTEGER DEFAULT 0
	)`)

	create, ok := stmt.(*CreateTableStmt)
	if !ok {
		t.Fatalf("expected CreateTableStmt, got %T", stmt)
	}

	if create.Table.Name != "users" {
		t.Errorf("expected table users, got %s", create.Table.Name)
	}

	if len(create.Columns) != 4 {
		t.Fatalf("expected 4 columns, got %d", len(create.Columns))
	}

	// Check id column
	if create.Columns[0].Name != "id" {
		t.Error("expected first column to be id")
	}
	if create.Columns[0].Type.Name != "INTEGER" {
		t.Error("expected INTEGER type")
	}

	// Check name column has NOT NULL
	found := false
	for _, c := range create.Columns[1].Constraints {
		if c.Type == ConstraintNotNull {
			found = true
		}
	}
	if !found {
		t.Error("expected NOT NULL constraint on name")
	}

	// Check email has VARCHAR(255)
	if create.Columns[2].Type.Name != "VARCHAR" || create.Columns[2].Type.Precision != 255 {
		t.Error("expected VARCHAR(255) for email")
	}
}

func TestParseCreateTableIfNotExists(t *testing.T) {
	stmt := parse(t, "CREATE TABLE IF NOT EXISTS users (id INTEGER)")
	create := stmt.(*CreateTableStmt)

	if !create.IfNotExists {
		t.Error("expected IF NOT EXISTS")
	}
}

func TestParseCreateTableWithConstraints(t *testing.T) {
	stmt := parse(t, `CREATE TABLE orders (
		id INTEGER,
		user_id INTEGER,
		PRIMARY KEY (id),
		FOREIGN KEY (user_id) REFERENCES users(id)
	)`)

	create := stmt.(*CreateTableStmt)

	if len(create.Constraints) != 2 {
		t.Fatalf("expected 2 table constraints, got %d", len(create.Constraints))
	}

	// Check PRIMARY KEY
	if create.Constraints[0].Type != ConstraintPrimaryKey {
		t.Error("expected PRIMARY KEY constraint")
	}

	// Check FOREIGN KEY
	if create.Constraints[1].Type != ConstraintForeignKey {
		t.Error("expected FOREIGN KEY constraint")
	}
	if create.Constraints[1].RefTable != "users" {
		t.Errorf("expected reference to users, got %s", create.Constraints[1].RefTable)
	}
}

func TestParseCreateTableExplicitNullable(t *testing.T) {
	stmt := parse(t, `CREATE TABLE users (
		id INTEGER,
		full_name TEXT NULL,
		nickname TEXT NOT NULL,
		created_at TIMESTAMP NULL NOT NULL
	)`)

	create, ok := stmt.(*CreateTableStmt)
	if !ok {
		t.Fatalf("expected CreateTableStmt, got %T", stmt)
	}

	if len(create.Columns) != 4 {
		t.Fatalf("expected 4 columns, got %d", len(create.Columns))
	}

	// full_name TEXT NULL: explicit NULL is a no-op, so no constraint is added.
	fullName := create.Columns[1]
	if fullName.Name != "full_name" || fullName.Type.Name != "TEXT" {
		t.Fatalf("unexpected full_name column: %+v", fullName)
	}
	if len(fullName.Constraints) != 0 {
		t.Errorf("explicit NULL should not produce a constraint, got %d", len(fullName.Constraints))
	}

	// nickname TEXT NOT NULL still records NOT NULL.
	nickname := create.Columns[2]
	if len(nickname.Constraints) != 1 || nickname.Constraints[0].Type != ConstraintNotNull {
		t.Errorf("expected NOT NULL on nickname, got %+v", nickname.Constraints)
	}

	// created_at TIMESTAMP NULL NOT NULL: NOT NULL wins regardless of ordering.
	created := create.Columns[3]
	if len(created.Constraints) != 1 || created.Constraints[0].Type != ConstraintNotNull {
		t.Errorf("expected NOT NULL on created_at, got %+v", created.Constraints)
	}
}

// DROP TABLE tests

func TestParseDropTable(t *testing.T) {
	stmt := parse(t, "DROP TABLE users")
	drop, ok := stmt.(*DropTableStmt)
	if !ok {
		t.Fatalf("expected DropTableStmt, got %T", stmt)
	}

	if len(drop.Tables) != 1 || drop.Tables[0].Name != "users" {
		t.Error("expected DROP TABLE users")
	}
}

func TestParseCreateTableDefaultSignedNumeric(t *testing.T) {
	stmt := parse(t, `CREATE TABLE repo (
		id INTEGER PRIMARY KEY,
		max_repo_creation INTEGER DEFAULT -1 NOT NULL,
		delta INTEGER DEFAULT +5,
		tally INTEGER DEFAULT (-7)
	)`)

	create, ok := stmt.(*CreateTableStmt)
	if !ok {
		t.Fatalf("expected CreateTableStmt, got %T", stmt)
	}
	if len(create.Columns) != 4 {
		t.Fatalf("expected 4 columns, got %d", len(create.Columns))
	}

	// xorm real shape: DEFAULT -1 followed by NOT NULL.
	col := create.Columns[1]
	if len(col.Constraints) != 2 {
		t.Fatalf("expected DEFAULT + NOT NULL, got %d constraints", len(col.Constraints))
	}
	var defaultExpr Expr
	var notNull bool
	for _, c := range col.Constraints {
		switch c.Type {
		case ConstraintDefault:
			defaultExpr = c.Default
		case ConstraintNotNull:
			notNull = true
		}
	}
	if !notNull {
		t.Error("expected NOT NULL constraint on max_repo_creation")
	}
	unary, ok := defaultExpr.(*UnaryExpr)
	if !ok || unary.Op != lexer.TokenMinus {
		t.Fatalf("expected unary minus default, got %T %+v", defaultExpr, defaultExpr)
	}
	lit, ok := unary.Operand.(*LiteralExpr)
	if !ok || lit.Value != "1" {
		t.Fatalf("expected -1 literal, got %+v", unary.Operand)
	}

	// Positive signed default: DEFAULT +5.
	plus, ok := create.Columns[2].Constraints[0].Default.(*UnaryExpr)
	if !ok || plus.Op != lexer.TokenPlus {
		t.Fatalf("expected unary plus default, got %+v", create.Columns[2].Constraints[0].Default)
	}

	// Parenthesized signed default: DEFAULT (-7).
	paren, ok := create.Columns[3].Constraints[0].Default.(*ParenExpr)
	if !ok {
		t.Fatalf("expected parenthesized default, got %T", create.Columns[3].Constraints[0].Default)
	}
	inner, ok := paren.Expr.(*UnaryExpr)
	if !ok || inner.Op != lexer.TokenMinus {
		t.Fatalf("expected unary minus inside parens, got %T %+v", paren.Expr, paren.Expr)
	}
}

func TestParseCreateTableUUIDType(t *testing.T) {
	stmt := parse(t, `CREATE TABLE upload (
		id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
		uuid UUID NULL,
		name TEXT NULL
	)`)

	create, ok := stmt.(*CreateTableStmt)
	if !ok {
		t.Fatalf("expected CreateTableStmt, got %T", stmt)
	}
	if len(create.Columns) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(create.Columns))
	}

	uuidCol := create.Columns[1]
	if uuidCol.Name != "uuid" {
		t.Errorf("column name = %q, want %q", uuidCol.Name, "uuid")
	}
	if uuidCol.Type.Name != "UUID" {
		t.Errorf("column type = %q, want %q", uuidCol.Type.Name, "UUID")
	}
	if len(uuidCol.Constraints) != 0 {
		t.Errorf("explicit NULL should produce no constraints, got %d", len(uuidCol.Constraints))
	}
}

func TestParseDropTableIfExists(t *testing.T) {
	stmt := parse(t, "DROP TABLE IF EXISTS users")
	drop := stmt.(*DropTableStmt)

	if !drop.IfExists {
		t.Error("expected IF EXISTS")
	}
}

// Expression tests

func TestParseExprArithmetic(t *testing.T) {
	expr := parseExpr(t, "1 + 2 * 3")

	// Should be: 1 + (2 * 3) due to precedence
	add, ok := expr.(*BinaryExpr)
	if !ok || add.Op != lexer.TokenPlus {
		t.Fatal("expected + at top level")
	}

	mul, ok := add.Right.(*BinaryExpr)
	if !ok || mul.Op != lexer.TokenStar {
		t.Fatal("expected * on right side")
	}
}

func TestParseExprParens(t *testing.T) {
	expr := parseExpr(t, "(1 + 2) * 3")

	// Should be: (1 + 2) * 3
	mul, ok := expr.(*BinaryExpr)
	if !ok || mul.Op != lexer.TokenStar {
		t.Fatal("expected * at top level")
	}

	paren, ok := mul.Left.(*ParenExpr)
	if !ok {
		t.Fatal("expected ParenExpr on left")
	}

	add, ok := paren.Expr.(*BinaryExpr)
	if !ok || add.Op != lexer.TokenPlus {
		t.Fatal("expected + inside parens")
	}
}

func TestParseExprComparison(t *testing.T) {
	tests := []struct {
		input string
		op    lexer.TokenType
	}{
		{"a = b", lexer.TokenEq},
		{"a <> b", lexer.TokenNeq},
		{"a != b", lexer.TokenNeq},
		{"a < b", lexer.TokenLt},
		{"a <= b", lexer.TokenLte},
		{"a > b", lexer.TokenGt},
		{"a >= b", lexer.TokenGte},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			expr := parseExpr(t, tt.input)
			binary, ok := expr.(*BinaryExpr)
			if !ok {
				t.Fatalf("expected BinaryExpr, got %T", expr)
			}
			if binary.Op != tt.op {
				t.Errorf("expected %v, got %v", tt.op, binary.Op)
			}
		})
	}
}

func TestParseExprIsNull(t *testing.T) {
	tests := []struct {
		input string
		not   bool
	}{
		{"a IS NULL", false},
		{"a IS NOT NULL", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			expr := parseExpr(t, tt.input)
			isNull, ok := expr.(*IsNullExpr)
			if !ok {
				t.Fatalf("expected IsNullExpr, got %T", expr)
			}
			if isNull.Not != tt.not {
				t.Errorf("expected Not=%v, got %v", tt.not, isNull.Not)
			}
		})
	}
}

func TestParseExprIn(t *testing.T) {
	tests := []struct {
		input string
		not   bool
	}{
		{"a IN (1, 2, 3)", false},
		{"a NOT IN (1, 2, 3)", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			expr := parseExpr(t, tt.input)
			in, ok := expr.(*InExpr)
			if !ok {
				t.Fatalf("expected InExpr, got %T", expr)
			}
			if in.Not != tt.not {
				t.Errorf("expected Not=%v, got %v", tt.not, in.Not)
			}
			if len(in.Values) != 3 {
				t.Errorf("expected 3 values, got %d", len(in.Values))
			}
		})
	}
}

func TestParseExprBetween(t *testing.T) {
	tests := []struct {
		input string
		not   bool
	}{
		{"a BETWEEN 1 AND 10", false},
		{"a NOT BETWEEN 1 AND 10", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			expr := parseExpr(t, tt.input)
			between, ok := expr.(*BetweenExpr)
			if !ok {
				t.Fatalf("expected BetweenExpr, got %T", expr)
			}
			if between.Not != tt.not {
				t.Errorf("expected Not=%v, got %v", tt.not, between.Not)
			}
		})
	}
}

func TestParseExprLike(t *testing.T) {
	tests := []struct {
		input string
		not   bool
	}{
		{"name LIKE '%test%'", false},
		{"name NOT LIKE '%test%'", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			expr := parseExpr(t, tt.input)
			like, ok := expr.(*LikeExpr)
			if !ok {
				t.Fatalf("expected LikeExpr, got %T", expr)
			}
			if like.Not != tt.not {
				t.Errorf("expected Not=%v, got %v", tt.not, like.Not)
			}
		})
	}
}

func TestParseExprCase(t *testing.T) {
	expr := parseExpr(t, "CASE WHEN x = 1 THEN 'one' WHEN x = 2 THEN 'two' ELSE 'other' END")

	caseExpr, ok := expr.(*CaseExpr)
	if !ok {
		t.Fatalf("expected CaseExpr, got %T", expr)
	}

	if len(caseExpr.Whens) != 2 {
		t.Errorf("expected 2 WHEN clauses, got %d", len(caseExpr.Whens))
	}

	if caseExpr.Else == nil {
		t.Error("expected ELSE clause")
	}
}

func TestParseExprCast(t *testing.T) {
	expr := parseExpr(t, "CAST(x AS INTEGER)")

	cast, ok := expr.(*CastExpr)
	if !ok {
		t.Fatalf("expected CastExpr, got %T", expr)
	}

	if cast.Type.Name != "INTEGER" {
		t.Errorf("expected INTEGER type, got %s", cast.Type.Name)
	}
}

func TestParseExprFunction(t *testing.T) {
	tests := []struct {
		input    string
		name     string
		argCount int
		star     bool
		distinct bool
	}{
		{"COUNT(*)", "COUNT", 0, true, false},
		{"COUNT(id)", "COUNT", 1, false, false},
		{"COUNT(DISTINCT id)", "COUNT", 1, false, true},
		{"SUM(amount)", "SUM", 1, false, false},
		{"UPPER(name)", "UPPER", 1, false, false},
		{"COALESCE(a, b, c)", "COALESCE", 3, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			expr := parseExpr(t, tt.input)
			fn, ok := expr.(*FunctionCall)
			if !ok {
				t.Fatalf("expected FunctionCall, got %T", expr)
			}
			if fn.Name != tt.name {
				t.Errorf("expected name %s, got %s", tt.name, fn.Name)
			}
			if len(fn.Args) != tt.argCount {
				t.Errorf("expected %d args, got %d", tt.argCount, len(fn.Args))
			}
			if fn.Star != tt.star {
				t.Errorf("expected Star=%v, got %v", tt.star, fn.Star)
			}
			if fn.Distinct != tt.distinct {
				t.Errorf("expected Distinct=%v, got %v", tt.distinct, fn.Distinct)
			}
		})
	}
}

func TestParseExprSubquery(t *testing.T) {
	expr := parseExpr(t, "id IN (SELECT user_id FROM orders)")

	in, ok := expr.(*InExpr)
	if !ok {
		t.Fatalf("expected InExpr, got %T", expr)
	}

	if in.Subquery == nil {
		t.Error("expected subquery")
	}
}

func TestParseExprExists(t *testing.T) {
	expr := parseExpr(t, "EXISTS (SELECT 1 FROM users WHERE id = 1)")

	exists, ok := expr.(*ExistsExpr)
	if !ok {
		t.Fatalf("expected ExistsExpr, got %T", expr)
	}

	if exists.Subquery == nil {
		t.Error("expected subquery")
	}
}

func TestParseExprColumnRef(t *testing.T) {
	tests := []struct {
		input  string
		table  string
		column string
	}{
		{"id", "", "id"},
		{"users.id", "users", "id"},
		{"u.name", "u", "name"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			expr := parseExpr(t, tt.input)
			ref, ok := expr.(*ColumnRef)
			if !ok {
				t.Fatalf("expected ColumnRef, got %T", expr)
			}
			if ref.Table != tt.table {
				t.Errorf("expected table %q, got %q", tt.table, ref.Table)
			}
			if ref.Column != tt.column {
				t.Errorf("expected column %q, got %q", tt.column, ref.Column)
			}
		})
	}
}

// Error cases

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"missing columns", "SELECT FROM users"},
		{"missing VALUES", "INSERT INTO users"},
		{"missing SET", "UPDATE users WHERE id = 1"},
		{"missing table name", "DELETE FROM WHERE id = 1"},
		{"unclosed paren", "SELECT * FROM users WHERE (id = 1"},
		{"invalid token", "SELECT @ FROM users"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := lexer.New(tt.input)
			p := New(l)
			_, err := p.Parse()
			if err == nil {
				t.Error("expected parse error")
			}
		})
	}
}

// Multiple statements

func TestParseMultiple(t *testing.T) {
	input := `
		SELECT * FROM users;
		INSERT INTO users VALUES (1, 'John');
		DELETE FROM users WHERE id = 1
	`

	l := lexer.New(input)
	p := New(l)
	stmts, err := p.ParseMultiple()
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if len(stmts) != 3 {
		t.Errorf("expected 3 statements, got %d", len(stmts))
	}
}

func TestParseRejectsTrailingReturning(t *testing.T) {
	l := lexer.New("INSERT INTO users (id) VALUES (1) RETURNING id")
	if _, err := New(l).Parse(); err == nil {
		t.Fatal("expected RETURNING to be rejected before execution")
	}
}

func TestParsePostgresCompatibilityClauses(t *testing.T) {
	t.Run("alter add column if not exists", func(t *testing.T) {
		stmt := parse(t, "ALTER TABLE users ADD COLUMN IF NOT EXISTS revision INTEGER DEFAULT 0")
		action := stmt.(*AlterTableStmt).Action.(*AddColumnAction)
		if !action.IfNotExists || action.Column.Name != "revision" {
			t.Fatalf("unexpected action: %#v", action)
		}
	})

	t.Run("on conflict do update", func(t *testing.T) {
		stmt := parse(t, "INSERT INTO users (id, count) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET count = users.count + 1")
		insert := stmt.(*InsertStmt)
		if len(insert.ConflictTarget) != 1 || insert.ConflictTarget[0] != "id" || len(insert.ConflictUpdate) != 1 {
			t.Fatalf("unexpected conflict clause: %#v", insert)
		}
	})

	t.Run("jsonb cast", func(t *testing.T) {
		parse(t, "SELECT CAST('{}' AS JSONB)")
	})
}

// Phase 4: PRAGMA and EXPLAIN tests

func TestParsePragmaTableInfo(t *testing.T) {
	stmt := parse(t, "PRAGMA table_info(users)")
	pragma, ok := stmt.(*PragmaStmt)
	if !ok {
		t.Fatalf("expected PragmaStmt, got %T", stmt)
	}

	if pragma.Name != "table_info" {
		t.Errorf("expected pragma name 'table_info', got %s", pragma.Name)
	}
	if pragma.Arg != "users" {
		t.Errorf("expected arg 'users', got %s", pragma.Arg)
	}
}

func TestParsePragmaTableList(t *testing.T) {
	stmt := parse(t, "PRAGMA table_list")
	pragma, ok := stmt.(*PragmaStmt)
	if !ok {
		t.Fatalf("expected PragmaStmt, got %T", stmt)
	}

	if pragma.Name != "table_list" {
		t.Errorf("expected pragma name 'table_list', got %s", pragma.Name)
	}
}

func TestParsePragmaDatabaseList(t *testing.T) {
	stmt := parse(t, "PRAGMA database_list")
	pragma := stmt.(*PragmaStmt)

	if pragma.Name != "database_list" {
		t.Errorf("expected pragma name 'database_list', got %s", pragma.Name)
	}
}

func TestParsePragmaVersion(t *testing.T) {
	stmt := parse(t, "PRAGMA version")
	pragma := stmt.(*PragmaStmt)

	if pragma.Name != "version" {
		t.Errorf("expected pragma name 'version', got %s", pragma.Name)
	}
}

func TestParseExplain(t *testing.T) {
	stmt := parse(t, "EXPLAIN SELECT * FROM users")
	explain, ok := stmt.(*ExplainStmt)
	if !ok {
		t.Fatalf("expected ExplainStmt, got %T", stmt)
	}

	if explain.QueryPlan {
		t.Error("expected QueryPlan to be false")
	}

	_, ok = explain.Statement.(*SelectStmt)
	if !ok {
		t.Errorf("expected SelectStmt inside EXPLAIN, got %T", explain.Statement)
	}
}

func TestParseExplainQueryPlan(t *testing.T) {
	stmt := parse(t, "EXPLAIN QUERY PLAN SELECT * FROM users WHERE id = 1")
	explain, ok := stmt.(*ExplainStmt)
	if !ok {
		t.Fatalf("expected ExplainStmt, got %T", stmt)
	}

	if !explain.QueryPlan {
		t.Error("expected QueryPlan to be true")
	}

	sel, ok := explain.Statement.(*SelectStmt)
	if !ok {
		t.Errorf("expected SelectStmt inside EXPLAIN, got %T", explain.Statement)
	}

	if sel.Where == nil {
		t.Error("expected WHERE clause in explained statement")
	}
}

func TestParseExplainInsert(t *testing.T) {
	stmt := parse(t, "EXPLAIN INSERT INTO users (name) VALUES ('John')")
	explain := stmt.(*ExplainStmt)

	_, ok := explain.Statement.(*InsertStmt)
	if !ok {
		t.Errorf("expected InsertStmt inside EXPLAIN, got %T", explain.Statement)
	}
}

// Phase 5: Transaction statement tests

func TestParseBegin(t *testing.T) {
	stmt := parse(t, "BEGIN")
	_, ok := stmt.(*BeginStmt)
	if !ok {
		t.Fatalf("expected BeginStmt, got %T", stmt)
	}
}

func TestParseBeginTransaction(t *testing.T) {
	stmt := parse(t, "BEGIN TRANSACTION")
	_, ok := stmt.(*BeginStmt)
	if !ok {
		t.Fatalf("expected BeginStmt, got %T", stmt)
	}
}

func TestParseCommit(t *testing.T) {
	stmt := parse(t, "COMMIT")
	_, ok := stmt.(*CommitStmt)
	if !ok {
		t.Fatalf("expected CommitStmt, got %T", stmt)
	}
}

func TestParseCommitTransaction(t *testing.T) {
	stmt := parse(t, "COMMIT TRANSACTION")
	_, ok := stmt.(*CommitStmt)
	if !ok {
		t.Fatalf("expected CommitStmt, got %T", stmt)
	}
}

func TestParseRollback(t *testing.T) {
	stmt := parse(t, "ROLLBACK")
	rollback, ok := stmt.(*RollbackStmt)
	if !ok {
		t.Fatalf("expected RollbackStmt, got %T", stmt)
	}
	if rollback.Savepoint != "" {
		t.Errorf("expected empty savepoint, got %s", rollback.Savepoint)
	}
}

func TestParseRollbackToSavepoint(t *testing.T) {
	stmt := parse(t, "ROLLBACK TO SAVEPOINT sp1")
	rollback, ok := stmt.(*RollbackStmt)
	if !ok {
		t.Fatalf("expected RollbackStmt, got %T", stmt)
	}
	if rollback.Savepoint != "sp1" {
		t.Errorf("expected savepoint 'sp1', got %s", rollback.Savepoint)
	}
}

func TestParseRollbackTo(t *testing.T) {
	stmt := parse(t, "ROLLBACK TO sp1")
	rollback := stmt.(*RollbackStmt)
	if rollback.Savepoint != "sp1" {
		t.Errorf("expected savepoint 'sp1', got %s", rollback.Savepoint)
	}
}

func TestParseSavepoint(t *testing.T) {
	stmt := parse(t, "SAVEPOINT my_savepoint")
	sp, ok := stmt.(*SavepointStmt)
	if !ok {
		t.Fatalf("expected SavepointStmt, got %T", stmt)
	}
	if sp.Name != "my_savepoint" {
		t.Errorf("expected savepoint name 'my_savepoint', got %s", sp.Name)
	}
}

func TestParseReleaseSavepoint(t *testing.T) {
	stmt := parse(t, "RELEASE SAVEPOINT sp1")
	rel, ok := stmt.(*ReleaseStmt)
	if !ok {
		t.Fatalf("expected ReleaseStmt, got %T", stmt)
	}
	if rel.Name != "sp1" {
		t.Errorf("expected savepoint name 'sp1', got %s", rel.Name)
	}
}

func TestParseRelease(t *testing.T) {
	stmt := parse(t, "RELEASE sp1")
	rel := stmt.(*ReleaseStmt)
	if rel.Name != "sp1" {
		t.Errorf("expected savepoint name 'sp1', got %s", rel.Name)
	}
}

// Benchmark

func BenchmarkParseSelect(b *testing.B) {
	input := `SELECT u.id, u.name, u.email, COUNT(o.id) as order_count
		FROM users u
		LEFT JOIN orders o ON u.id = o.user_id
		WHERE u.active = TRUE AND u.created_at >= '2024-01-01'
		GROUP BY u.id, u.name, u.email
		HAVING COUNT(o.id) > 5
		ORDER BY order_count DESC
		LIMIT 100 OFFSET 0`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := lexer.New(input)
		p := New(l)
		_, _ = p.Parse()
	}
}

func BenchmarkParseCreateTable(b *testing.B) {
	input := `CREATE TABLE users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		email VARCHAR(255) UNIQUE,
		age INTEGER DEFAULT 0,
		active BOOLEAN DEFAULT TRUE,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := lexer.New(input)
		p := New(l)
		_, _ = p.Parse()
	}
}

// CREATE INDEX tests

func TestParseCreateIndex(t *testing.T) {
	stmt := parse(t, "CREATE INDEX idx_email ON users (email)")
	idx, ok := stmt.(*CreateIndexStmt)
	if !ok {
		t.Fatalf("expected CreateIndexStmt, got %T", stmt)
	}

	if idx.Name != "idx_email" {
		t.Errorf("expected index name idx_email, got %s", idx.Name)
	}
	if idx.Table != "users" {
		t.Errorf("expected table users, got %s", idx.Table)
	}
	if len(idx.Columns) != 1 || idx.Columns[0].Name != "email" {
		t.Error("expected column email")
	}
	if idx.Unique {
		t.Error("expected non-unique index")
	}
	if idx.IfNotExists {
		t.Error("expected IfNotExists to be false")
	}
}

func TestParseCreateUniqueIndex(t *testing.T) {
	stmt := parse(t, "CREATE UNIQUE INDEX idx_email ON users (email)")
	idx, ok := stmt.(*CreateIndexStmt)
	if !ok {
		t.Fatalf("expected CreateIndexStmt, got %T", stmt)
	}

	if !idx.Unique {
		t.Error("expected unique index")
	}
}

func TestParseCreateIndexIfNotExists(t *testing.T) {
	stmt := parse(t, "CREATE INDEX IF NOT EXISTS idx_email ON users (email)")
	idx, ok := stmt.(*CreateIndexStmt)
	if !ok {
		t.Fatalf("expected CreateIndexStmt, got %T", stmt)
	}

	if !idx.IfNotExists {
		t.Error("expected IfNotExists to be true")
	}
}

func TestParseCreateIndexMultiColumn(t *testing.T) {
	stmt := parse(t, "CREATE INDEX idx_name_email ON users (name, email)")
	idx, ok := stmt.(*CreateIndexStmt)
	if !ok {
		t.Fatalf("expected CreateIndexStmt, got %T", stmt)
	}

	if len(idx.Columns) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(idx.Columns))
	}
	if idx.Columns[0].Name != "name" {
		t.Errorf("expected first column name, got %s", idx.Columns[0].Name)
	}
	if idx.Columns[1].Name != "email" {
		t.Errorf("expected second column email, got %s", idx.Columns[1].Name)
	}
}

func TestParseCreateIndexWithDesc(t *testing.T) {
	stmt := parse(t, "CREATE INDEX idx_created ON users (created_at DESC)")
	idx, ok := stmt.(*CreateIndexStmt)
	if !ok {
		t.Fatalf("expected CreateIndexStmt, got %T", stmt)
	}

	if len(idx.Columns) != 1 {
		t.Fatalf("expected 1 column, got %d", len(idx.Columns))
	}
	if !idx.Columns[0].Desc {
		t.Error("expected DESC ordering")
	}
}

// DROP INDEX tests

func TestParseDropIndex(t *testing.T) {
	stmt := parse(t, "DROP INDEX idx_email")
	drop, ok := stmt.(*DropIndexStmt)
	if !ok {
		t.Fatalf("expected DropIndexStmt, got %T", stmt)
	}

	if drop.Name != "idx_email" {
		t.Errorf("expected index name idx_email, got %s", drop.Name)
	}
	if drop.IfExists {
		t.Error("expected IfExists to be false")
	}
}

func TestParseDropIndexIfExists(t *testing.T) {
	stmt := parse(t, "DROP INDEX IF EXISTS idx_email")
	drop, ok := stmt.(*DropIndexStmt)
	if !ok {
		t.Fatalf("expected DropIndexStmt, got %T", stmt)
	}

	if !drop.IfExists {
		t.Error("expected IfExists to be true")
	}
}

// Subquery in FROM clause tests

func TestParseSelectFromSubquery(t *testing.T) {
	stmt := parse(t, "SELECT * FROM (SELECT id, name FROM users) AS u")
	sel, ok := stmt.(*SelectStmt)
	if !ok {
		t.Fatalf("expected SelectStmt, got %T", stmt)
	}

	if len(sel.From) != 1 {
		t.Fatalf("expected 1 FROM item, got %d", len(sel.From))
	}

	if sel.From[0].Subquery == nil {
		t.Fatal("expected subquery in FROM")
	}

	if sel.From[0].Alias != "u" {
		t.Errorf("expected alias 'u', got '%s'", sel.From[0].Alias)
	}

	// Check subquery
	subquery := sel.From[0].Subquery
	if len(subquery.Columns) != 2 {
		t.Errorf("expected 2 columns in subquery, got %d", len(subquery.Columns))
	}
	if len(subquery.From) != 1 || subquery.From[0].Name != "users" {
		t.Error("expected subquery FROM users")
	}
}

func TestParseSelectFromSubqueryWithWhere(t *testing.T) {
	stmt := parse(t, "SELECT name FROM (SELECT id, name FROM users WHERE active = TRUE) AS active_users WHERE id > 10")
	sel, ok := stmt.(*SelectStmt)
	if !ok {
		t.Fatalf("expected SelectStmt, got %T", stmt)
	}

	if sel.From[0].Subquery == nil {
		t.Fatal("expected subquery in FROM")
	}

	// Check outer WHERE clause
	if sel.Where == nil {
		t.Error("expected outer WHERE clause")
	}

	// Check subquery WHERE clause
	if sel.From[0].Subquery.Where == nil {
		t.Error("expected subquery WHERE clause")
	}
}

func TestParseSelectFromSubqueryComplex(t *testing.T) {
	stmt := parse(t, "SELECT u.name, u.total FROM (SELECT user_id, SUM(amount) AS total FROM orders GROUP BY user_id) AS u")
	sel, ok := stmt.(*SelectStmt)
	if !ok {
		t.Fatalf("expected SelectStmt, got %T", stmt)
	}

	if sel.From[0].Subquery == nil {
		t.Fatal("expected subquery in FROM")
	}

	subquery := sel.From[0].Subquery
	if len(subquery.GroupBy) == 0 {
		t.Error("expected GROUP BY in subquery")
	}

	// Check that columns reference the alias
	if len(sel.Columns) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(sel.Columns))
	}
}

func TestParseSelectFromNestedSubquery(t *testing.T) {
	stmt := parse(t, "SELECT * FROM (SELECT * FROM (SELECT id FROM users) AS inner_q) AS outer_q")
	sel, ok := stmt.(*SelectStmt)
	if !ok {
		t.Fatalf("expected SelectStmt, got %T", stmt)
	}

	if sel.From[0].Subquery == nil {
		t.Fatal("expected subquery in FROM")
	}

	// Check nested subquery
	outerSubquery := sel.From[0].Subquery
	if len(outerSubquery.From) == 0 || outerSubquery.From[0].Subquery == nil {
		t.Error("expected nested subquery")
	}
}

// ALTER TABLE tests

func TestParseAlterTableAddColumn(t *testing.T) {
	stmt := parse(t, "ALTER TABLE users ADD COLUMN age INTEGER")
	alter, ok := stmt.(*AlterTableStmt)
	if !ok {
		t.Fatalf("expected AlterTableStmt, got %T", stmt)
	}

	if alter.Table != "users" {
		t.Errorf("expected table users, got %s", alter.Table)
	}

	action, ok := alter.Action.(*AddColumnAction)
	if !ok {
		t.Fatalf("expected AddColumnAction, got %T", alter.Action)
	}

	if action.Column.Name != "age" {
		t.Errorf("expected column name age, got %s", action.Column.Name)
	}
	if action.Column.Type.Name != "INTEGER" {
		t.Errorf("expected column type INTEGER, got %s", action.Column.Type.Name)
	}
}

func TestParseAlterTableAddColumnOptional(t *testing.T) {
	stmt := parse(t, "ALTER TABLE users ADD age INTEGER")
	alter, ok := stmt.(*AlterTableStmt)
	if !ok {
		t.Fatalf("expected AlterTableStmt, got %T", stmt)
	}

	action, ok := alter.Action.(*AddColumnAction)
	if !ok {
		t.Fatalf("expected AddColumnAction, got %T", alter.Action)
	}

	if action.Column.Name != "age" {
		t.Errorf("expected column name age, got %s", action.Column.Name)
	}
}

func TestParseAlterTableDropColumn(t *testing.T) {
	stmt := parse(t, "ALTER TABLE users DROP COLUMN email")
	alter, ok := stmt.(*AlterTableStmt)
	if !ok {
		t.Fatalf("expected AlterTableStmt, got %T", stmt)
	}

	action, ok := alter.Action.(*DropColumnAction)
	if !ok {
		t.Fatalf("expected DropColumnAction, got %T", alter.Action)
	}

	if action.Column != "email" {
		t.Errorf("expected column email, got %s", action.Column)
	}
}

func TestParseAlterTableRename(t *testing.T) {
	stmt := parse(t, "ALTER TABLE users RENAME TO customers")
	alter, ok := stmt.(*AlterTableStmt)
	if !ok {
		t.Fatalf("expected AlterTableStmt, got %T", stmt)
	}

	action, ok := alter.Action.(*RenameTableAction)
	if !ok {
		t.Fatalf("expected RenameTableAction, got %T", alter.Action)
	}

	if action.NewName != "customers" {
		t.Errorf("expected new name customers, got %s", action.NewName)
	}
}

func TestParseAlterTableRenameColumn(t *testing.T) {
	stmt := parse(t, "ALTER TABLE users RENAME COLUMN name TO full_name")
	alter, ok := stmt.(*AlterTableStmt)
	if !ok {
		t.Fatalf("expected AlterTableStmt, got %T", stmt)
	}

	action, ok := alter.Action.(*RenameColumnAction)
	if !ok {
		t.Fatalf("expected RenameColumnAction, got %T", alter.Action)
	}

	if action.OldName != "name" {
		t.Errorf("expected old name 'name', got %s", action.OldName)
	}
	if action.NewName != "full_name" {
		t.Errorf("expected new name 'full_name', got %s", action.NewName)
	}
}

// ATTACH/DETACH DATABASE tests

func TestParseAttach(t *testing.T) {
	stmt := parse(t, "ATTACH DATABASE 'test.db' AS testdb")
	attach, ok := stmt.(*AttachStmt)
	if !ok {
		t.Fatalf("expected AttachStmt, got %T", stmt)
	}

	if attach.FilePath != "test.db" {
		t.Errorf("expected file path 'test.db', got '%s'", attach.FilePath)
	}
	if attach.Alias != "testdb" {
		t.Errorf("expected alias 'testdb', got '%s'", attach.Alias)
	}
}

func TestParseAttachOptional(t *testing.T) {
	stmt := parse(t, "ATTACH 'another.db' AS other")
	attach, ok := stmt.(*AttachStmt)
	if !ok {
		t.Fatalf("expected AttachStmt, got %T", stmt)
	}

	if attach.FilePath != "another.db" {
		t.Errorf("expected file path 'another.db', got '%s'", attach.FilePath)
	}
	if attach.Alias != "other" {
		t.Errorf("expected alias 'other', got '%s'", attach.Alias)
	}
}

func TestParseDetach(t *testing.T) {
	stmt := parse(t, "DETACH DATABASE testdb")
	detach, ok := stmt.(*DetachStmt)
	if !ok {
		t.Fatalf("expected DetachStmt, got %T", stmt)
	}

	if detach.Alias != "testdb" {
		t.Errorf("expected alias 'testdb', got '%s'", detach.Alias)
	}
}

func TestParseDetachOptional(t *testing.T) {
	stmt := parse(t, "DETACH other")
	detach, ok := stmt.(*DetachStmt)
	if !ok {
		t.Fatalf("expected DetachStmt, got %T", stmt)
	}

	if detach.Alias != "other" {
		t.Errorf("expected alias 'other', got '%s'", detach.Alias)
	}
}
