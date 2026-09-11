package lexer

import "fmt"

type TokenType int

const (
	// Special tokens
	TokenEOF TokenType = iota
	TokenError
	TokenComment

	// Literals
	TokenIdent  // identifiers
	TokenNumber // integers and floats
	TokenString // 'string literals'
	TokenBlob   // X'hex' blob literals

	// Operators
	TokenPlus       // +
	TokenMinus      // -
	TokenStar       // *
	TokenSlash      // /
	TokenPercent    // %
	TokenConcat     // ||
	TokenEq         // =
	TokenNeq        // <> or !=
	TokenLt         // <
	TokenLte        // <=
	TokenGt         // >
	TokenGte        // >=
	TokenBitAnd     // &
	TokenBitOr      // |
	TokenBitNot     // ~
	TokenShiftLeft  // <<
	TokenShiftRight // >>

	// Punctuation
	TokenLParen    // (
	TokenRParen    // )
	TokenComma     // ,
	TokenSemicolon // ;
	TokenDot       // .

	// SQL Keywords - DML
	TokenSELECT
	TokenFROM
	TokenWHERE
	TokenAND
	TokenOR
	TokenNOT
	TokenAS
	TokenDISTINCT
	TokenALL
	TokenRETURNING

	TokenINSERT
	TokenINTO
	TokenVALUES

	TokenUPDATE
	TokenSET

	TokenDELETE

	// SQL Keywords - DDL
	TokenCREATE
	TokenDROP
	TokenALTER
	TokenTABLE
	TokenINDEX
	TokenVIEW
	TokenDATABASE
	TokenSCHEMA
	TokenADD
	TokenCOLUMN
	TokenRENAME
	TokenTO

	// SQL Keywords - Constraints
	TokenPRIMARY
	TokenKEY
	TokenFOREIGN
	TokenREFERENCES
	TokenUNIQUE
	TokenCHECK
	TokenCONSTRAINT
	TokenDEFAULT
	TokenAUTOINCREMENT

	// SQL Keywords - Clauses
	TokenORDER
	TokenBY
	TokenASC
	TokenDESC
	TokenLIMIT
	TokenOFFSET
	TokenGROUP
	TokenHAVING

	// SQL Keywords - Joins
	TokenJOIN
	TokenINNER
	TokenLEFT
	TokenRIGHT
	TokenFULL
	TokenOUTER
	TokenCROSS
	TokenNATURAL
	TokenON
	TokenUSING

	// SQL Keywords - Set operations
	TokenUNION
	TokenINTERSECT
	TokenEXCEPT

	// SQL Keywords - Predicates
	TokenIN
	TokenBETWEEN
	TokenLIKE
	TokenGLOB
	TokenESCAPE
	TokenIS
	TokenNULL
	TokenEXISTS

	// SQL Keywords - CASE
	TokenCASE
	TokenWHEN
	TokenTHEN
	TokenELSE
	TokenEND

	// SQL Keywords - Other
	TokenCAST
	TokenCOALESCE
	TokenNULLIF
	TokenIF

	// Boolean literals
	TokenTRUE
	TokenFALSE

	// Data types
	TokenINTEGER
	TokenINT
	TokenTINYINT
	TokenSMALLINT
	TokenMEDIUMINT
	TokenBIGINT
	TokenREAL
	TokenFLOAT
	TokenDOUBLE
	TokenNUMERIC
	TokenDECIMAL
	TokenTEXT
	TokenVARCHAR
	TokenCHAR
	TokenCHARACTER
	TokenCLOB
	TokenNCHAR
	TokenNVARCHAR
	TokenBLOB
	TokenBOOLEAN
	TokenDATE
	TokenTIME
	TokenTIMESTAMP
	TokenDATETIME
	TokenJSON
	TokenJSONB

	// Transaction keywords
	TokenBEGIN
	TokenCOMMIT
	TokenROLLBACK
	TokenTRANSACTION
	TokenSAVEPOINT
	TokenRELEASE

	// SQLite specific
	TokenPRAGMA
	TokenEXPLAIN
	TokenQUERY
	TokenPLAN
	TokenATTACH
	TokenDETACH
	TokenVACUUM
	TokenANALYZE
	TokenREINDEX

	// Conflict resolution
	TokenREPLACE
	TokenIGNORE
	TokenFAIL
	TokenABORT
	TokenCONFLICT
	TokenDO
	TokenNOTHING
)

