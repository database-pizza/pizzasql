package lexer

import (
	"encoding/hex"
	"strings"
	"unicode"
)

// Lexer tokenizes SQL input.
type Lexer struct {
	input   string
	pos     int  // current position in input
	readPos int  // reading position (after current char)
	ch      byte // current char under examination
	line    int  // current line number (1-based)
	column  int  // current column number (1-based)
}

// New creates a new Lexer for the given input.
func New(input string) *Lexer {
	l := &Lexer{
		input:  input,
		line:   1,
		column: 0,
	}
	l.readChar()
	return l
}

// readChar advances the lexer by one character.
func (l *Lexer) readChar() {
	if l.readPos >= len(l.input) {
		l.ch = 0
	} else {
		l.ch = l.input[l.readPos]
	}
	l.pos = l.readPos
	l.readPos++
	l.column++

	if l.ch == '\n' {
		l.line++
		l.column = 0
	}
}

// peekChar returns the next character without advancing.
func (l *Lexer) peekChar() byte {
	if l.readPos >= len(l.input) {
		return 0
	}
	return l.input[l.readPos]
}

// NextToken returns the next token from the input.
func (l *Lexer) NextToken() Token {
	l.skipWhitespace()

	tok := Token{
		Line:   l.line,
		Column: l.column,
	}

	switch l.ch {
	case 0:
		tok.Type = TokenEOF
		tok.Literal = ""
	case '+':
		tok.Type = TokenPlus
		tok.Literal = "+"
		l.readChar()
	case '*':
		tok.Type = TokenStar
		tok.Literal = "*"
		l.readChar()
	case '/':
		tok.Type = TokenSlash
		tok.Literal = "/"
		l.readChar()
	case '%':
		tok.Type = TokenPercent
		tok.Literal = "%"
		l.readChar()
	case '&':
		tok.Type = TokenBitAnd
		tok.Literal = "&"
		l.readChar()
	case '~':
		tok.Type = TokenBitNot
		tok.Literal = "~"
		l.readChar()
	case '(':
		tok.Type = TokenLParen
		tok.Literal = "("
		l.readChar()
	case ')':
		tok.Type = TokenRParen
		tok.Literal = ")"
		l.readChar()
	case ',':
		tok.Type = TokenComma
		tok.Literal = ","
		l.readChar()
	case ';':
		tok.Type = TokenSemicolon
		tok.Literal = ";"
		l.readChar()
	case '.':
		tok.Type = TokenDot
		tok.Literal = "."
		l.readChar()
	case '=':
		tok.Type = TokenEq
		tok.Literal = "="
		l.readChar()
	case '<':
		if l.peekChar() == '=' {
			l.readChar()
			tok.Type = TokenLte
			tok.Literal = "<="
		} else if l.peekChar() == '>' {
			l.readChar()
			tok.Type = TokenNeq
			tok.Literal = "<>"
		} else if l.peekChar() == '<' {
			l.readChar()
			tok.Type = TokenShiftLeft
			tok.Literal = "<<"
		} else {
			tok.Type = TokenLt
			tok.Literal = "<"
		}
		l.readChar()
	case '>':
		if l.peekChar() == '=' {
			l.readChar()
			tok.Type = TokenGte
			tok.Literal = ">="
		} else if l.peekChar() == '>' {
			l.readChar()
			tok.Type = TokenShiftRight
			tok.Literal = ">>"
		} else {
			tok.Type = TokenGt
			tok.Literal = ">"
		}
		l.readChar()
	case '!':
		if l.peekChar() == '=' {
			l.readChar()
			tok.Type = TokenNeq
			tok.Literal = "!="
			l.readChar()
		} else {
			tok.Type = TokenError
			tok.Literal = "unexpected character: !"
			l.readChar()
		}
	case '|':
		if l.peekChar() == '|' {
			l.readChar()
			tok.Type = TokenConcat
			tok.Literal = "||"
			l.readChar()
		} else {
			tok.Type = TokenBitOr
			tok.Literal = "|"
			l.readChar()
		}
	case '-':
		if l.peekChar() == '-' {
			// Line comment
			tok = l.readLineComment()
		} else {
			tok.Type = TokenMinus
			tok.Literal = "-"
			l.readChar()
		}
	case '\'':
		tok = l.readString()
	case '"':
		tok = l.readQuotedIdentifier('"')
	case '`':
		tok = l.readQuotedIdentifier('`')
	case '[':
		tok = l.readBracketIdentifier()
	default:
		if (l.ch == 'x' || l.ch == 'X') && l.peekChar() == '\'' {
			tok = l.readBlob()
		} else if isLetter(l.ch) || l.ch == '_' {
			tok = l.readIdentifier()
		} else if isDigit(l.ch) {
			tok = l.readNumber()
		} else {
			tok.Type = TokenError
			tok.Literal = "unexpected character: " + string(l.ch)
			l.readChar()
		}
	}

	return tok
}

// skipWhitespace skips spaces, tabs, and newlines.
func (l *Lexer) skipWhitespace() {
	for l.ch == ' ' || l.ch == '\t' || l.ch == '\n' || l.ch == '\r' {
		l.readChar()
	}

	// Also skip block comments
	if l.ch == '/' && l.peekChar() == '*' {
		l.skipBlockComment()
		l.skipWhitespace()
	}
}

// skipBlockComment skips /* ... */ comments.
func (l *Lexer) skipBlockComment() {
	l.readChar() // skip /
	l.readChar() // skip *

	for {
		if l.ch == 0 {
			return // EOF in comment
		}
		if l.ch == '*' && l.peekChar() == '/' {
			l.readChar() // skip *
			l.readChar() // skip /
			return
		}
		l.readChar()
	}
}

