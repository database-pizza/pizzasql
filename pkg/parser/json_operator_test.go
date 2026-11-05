package parser

import (
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
)

func TestParseJSONArrowChainingIsLeftAssociative(t *testing.T) {
	expr := parseExpr(t, "doc -> 'a' -> 0")

	outer, ok := expr.(*BinaryExpr)
	if !ok {
		t.Fatalf("expected *BinaryExpr, got %T", expr)
	}
	if outer.Op != lexer.TokenArrow {
		t.Fatalf("outer operator = %v, want ->", outer.Op)
	}
	lit, ok := outer.Right.(*LiteralExpr)
	if !ok || lit.Value != "0" {
		t.Fatalf("outer right = %#v, want literal 0", outer.Right)
	}

	inner, ok := outer.Left.(*BinaryExpr)
	if !ok {
		t.Fatalf("outer left = %T, want *BinaryExpr", outer.Left)
	}
	if inner.Op != lexer.TokenArrow {
		t.Fatalf("inner operator = %v, want ->", inner.Op)
	}
	ref, ok := inner.Left.(*ColumnRef)
	if !ok || ref.Table != "" || ref.Column != "doc" {
		t.Fatalf("inner left = %#v, want column doc", inner.Left)
	}
	key, ok := inner.Right.(*LiteralExpr)
	if !ok || key.Value != "a" {
		t.Fatalf("inner right = %#v, want literal 'a'", inner.Right)
	}
}

func TestParseJSONArrowPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		root    lexer.TokenType
		childOp lexer.TokenType
	}{
		{"concatenation binds looser", "doc -> 'a' || 'x'", lexer.TokenConcat, lexer.TokenArrow},
		{"addition binds looser", "doc -> 'a' + 1", lexer.TokenPlus, lexer.TokenArrow},
		{"multiplication binds looser", "doc ->> 'a' * 2", lexer.TokenStar, lexer.TokenArrowText},
		{"bitwise binds looser", "doc -> 'a' & 1", lexer.TokenBitAnd, lexer.TokenArrow},
		{"comparison binds looser", "doc ->> 'a' = 'x'", lexer.TokenEq, lexer.TokenArrowText},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr := parseExpr(t, tt.input)
			root, ok := expr.(*BinaryExpr)
			if !ok {
				t.Fatalf("expected *BinaryExpr, got %T", expr)
			}
			if root.Op != tt.root {
				t.Fatalf("root operator = %v, want %v", root.Op, tt.root)
			}
			child, ok := root.Left.(*BinaryExpr)
			if !ok {
				t.Fatalf("root left = %T, want *BinaryExpr", root.Left)
			}
			if child.Op != tt.childOp {
				t.Fatalf("left child operator = %v, want %v", child.Op, tt.childOp)
			}
		})
	}
}

func TestParseJSONArrowBindsLooserThanUnary(t *testing.T) {
	expr := parseExpr(t, "-doc -> 'a'")
	arrow, ok := expr.(*BinaryExpr)
	if !ok || arrow.Op != lexer.TokenArrow {
		t.Fatalf("expected root ->, got %#v", expr)
	}
	neg, ok := arrow.Left.(*UnaryExpr)
	if !ok || neg.Op != lexer.TokenMinus {
		t.Fatalf("arrow left = %#v, want unary minus", arrow.Left)
	}

	// The right operand may be a negative array index.
	expr = parseExpr(t, "doc -> -1")
	arrow, ok = expr.(*BinaryExpr)
	if !ok || arrow.Op != lexer.TokenArrow {
		t.Fatalf("expected root ->, got %#v", expr)
	}
	neg, ok = arrow.Right.(*UnaryExpr)
	if !ok || neg.Op != lexer.TokenMinus {
		t.Fatalf("arrow right = %#v, want unary minus", arrow.Right)
	}
}

func TestParseJSONArrowRespectsParentheses(t *testing.T) {
	expr := parseExpr(t, "(doc -> 'a') ->> 'b'")

	outer, ok := expr.(*BinaryExpr)
	if !ok || outer.Op != lexer.TokenArrowText {
		t.Fatalf("expected root ->>, got %#v", expr)
	}
	paren, ok := outer.Left.(*ParenExpr)
	if !ok {
		t.Fatalf("outer left = %T, want *ParenExpr", outer.Left)
	}
	inner, ok := paren.Expr.(*BinaryExpr)
	if !ok || inner.Op != lexer.TokenArrow {
		t.Fatalf("parenthesized expression = %#v, want ->", paren.Expr)
	}
}
