package lexer

import (
	"testing"
)

func TestLexerSingleTokens(t *testing.T) {
	tests := []struct {
		input    string
		expected TokenType
		literal  string
	}{
		// Operators
		{"+", TokenPlus, "+"},
		{"-", TokenMinus, "-"},
		{"*", TokenStar, "*"},
		{"/", TokenSlash, "/"},
		{"%", TokenPercent, "%"},
		{"||", TokenConcat, "||"},
		{"=", TokenEq, "="},
		{"<>", TokenNeq, "<>"},
		{"!=", TokenNeq, "!="},
		{"<", TokenLt, "<"},
		{"<=", TokenLte, "<="},
		{">", TokenGt, ">"},
		{">=", TokenGte, ">="},

		// Punctuation
		{"(", TokenLParen, "("},
		{")", TokenRParen, ")"},
		{",", TokenComma, ","},
		{";", TokenSemicolon, ";"},
		{".", TokenDot, "."},

		// Keywords (case insensitive)
		{"SELECT", TokenSELECT, "SELECT"},
		{"select", TokenSELECT, "select"},
		{"SeLeCt", TokenSELECT, "SeLeCt"},
		{"FROM", TokenFROM, "FROM"},
		{"WHERE", TokenWHERE, "WHERE"},
		{"AND", TokenAND, "AND"},
		{"OR", TokenOR, "OR"},
		{"NOT", TokenNOT, "NOT"},
		{"INSERT", TokenINSERT, "INSERT"},
		{"INTO", TokenINTO, "INTO"},
		{"VALUES", TokenVALUES, "VALUES"},
		{"UPDATE", TokenUPDATE, "UPDATE"},
		{"SET", TokenSET, "SET"},
		{"DELETE", TokenDELETE, "DELETE"},
		{"CREATE", TokenCREATE, "CREATE"},
		{"DROP", TokenDROP, "DROP"},
		{"TABLE", TokenTABLE, "TABLE"},
		{"PRIMARY", TokenPRIMARY, "PRIMARY"},
		{"KEY", TokenKEY, "KEY"},
		{"NULL", TokenNULL, "NULL"},
		{"TRUE", TokenTRUE, "TRUE"},
		{"FALSE", TokenFALSE, "FALSE"},
		{"INTEGER", TokenINTEGER, "INTEGER"},
		{"TEXT", TokenTEXT, "TEXT"},
		{"BLOB", TokenBLOB, "BLOB"},
		{"REAL", TokenREAL, "REAL"},

		// Identifiers
		{"foo", TokenIdent, "foo"},
		{"_bar", TokenIdent, "_bar"},
		{"table123", TokenIdent, "table123"},
		{"CamelCase", TokenIdent, "CamelCase"},

		// Numbers
		{"42", TokenNumber, "42"},
		{"3.14", TokenNumber, "3.14"},
		{"1e10", TokenNumber, "1e10"},
		{"2.5e-3", TokenNumber, "2.5e-3"},
		{"0", TokenNumber, "0"},

		// Strings
		{"'hello'", TokenString, "hello"},
		{"'it''s escaped'", TokenString, "it's escaped"},
		{"''", TokenString, ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			l := New(tt.input)
			tok := l.NextToken()

			if tok.Type != tt.expected {
				t.Errorf("expected type %v, got %v", tt.expected, tok.Type)
			}
			if tok.Literal != tt.literal {
				t.Errorf("expected literal %q, got %q", tt.literal, tok.Literal)
			}
		})
	}
}

func TestLexerQuotedIdentifiers(t *testing.T) {
	tests := []struct {
		input   string
		literal string
	}{
		{`"column name"`, "column name"},
		{"`backtick`", "backtick"},
		{"[bracket]", "bracket"},
		{`"with spaces"`, "with spaces"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			l := New(tt.input)
			tok := l.NextToken()

			if tok.Type != TokenIdent {
				t.Errorf("expected TokenIdent, got %v", tok.Type)
			}
			if tok.Literal != tt.literal {
				t.Errorf("expected literal %q, got %q", tt.literal, tok.Literal)
			}
		})
	}
}

func TestLexerSelectStatement(t *testing.T) {
	input := "SELECT * FROM users WHERE id = 1;"

	expected := []struct {
		typ     TokenType
		literal string
	}{
		{TokenSELECT, "SELECT"},
		{TokenStar, "*"},
		{TokenFROM, "FROM"},
		{TokenIdent, "users"},
		{TokenWHERE, "WHERE"},
		{TokenIdent, "id"},
		{TokenEq, "="},
		{TokenNumber, "1"},
		{TokenSemicolon, ";"},
		{TokenEOF, ""},
	}

	l := New(input)
	for i, exp := range expected {
		tok := l.NextToken()
		if tok.Type != exp.typ {
			t.Errorf("token[%d]: expected type %v, got %v", i, exp.typ, tok.Type)
		}
		if tok.Literal != exp.literal {
			t.Errorf("token[%d]: expected literal %q, got %q", i, exp.literal, tok.Literal)
		}
	}
}

