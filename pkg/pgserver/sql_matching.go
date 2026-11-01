package pgserver

import (
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
)

func nextSQLToken(l *lexer.Lexer) lexer.Token {
	for {
		token := l.NextToken()
		if token.Type != lexer.TokenComment {
			return token
		}
	}
}

// Match complete token sequences, never substrings inside literals/comments or
// projections that happen to mention a driver initialization query.
func systemQueryKind(sql string) string {
	l := lexer.New(sql)
	first := nextSQLToken(l)
	if first.Type != lexer.TokenSELECT && !(first.Type == lexer.TokenIdent && strings.EqualFold(first.Literal, "SHOW")) {
		return ""
	}
	tokens := []string{strings.ToUpper(first.Literal)}
	for len(tokens) <= 4 {
		token := nextSQLToken(l)
		if token.Type == lexer.TokenSemicolon {
			if nextSQLToken(l).Type != lexer.TokenEOF {
				return ""
			}
			break
		}
		if token.Type == lexer.TokenEOF {
			break
		}
		if token.Type == lexer.TokenString || token.Type == lexer.TokenBlob || token.Type == lexer.TokenError {
			return ""
		}
		tokens = append(tokens, strings.ToUpper(token.Literal))
	}
	switch strings.Join(tokens, " ") {
	case "SELECT VERSION ( )":
		return "version"
	case "SELECT CURRENT_USER":
		return "current_user"
	case "SHOW SERVER_VERSION":
		return "server_version"
	case "SHOW SERVER_ENCODING":
		return "server_encoding"
	case "SHOW CLIENT_ENCODING":
		return "client_encoding"
	default:
		return ""
	}
}

func isSingleStatementSQL(sql string) bool {
	l := lexer.New(sql)
	ended := false
	for {
		token := nextSQLToken(l)
		if token.Type == lexer.TokenEOF {
			return true
		}
		if token.Type == lexer.TokenSemicolon {
			ended = true
			continue
		}
		if ended {
			return false
		}
	}
}

func catalogSource(sql string) string {
	l := lexer.New(sql)
	if nextSQLToken(l).Type != lexer.TokenSELECT {
		return ""
	}
	depth := 0
	for {
		token := nextSQLToken(l)
		switch token.Type {
		case lexer.TokenEOF, lexer.TokenError:
			return ""
		case lexer.TokenLParen:
			depth++
		case lexer.TokenRParen:
			depth--
		case lexer.TokenFROM:
			if depth != 0 {
				continue
			}
			name := nextSQLToken(l)
			if name.Type != lexer.TokenIdent {
				return ""
			}
			source := strings.ToUpper(name.Literal)
			if nextSQLToken(l).Type == lexer.TokenDot {
				name = nextSQLToken(l)
				if name.Type != lexer.TokenIdent {
					return ""
				}
				source += "." + strings.ToUpper(name.Literal)
			}
			switch source {
			case "INFORMATION_SCHEMA.TABLES", "INFORMATION_SCHEMA.COLUMNS", "INFORMATION_SCHEMA.TABLE_CONSTRAINTS", "INFORMATION_SCHEMA.KEY_COLUMN_USAGE", "PG_TABLES", "PG_INDEXES":
				return source
			default:
				return ""
			}
		}
	}
}