var keywords = map[string]TokenType{
	// DML
	"SELECT":   TokenSELECT,
	"FROM":     TokenFROM,
	"WHERE":    TokenWHERE,
	"AND":      TokenAND,
	"OR":       TokenOR,
	"NOT":      TokenNOT,
	"AS":       TokenAS,
	"DISTINCT": TokenDISTINCT,
	"ALL":      TokenALL,
	// RETURNING is a keyword so it is not mistaken for a table/column alias in
	// INSERT ... SELECT ... RETURNING.
	"RETURNING": TokenRETURNING,
	"INSERT":    TokenINSERT,
	"INTO":      TokenINTO,
	"VALUES":    TokenVALUES,
	"UPDATE":    TokenUPDATE,
	"SET":       TokenSET,
	"DELETE":    TokenDELETE,

	// DDL
	"CREATE":   TokenCREATE,
	"DROP":     TokenDROP,
	"ALTER":    TokenALTER,
	"TABLE":    TokenTABLE,
	"INDEX":    TokenINDEX,
	"VIEW":     TokenVIEW,
	"DATABASE": TokenDATABASE,
	"SCHEMA":   TokenSCHEMA,
	"ADD":      TokenADD,
	"COLUMN":   TokenCOLUMN,
	"RENAME":   TokenRENAME,
	"TO":       TokenTO,

	// Constraints
	"PRIMARY":       TokenPRIMARY,
	"KEY":           TokenKEY,
	"FOREIGN":       TokenFOREIGN,
	"REFERENCES":    TokenREFERENCES,
	"UNIQUE":        TokenUNIQUE,
	"CHECK":         TokenCHECK,
	"CONSTRAINT":    TokenCONSTRAINT,
	"DEFAULT":       TokenDEFAULT,
	"AUTOINCREMENT": TokenAUTOINCREMENT,

	// Clauses
	"ORDER":  TokenORDER,
	"BY":     TokenBY,
	"ASC":    TokenASC,
	"DESC":   TokenDESC,
	"LIMIT":  TokenLIMIT,
	"OFFSET": TokenOFFSET,
	"GROUP":  TokenGROUP,
	"HAVING": TokenHAVING,

	// Joins
	"JOIN":    TokenJOIN,
	"INNER":   TokenINNER,
	"LEFT":    TokenLEFT,
	"RIGHT":   TokenRIGHT,
	"FULL":    TokenFULL,
	"OUTER":   TokenOUTER,
	"CROSS":   TokenCROSS,
	"NATURAL": TokenNATURAL,
	"ON":      TokenON,
	"USING":   TokenUSING,

	// Set operations
	"UNION":     TokenUNION,
	"INTERSECT": TokenINTERSECT,
	"EXCEPT":    TokenEXCEPT,

	// Predicates
	"IN":      TokenIN,
	"BETWEEN": TokenBETWEEN,
	"LIKE":    TokenLIKE,
	"GLOB":    TokenGLOB,
	"ESCAPE":  TokenESCAPE,
	"IS":      TokenIS,
	"NULL":    TokenNULL,
	"EXISTS":  TokenEXISTS,

	// CASE
	"CASE": TokenCASE,
	"WHEN": TokenWHEN,
	"THEN": TokenTHEN,
	"ELSE": TokenELSE,
	"END":  TokenEND,

	// Other
	"CAST":     TokenCAST,
	"COALESCE": TokenCOALESCE,
	"NULLIF":   TokenNULLIF,
	"IF":       TokenIF,

	// Boolean
	"TRUE":  TokenTRUE,
	"FALSE": TokenFALSE,

	// Data types
	"INTEGER":   TokenINTEGER,
	"INT":       TokenINT,
	"TINYINT":   TokenTINYINT,
	"SMALLINT":  TokenSMALLINT,
	"MEDIUMINT": TokenMEDIUMINT,
	"BIGINT":    TokenBIGINT,
	"REAL":      TokenREAL,
	"FLOAT":     TokenFLOAT,
	"DOUBLE":    TokenDOUBLE,
	"NUMERIC":   TokenNUMERIC,
	"DECIMAL":   TokenDECIMAL,
	"TEXT":      TokenTEXT,
	"VARCHAR":   TokenVARCHAR,
	"CHAR":      TokenCHAR,
	"CHARACTER": TokenCHARACTER,
	"CLOB":      TokenCLOB,
	"NCHAR":     TokenNCHAR,
	"NVARCHAR":  TokenNVARCHAR,
	"BLOB":      TokenBLOB,
	"BOOLEAN":   TokenBOOLEAN,
	"DATE":      TokenDATE,
	"TIME":      TokenTIME,
	"TIMESTAMP": TokenTIMESTAMP,
	"DATETIME":  TokenDATETIME,
	"JSON":      TokenJSON,
	"JSONB":     TokenJSONB,

	// Transactions
	"BEGIN":       TokenBEGIN,
	"COMMIT":      TokenCOMMIT,
	"ROLLBACK":    TokenROLLBACK,
	"TRANSACTION": TokenTRANSACTION,
	"SAVEPOINT":   TokenSAVEPOINT,
	"RELEASE":     TokenRELEASE,

	// SQLite specific
	"PRAGMA":  TokenPRAGMA,
	"EXPLAIN": TokenEXPLAIN,
	"QUERY":   TokenQUERY,
	"ATTACH":  TokenATTACH,
	"DETACH":  TokenDETACH,
	"VACUUM":  TokenVACUUM,
	"ANALYZE": TokenANALYZE,
	"REINDEX": TokenREINDEX,

	// Conflict resolution
	"REPLACE":  TokenREPLACE,
	"IGNORE":   TokenIGNORE,
	"FAIL":     TokenFAIL,
	"ABORT":    TokenABORT,
	"CONFLICT": TokenCONFLICT,
	"DO":       TokenDO,
	"NOTHING":  TokenNOTHING,
}