func TestLexerInsertStatement(t *testing.T) {
	input := "INSERT INTO users (name, age) VALUES ('John', 30);"

	expected := []struct {
		typ     TokenType
		literal string
	}{
		{TokenINSERT, "INSERT"},
		{TokenINTO, "INTO"},
		{TokenIdent, "users"},
		{TokenLParen, "("},
		{TokenIdent, "name"},
		{TokenComma, ","},
		{TokenIdent, "age"},
		{TokenRParen, ")"},
		{TokenVALUES, "VALUES"},
		{TokenLParen, "("},
		{TokenString, "John"},
		{TokenComma, ","},
		{TokenNumber, "30"},
		{TokenRParen, ")"},
		{TokenSemicolon, ";"},
		{TokenEOF, ""},
	}

	l := New(input)
	for i, exp := range expected {
		tok := l.NextToken()
		if tok.Type != exp.typ {
			t.Errorf("token[%d]: expected type %v, got %v", i, exp.typ, tok.Type)
		}
		if tok.Literal != exp.literal {
			t.Errorf("token[%d]: expected literal %q, got %q", i, exp.literal, tok.Literal)
		}
	}
}

func TestLexerCreateTable(t *testing.T) {
	input := `CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		email VARCHAR(255) UNIQUE
	);`

	l := New(input)
	tokens := l.Tokenize()

	// Just verify we got all expected token types
	expectedTypes := []TokenType{
		TokenCREATE, TokenTABLE, TokenIdent, TokenLParen,
		TokenIdent, TokenINTEGER, TokenPRIMARY, TokenKEY, TokenComma,
		TokenIdent, TokenTEXT, TokenNOT, TokenNULL, TokenComma,
		TokenIdent, TokenVARCHAR, TokenLParen, TokenNumber, TokenRParen, TokenUNIQUE,
		TokenRParen, TokenSemicolon, TokenEOF,
	}

	if len(tokens) != len(expectedTypes) {
		t.Errorf("expected %d tokens, got %d", len(expectedTypes), len(tokens))
		for i, tok := range tokens {
			t.Logf("token[%d]: %v", i, tok)
		}
		return
	}

	for i, exp := range expectedTypes {
		if tokens[i].Type != exp {
			t.Errorf("token[%d]: expected %v, got %v (%q)", i, exp, tokens[i].Type, tokens[i].Literal)
		}
	}
}

func TestLexerComments(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []TokenType
	}{
		{
			name:     "line comment",
			input:    "SELECT -- comment\n* FROM t",
			expected: []TokenType{TokenSELECT, TokenComment, TokenStar, TokenFROM, TokenIdent, TokenEOF},
		},
		{
			name:     "block comment",
			input:    "SELECT /* comment */ * FROM t",
			expected: []TokenType{TokenSELECT, TokenStar, TokenFROM, TokenIdent, TokenEOF},
		},
		{
			name:     "multiline block comment",
			input:    "SELECT /* line1\nline2 */ * FROM t",
			expected: []TokenType{TokenSELECT, TokenStar, TokenFROM, TokenIdent, TokenEOF},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := New(tt.input)
			tokens := l.Tokenize()

			if len(tokens) != len(tt.expected) {
				t.Errorf("expected %d tokens, got %d", len(tt.expected), len(tokens))
				for i, tok := range tokens {
					t.Logf("token[%d]: %v", i, tok)
				}
				return
			}

			for i, exp := range tt.expected {
				if tokens[i].Type != exp {
					t.Errorf("token[%d]: expected %v, got %v", i, exp, tokens[i].Type)
				}
			}
		})
	}
}

func TestLexerLineTracking(t *testing.T) {
	input := "SELECT\n*\nFROM t"

	l := New(input)

	// SELECT on line 1
	tok := l.NextToken()
	if tok.Line != 1 {
		t.Errorf("SELECT: expected line 1, got %d", tok.Line)
	}

	// * on line 2
	tok = l.NextToken()
	if tok.Line != 2 {
		t.Errorf("*: expected line 2, got %d", tok.Line)
	}

	// FROM on line 3
	tok = l.NextToken()
	if tok.Line != 3 {
		t.Errorf("FROM: expected line 3, got %d", tok.Line)
	}
}

