package parser

import (
	"fmt"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
)

// FormatExpr renders an expression as canonical SQL text. It is used to persist
// expression indexes and generated-column definitions in the durable schema, so
// the output only needs to be stable and re-parseable, not byte-identical to the
// original input. A nil expression formats as an empty string.
func FormatExpr(expr Expr) string {
	if expr == nil {
		return ""
	}
	switch e := expr.(type) {
	case *LiteralExpr:
		switch e.Type {
		case lexer.TokenString:
			return "'" + strings.ReplaceAll(e.Value, "'", "''") + "'"
		case lexer.TokenBlob:
			return "X'" + fmt.Sprintf("%X", []byte(e.Value)) + "'"
		case lexer.TokenNULL:
			return "NULL"
		case lexer.TokenTRUE:
			return "TRUE"
		case lexer.TokenFALSE:
			return "FALSE"
		default:
			return e.Value
		}
	case *ColumnRef:
		if e.Table != "" {
			return quoteFormatIdent(e.Table) + "." + quoteFormatIdent(e.Column)
		}
		return quoteFormatIdent(e.Column)
	case *BinaryExpr:
		return fmt.Sprintf("(%s %s %s)", FormatExpr(e.Left), operatorString(e.Op), FormatExpr(e.Right))
	case *UnaryExpr:
		if e.Op == lexer.TokenNOT {
			return fmt.Sprintf("(NOT %s)", FormatExpr(e.Operand))
		}
		return fmt.Sprintf("(%s%s)", operatorString(e.Op), FormatExpr(e.Operand))
	case *ParenExpr:
		return fmt.Sprintf("(%s)", FormatExpr(e.Expr))
	case *FunctionCall:
		if e.Star {
			return strings.ToLower(e.Name) + "(*)"
		}
		args := make([]string, len(e.Args))
		for i, a := range e.Args {
			args[i] = FormatExpr(a)
		}
		prefix := ""
		if e.Distinct {
			prefix = "DISTINCT "
		}
		return strings.ToLower(e.Name) + "(" + prefix + strings.Join(args, ", ") + ")"
	case *CastExpr:
		return fmt.Sprintf("CAST(%s AS %s)", FormatExpr(e.Expr), e.Type.Name)
	case *CaseExpr:
		var b strings.Builder
		b.WriteString("CASE")
		if e.Operand != nil {
			b.WriteString(" ")
			b.WriteString(FormatExpr(e.Operand))
		}
		for _, w := range e.Whens {
			b.WriteString(" WHEN ")
			b.WriteString(FormatExpr(w.Condition))
			b.WriteString(" THEN ")
			b.WriteString(FormatExpr(w.Result))
		}
		if e.Else != nil {
			b.WriteString(" ELSE ")
			b.WriteString(FormatExpr(e.Else))
		}
		b.WriteString(" END")
		return b.String()
	case *InExpr:
		not := ""
		if e.Not {
			not = "NOT "
		}
		if e.Subquery != nil {
			return fmt.Sprintf("(%s %sIN (SELECT ...))", FormatExpr(e.Left), not)
		}
		vals := make([]string, len(e.Values))
		for i, v := range e.Values {
			vals[i] = FormatExpr(v)
		}
		return fmt.Sprintf("(%s %sIN (%s))", FormatExpr(e.Left), not, strings.Join(vals, ", "))
	case *BetweenExpr:
		not := ""
		if e.Not {
			not = "NOT "
		}
		return fmt.Sprintf("(%s %sBETWEEN %s AND %s)", FormatExpr(e.Left), not, FormatExpr(e.Low), FormatExpr(e.High))
	case *LikeExpr:
		not := ""
		if e.Not {
			not = "NOT "
		}
		out := fmt.Sprintf("(%s %sLIKE %s)", FormatExpr(e.Left), not, FormatExpr(e.Pattern))
		if e.Escape != nil {
			out = fmt.Sprintf("(%s %sLIKE %s ESCAPE %s)", FormatExpr(e.Left), not, FormatExpr(e.Pattern), FormatExpr(e.Escape))
		}
		return out
	case *IsNullExpr:
		if e.Not {
			return fmt.Sprintf("(%s IS NOT NULL)", FormatExpr(e.Left))
		}
		return fmt.Sprintf("(%s IS NULL)", FormatExpr(e.Left))
	case *IsDistinctExpr:
		op := "IS DISTINCT FROM"
		if e.Not {
			op = "IS NOT DISTINCT FROM"
		}
		return fmt.Sprintf("(%s %s %s)", FormatExpr(e.Left), op, FormatExpr(e.Right))
	case *SubqueryExpr:
		return "(SELECT ...)"
	default:
		return ""
	}
}

// operatorString renders an operator token back to SQL.
func operatorString(op lexer.TokenType) string {
	switch op {
	case lexer.TokenPlus:
		return "+"
	case lexer.TokenMinus:
		return "-"
	case lexer.TokenStar:
		return "*"
	case lexer.TokenSlash:
		return "/"
	case lexer.TokenPercent:
		return "%"
	case lexer.TokenConcat:
		return "||"
	case lexer.TokenEq:
		return "="
	case lexer.TokenNeq:
		return "<>"
	case lexer.TokenLt:
		return "<"
	case lexer.TokenLte:
		return "<="
	case lexer.TokenGt:
		return ">"
	case lexer.TokenGte:
		return ">="
	case lexer.TokenAND:
		return "AND"
	case lexer.TokenOR:
		return "OR"
	case lexer.TokenBitAnd:
		return "&"
	case lexer.TokenBitOr:
		return "|"
	case lexer.TokenBitNot:
		return "~"
	case lexer.TokenShiftLeft:
		return "<<"
	case lexer.TokenShiftRight:
		return ">>"
	default:
		return op.String()
	}
}

// quoteFormatIdent quotes an identifier when it is not a bare word.
func quoteFormatIdent(name string) string {
	if name == "" {
		return name
	}
	bare := true
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')) {
			bare = false
			break
		}
	}
	if bare {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