// LookupKeyword returns the token type for an identifier.
// If the identifier is a keyword, returns the keyword token type.
// Otherwise, returns TokenIdent.
func LookupKeyword(ident string) TokenType {
	if tok, ok := keywords[ident]; ok {
		return tok
	}
	return TokenIdent
}

// Token represents a lexical token.
type Token struct {
	Type    TokenType
	Literal string
	Line    int
	Column  int
}

func (t Token) String() string {
	return fmt.Sprintf("Token{Type: %v, Literal: %q, Line: %d, Col: %d}",
		t.Type, t.Literal, t.Line, t.Column)
}

// IsKeyword returns true if the token is a SQL keyword.
func (t Token) IsKeyword() bool {
	return t.Type >= TokenSELECT
}

// IsOperator returns true if the token is an operator.
func (t Token) IsOperator() bool {
	return t.Type >= TokenPlus && t.Type <= TokenGte
}

var tokenNames = map[TokenType]string{
	TokenEOF:        "EOF",
	TokenError:      "ERROR",
	TokenComment:    "COMMENT",
	TokenIdent:      "IDENT",
	TokenNumber:     "NUMBER",
	TokenString:     "STRING",
	TokenBlob:       "BLOB",
	TokenPlus:       "+",
	TokenMinus:      "-",
	TokenStar:       "*",
	TokenSlash:      "/",
	TokenPercent:    "%",
	TokenConcat:     "||",
	TokenEq:         "=",
	TokenNeq:        "<>",
	TokenLt:         "<",
	TokenLte:        "<=",
	TokenGt:         ">",
	TokenGte:        ">=",
	TokenBitAnd:     "&",
	TokenBitOr:      "|",
	TokenBitNot:     "~",
	TokenShiftLeft:  "<<",
	TokenShiftRight: ">>",
	TokenLParen:     "(",
	TokenRParen:     ")",
	TokenComma:      ",",
	TokenSemicolon:  ";",
	TokenDot:        ".",
}

func (t TokenType) String() string {
	if name, ok := tokenNames[t]; ok {
		return name
	}
	// For keywords, look up in reverse
	for kw, tok := range keywords {
		if tok == t {
			return kw
		}
	}
	return fmt.Sprintf("TOKEN(%d)", t)
}