func TestLexerErrors(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		errMsg string
	}{
		{
			name:   "unterminated string",
			input:  "'hello",
			errMsg: "unterminated string",
		},
		{
			name:   "unterminated quoted identifier",
			input:  `"hello`,
			errMsg: "unterminated identifier",
		},
		{
			name:   "unexpected character",
			input:  "@",
			errMsg: "unexpected character: @",
		},
		{
			name:   "single pipe",
			input:  "|",
			errMsg: "unexpected character: |",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := New(tt.input)
			tok := l.NextToken()

			if tok.Type != TokenError {
				t.Errorf("expected TokenError, got %v", tok.Type)
			}
			if tok.Literal != tt.errMsg {
				t.Errorf("expected error %q, got %q", tt.errMsg, tok.Literal)
			}
		})
	}
}

func TestLexerComplexExpressions(t *testing.T) {
	input := "WHERE a = 1 AND b > 2 OR c <= 3 AND NOT d <> 4"

	expected := []struct {
		typ     TokenType
		literal string
	}{
		{TokenWHERE, "WHERE"},
		{TokenIdent, "a"},
		{TokenEq, "="},
		{TokenNumber, "1"},
		{TokenAND, "AND"},
		{TokenIdent, "b"},
		{TokenGt, ">"},
		{TokenNumber, "2"},
		{TokenOR, "OR"},
		{TokenIdent, "c"},
		{TokenLte, "<="},
		{TokenNumber, "3"},
		{TokenAND, "AND"},
		{TokenNOT, "NOT"},
		{TokenIdent, "d"},
		{TokenNeq, "<>"},
		{TokenNumber, "4"},
		{TokenEOF, ""},
	}

	l := New(input)
	for i, exp := range expected {
		tok := l.NextToken()
		if tok.Type != exp.typ {
			t.Errorf("token[%d]: expected type %v, got %v", i, exp.typ, tok.Type)
		}
		if tok.Literal != exp.literal {
			t.Errorf("token[%d]: expected literal %q, got %q", i, exp.literal, tok.Literal)
		}
	}
}

func TestLexerJoinKeywords(t *testing.T) {
	input := "LEFT OUTER JOIN t ON a.id = b.id"

	expected := []TokenType{
		TokenLEFT, TokenOUTER, TokenJOIN, TokenIdent,
		TokenON, TokenIdent, TokenDot, TokenIdent,
		TokenEq, TokenIdent, TokenDot, TokenIdent,
		TokenEOF,
	}

	l := New(input)
	tokens := l.Tokenize()

	if len(tokens) != len(expected) {
		t.Errorf("expected %d tokens, got %d", len(expected), len(tokens))
		return
	}

	for i, exp := range expected {
		if tokens[i].Type != exp {
			t.Errorf("token[%d]: expected %v, got %v", i, exp, tokens[i].Type)
		}
	}
}

func TestLexerCaseExpression(t *testing.T) {
	input := "CASE WHEN x = 1 THEN 'one' ELSE 'other' END"

	expected := []TokenType{
		TokenCASE, TokenWHEN, TokenIdent, TokenEq, TokenNumber,
		TokenTHEN, TokenString, TokenELSE, TokenString, TokenEND,
		TokenEOF,
	}

	l := New(input)
	tokens := l.Tokenize()

	if len(tokens) != len(expected) {
		t.Errorf("expected %d tokens, got %d", len(expected), len(tokens))
		return
	}

	for i, exp := range expected {
		if tokens[i].Type != exp {
			t.Errorf("token[%d]: expected %v, got %v", i, exp, tokens[i].Type)
		}
	}
}

func TestLexerSubquery(t *testing.T) {
	input := "WHERE id IN (SELECT user_id FROM orders)"

	expected := []TokenType{
		TokenWHERE, TokenIdent, TokenIN, TokenLParen,
		TokenSELECT, TokenIdent, TokenFROM, TokenIdent,
		TokenRParen, TokenEOF,
	}

	l := New(input)
	tokens := l.Tokenize()

	if len(tokens) != len(expected) {
		t.Errorf("expected %d tokens, got %d", len(expected), len(tokens))
		return
	}

	for i, exp := range expected {
		if tokens[i].Type != exp {
			t.Errorf("token[%d]: expected %v, got %v", i, exp, tokens[i].Type)
		}
	}
}

func BenchmarkLexer(b *testing.B) {
	input := `
		SELECT u.id, u.name, u.email, COUNT(o.id) as order_count
		FROM users u
		LEFT JOIN orders o ON u.id = o.user_id
		WHERE u.active = TRUE AND u.created_at >= '2024-01-01'
		GROUP BY u.id, u.name, u.email
		HAVING COUNT(o.id) > 5
		ORDER BY order_count DESC
		LIMIT 100 OFFSET 0;
	`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := New(input)
		for {
			tok := l.NextToken()
			if tok.Type == TokenEOF {
				break
			}
		}
	}
}