// readLineComment reads a -- line comment.
func (l *Lexer) readLineComment() Token {
	tok := Token{
		Type:   TokenComment,
		Line:   l.line,
		Column: l.column,
	}

	startPos := l.pos
	for l.ch != '\n' && l.ch != 0 {
		l.readChar()
	}
	tok.Literal = l.input[startPos:l.pos]

	return tok
}

// readString reads a 'string literal'.
func (l *Lexer) readString() Token {
	tok := Token{
		Type:   TokenString,
		Line:   l.line,
		Column: l.column,
	}

	l.readChar() // skip opening quote
	var sb strings.Builder

	for {
		if l.ch == 0 {
			tok.Type = TokenError
			tok.Literal = "unterminated string"
			return tok
		}
		if l.ch == '\'' {
			if l.peekChar() == '\'' {
				// Escaped quote
				sb.WriteByte('\'')
				l.readChar()
				l.readChar()
			} else {
				// End of string
				l.readChar()
				break
			}
		} else {
			sb.WriteByte(l.ch)
			l.readChar()
		}
	}

	tok.Literal = sb.String()
	return tok
}

// readQuotedIdentifier reads a "quoted identifier" or `backtick identifier`.
func (l *Lexer) readQuotedIdentifier(quote byte) Token {
	tok := Token{
		Type:   TokenIdent,
		Line:   l.line,
		Column: l.column,
	}

	l.readChar() // skip opening quote
	startPos := l.pos

	for l.ch != quote && l.ch != 0 {
		l.readChar()
	}

	if l.ch == 0 {
		tok.Type = TokenError
		tok.Literal = "unterminated identifier"
		return tok
	}

	tok.Literal = l.input[startPos:l.pos]
	l.readChar() // skip closing quote
	return tok
}

// readBracketIdentifier reads a [bracket identifier] (SQL Server style).
func (l *Lexer) readBracketIdentifier() Token {
	tok := Token{
		Type:   TokenIdent,
		Line:   l.line,
		Column: l.column,
	}

	l.readChar() // skip [
	startPos := l.pos

	for l.ch != ']' && l.ch != 0 {
		l.readChar()
	}

	if l.ch == 0 {
		tok.Type = TokenError
		tok.Literal = "unterminated identifier"
		return tok
	}

	tok.Literal = l.input[startPos:l.pos]
	l.readChar() // skip ]
	return tok
}

// readBlob reads a X'hex' blob literal. Whitespace between hex digits is
// ignored (SQLite allows it). The token literal holds the decoded raw bytes so
// consumers never have to re-parse the hex form.
func (l *Lexer) readBlob() Token {
	tok := Token{
		Type:   TokenBlob,
		Line:   l.line,
		Column: l.column,
	}

	l.readChar() // skip x/X
	l.readChar() // skip opening quote

	var sb strings.Builder
	for l.ch != '\'' && l.ch != 0 {
		if isHexDigit(l.ch) {
			sb.WriteByte(l.ch)
		} else if l.ch != ' ' && l.ch != '\t' && l.ch != '\n' && l.ch != '\r' {
			tok.Type = TokenError
			tok.Literal = "invalid character in blob literal: " + string(l.ch)
			return tok
		}
		l.readChar()
	}

	if l.ch == 0 {
		tok.Type = TokenError
		tok.Literal = "unterminated blob literal"
		return tok
	}
	l.readChar() // skip closing quote

	if sb.Len()%2 != 0 {
		tok.Type = TokenError
		tok.Literal = "blob literal must contain an even number of hex digits"
		return tok
	}

	decoded, err := hex.DecodeString(sb.String())
	if err != nil {
		tok.Type = TokenError
		tok.Literal = "invalid blob literal: " + err.Error()
		return tok
	}

	tok.Literal = string(decoded)
	return tok
}

// readIdentifier reads an identifier or keyword.
func (l *Lexer) readIdentifier() Token {
	tok := Token{
		Line:   l.line,
		Column: l.column,
	}

	startPos := l.pos
	for isLetter(l.ch) || isDigit(l.ch) || l.ch == '_' {
		l.readChar()
	}

	literal := l.input[startPos:l.pos]
	tok.Literal = literal
	tok.Type = LookupKeyword(strings.ToUpper(literal))

	return tok
}

// readNumber reads an integer or float.
func (l *Lexer) readNumber() Token {
	tok := Token{
		Type:   TokenNumber,
		Line:   l.line,
		Column: l.column,
	}

	startPos := l.pos

	// Read integer part
	for isDigit(l.ch) {
		l.readChar()
	}

	// Check for decimal point
	if l.ch == '.' && isDigit(l.peekChar()) {
		l.readChar() // skip .
		for isDigit(l.ch) {
			l.readChar()
		}
	}

	// Check for exponent
	if l.ch == 'e' || l.ch == 'E' {
		l.readChar()
		if l.ch == '+' || l.ch == '-' {
			l.readChar()
		}
		for isDigit(l.ch) {
			l.readChar()
		}
	}

	tok.Literal = l.input[startPos:l.pos]
	return tok
}

// Tokenize returns all tokens from the input.
func (l *Lexer) Tokenize() []Token {
	var tokens []Token
	for {
		tok := l.NextToken()
		tokens = append(tokens, tok)
		if tok.Type == TokenEOF || tok.Type == TokenError {
			break
		}
	}
	return tokens
}

func isLetter(ch byte) bool {
	return unicode.IsLetter(rune(ch))
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

func isHexDigit(ch byte) bool {
	return (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}
