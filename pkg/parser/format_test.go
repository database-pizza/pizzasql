package parser

import (
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
)

// FormatExpr output is persisted in the schema and re-parsed on every later
// evaluation, so an identifier that lexes as a keyword has to stay quoted or the
// stored text would no longer name a column.
func TestFormatExprQuotesKeywordIdentifiers(t *testing.T) {
	tests := []struct {
		name string
		expr Expr
		want string
	}{
		{"keyword column", &ColumnRef{Column: "order"}, `"order"`},
		{"plain column", &ColumnRef{Column: "age"}, "age"},
		{"keyword in expression", &BinaryExpr{
			Left:  &ColumnRef{Column: "order"},
			Op:    lexer.TokenGt,
			Right: &LiteralExpr{Type: lexer.TokenNumber, Value: "0"},
		}, `("order" > 0)`},
		{"qualified keyword column", &ColumnRef{Table: "select", Column: "from"}, `"select"."from"`},
		{"underscored column", &ColumnRef{Column: "user_id"}, "user_id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			formatted := FormatExpr(tt.expr)
			if formatted != tt.want {
				t.Fatalf("FormatExpr = %q, want %q", formatted, tt.want)
			}
			if _, err := ParseExpr(formatted); err != nil {
				t.Fatalf("formatted expression %q does not re-parse: %v", formatted, err)
			}
		})
	}
}
