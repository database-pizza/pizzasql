package parser

import (
	"fmt"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
)

// cteDef is a single common table expression parsed from a WITH clause.
type cteDef struct {
	name  string
	cols  []string
	query *SelectStmt
}

// isWithStart reports whether the current token begins a WITH clause. WITH is
// not a lexer keyword (it can be a column or table name), so it is recognized
// by its literal at statement start.
func (p *Parser) isWithStart() bool {
	return p.curTokenIs(lexer.TokenIdent) && strings.EqualFold(p.curToken.Literal, "WITH")
}

// parseWithStatement parses a WITH clause followed by a SELECT and desugars each
// non-recursive CTE into a derived table. Downstream stages therefore only ever
// see regular SELECTs. Recursive CTEs are rejected explicitly rather than being
// silently mis-executed.
func (p *Parser) parseWithStatement() (Statement, error) {
	p.nextToken() // consume WITH

	recursive := false
	if p.curTokenIs(lexer.TokenIdent) && strings.EqualFold(p.curToken.Literal, "RECURSIVE") {
		recursive = true
		p.nextToken()
	}

	var ctes []*cteDef
	for {
		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, p.curError("expected CTE name")
		}
		cte := &cteDef{name: p.curToken.Literal}
		p.nextToken()

		if p.curTokenIs(lexer.TokenLParen) {
			p.nextToken()
			for {
				if !p.curTokenIs(lexer.TokenIdent) {
					return nil, p.curError("expected column name in CTE column list")
				}
				cte.cols = append(cte.cols, p.curToken.Literal)
				p.nextToken()
				if p.curTokenIs(lexer.TokenComma) {
					p.nextToken()
					continue
				}
				break
			}
			if !p.curTokenIs(lexer.TokenRParen) {
				return nil, p.curError("expected ) after CTE column list")
			}
			p.nextToken()
		}

		if !p.curTokenIs(lexer.TokenAS) {
			return nil, p.curError("expected AS in CTE definition")
		}
		p.nextToken()
		if !p.curTokenIs(lexer.TokenLParen) {
			return nil, p.curError("expected ( before CTE query")
		}
		p.nextToken()

		query, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		if !p.curTokenIs(lexer.TokenRParen) {
			return nil, p.curError("expected ) after CTE query")
		}
		p.nextToken()

		// A recursive CTE's query is a compound (anchor UNION recursive), so its
		// column names are applied at materialization time instead.
		if !recursive {
			if err := applyCTEColumnNames(cte, query); err != nil {
				return nil, err
			}
		}
		cte.query = query
		ctes = append(ctes, cte)

		if p.curTokenIs(lexer.TokenComma) {
			p.nextToken()
			continue
		}
		break
	}

	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}

	if recursive {
		// Recursive CTEs are materialized by the executor, which needs the
		// definitions; desugaring cannot express self-reference. Only SELECT
		// carries that machinery today.
		sel, ok := stmt.(*SelectStmt)
		if !ok {
			return nil, p.curError("WITH RECURSIVE is only supported before a SELECT statement")
		}
		sel.With = make([]*CTE, 0, len(ctes))
		for _, c := range ctes {
			sel.With = append(sel.With, &CTE{
				Name:      c.name,
				Columns:   c.cols,
				Recursive: true,
				Query:     c.query,
			})
		}
		return sel, nil
	}

	// Each CTE may reference the CTEs defined before it.
	for i := range ctes {
		if err := substituteSelectCTEs(ctes[i].query, ctes[:i]); err != nil {
			return nil, err
		}
	}
	if err := substituteStatementCTEs(stmt, ctes); err != nil {
		return nil, err
	}
	return stmt, nil
}

// substituteStatementCTEs desugars named CTE references inside any DML
// statement. UPDATE reads them from its FROM clause (and expressions); DELETE
// and INSERT read them from subqueries.
func substituteStatementCTEs(stmt Statement, ctes []*cteDef) error {
	switch s := stmt.(type) {
	case *SelectStmt:
		return substituteSelectCTEs(s, ctes)
	case *UpdateStmt:
		for i := range s.From {
			if err := substituteTableRefCTEs(&s.From[i], ctes); err != nil {
				return err
			}
		}
		for i := range s.Set {
			if err := substituteExprCTEs(s.Set[i].Value, ctes); err != nil {
				return err
			}
		}
		if err := substituteExprCTEs(s.Where, ctes); err != nil {
			return err
		}
		for i := range s.Returning {
			if err := substituteExprCTEs(s.Returning[i].Expr, ctes); err != nil {
				return err
			}
		}
		return nil
	case *DeleteStmt:
		if err := substituteExprCTEs(s.Where, ctes); err != nil {
			return err
		}
		for i := range s.Returning {
			if err := substituteExprCTEs(s.Returning[i].Expr, ctes); err != nil {
				return err
			}
		}
		return nil
	case *InsertStmt:
		if s.Select != nil {
			return substituteSelectCTEs(s.Select, ctes)
		}
		for _, row := range s.Values {
			for _, v := range row {
				if err := substituteExprCTEs(v, ctes); err != nil {
					return err
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("WITH is not supported before this statement")
	}
}

// applyCTEColumnNames aliases the CTE query's projection columns with the names
// given in the CTE column list so a derived-table reference exposes them.
func applyCTEColumnNames(cte *cteDef, query *SelectStmt) error {
	if len(cte.cols) == 0 {
		return nil
	}
	if query.Compound != nil {
		return fmt.Errorf("CTE %q: column list on a compound query is not supported", cte.name)
	}
	if len(cte.cols) > len(query.Columns) {
		return fmt.Errorf("CTE %q: %d column names for %d columns", cte.name, len(cte.cols), len(query.Columns))
	}
	for i, name := range cte.cols {
		query.Columns[i].Alias = name
	}
	return nil
}

// substituteSelectCTEs replaces every reference to a named CTE with a derived
// table carrying that CTE's query. Substitution recurses through set operations,
// derived tables, joins, and subquery expressions.
func substituteSelectCTEs(sel *SelectStmt, ctes []*cteDef) error {
	if sel == nil {
		return nil
	}
	if sel.Compound != nil {
		if err := substituteSelectCTEs(sel.Compound.Left, ctes); err != nil {
			return err
		}
		if err := substituteSelectCTEs(sel.Compound.Right, ctes); err != nil {
			return err
		}
	}
	for i := range sel.From {
		if err := substituteTableRefCTEs(&sel.From[i], ctes); err != nil {
			return err
		}
	}
	if err := substituteExprCTEs(sel.Where, ctes); err != nil {
		return err
	}
	for i := range sel.Columns {
		if err := substituteExprCTEs(sel.Columns[i].Expr, ctes); err != nil {
			return err
		}
	}
	for i := range sel.GroupBy {
		if err := substituteExprCTEs(sel.GroupBy[i], ctes); err != nil {
			return err
		}
	}
	if err := substituteExprCTEs(sel.Having, ctes); err != nil {
		return err
	}
	for i := range sel.OrderBy {
		if err := substituteExprCTEs(sel.OrderBy[i].Expr, ctes); err != nil {
			return err
		}
	}
	if err := substituteExprCTEs(sel.Limit, ctes); err != nil {
		return err
	}
	return substituteExprCTEs(sel.Offset, ctes)
}

func lookupCTE(name string, ctes []*cteDef) *cteDef {
	for _, cte := range ctes {
		if strings.EqualFold(cte.name, name) {
			return cte
		}
	}
	return nil
}

func substituteTableRefCTEs(ref *TableRef, ctes []*cteDef) error {
	if ref == nil {
		return nil
	}
	if ref.Subquery != nil {
		if err := substituteSelectCTEs(ref.Subquery, ctes); err != nil {
			return err
		}
	} else if cte := lookupCTE(ref.Name, ctes); cte != nil {
		alias := ref.Alias
		if alias == "" {
			alias = cte.name
		}
		ref.Subquery = cte.query
		ref.Name = ""
		ref.Alias = alias
	}
	if ref.Join != nil {
		return substituteJoinCTEs(ref.Join, ctes)
	}
	return nil
}

func substituteJoinCTEs(join *JoinClause, ctes []*cteDef) error {
	if join == nil {
		return nil
	}
	if err := substituteTableRefCTEs(join.Table, ctes); err != nil {
		return err
	}
	return substituteExprCTEs(join.Condition, ctes)
}

func substituteExprCTEs(expr Expr, ctes []*cteDef) error {
	if expr == nil {
		return nil
	}
	switch e := expr.(type) {
	case *InExpr:
		if err := substituteExprCTEs(e.Left, ctes); err != nil {
			return err
		}
		for _, v := range e.Values {
			if err := substituteExprCTEs(v, ctes); err != nil {
				return err
			}
		}
		return substituteSelectCTEs(e.Subquery, ctes)
	case *SubqueryExpr:
		return substituteSelectCTEs(e.Query, ctes)
	case *ExistsExpr:
		return substituteSelectCTEs(e.Subquery, ctes)
	case *BinaryExpr:
		if err := substituteExprCTEs(e.Left, ctes); err != nil {
			return err
		}
		return substituteExprCTEs(e.Right, ctes)
	case *UnaryExpr:
		return substituteExprCTEs(e.Operand, ctes)
	case *BetweenExpr:
		if err := substituteExprCTEs(e.Left, ctes); err != nil {
			return err
		}
		if err := substituteExprCTEs(e.Low, ctes); err != nil {
			return err
		}
		return substituteExprCTEs(e.High, ctes)
	case *LikeExpr:
		if err := substituteExprCTEs(e.Left, ctes); err != nil {
			return err
		}
		if err := substituteExprCTEs(e.Pattern, ctes); err != nil {
			return err
		}
		return substituteExprCTEs(e.Escape, ctes)
	case *IsNullExpr:
		return substituteExprCTEs(e.Left, ctes)
	case *IsDistinctExpr:
		if err := substituteExprCTEs(e.Left, ctes); err != nil {
			return err
		}
		return substituteExprCTEs(e.Right, ctes)
	case *CaseExpr:
		if err := substituteExprCTEs(e.Operand, ctes); err != nil {
			return err
		}
		for _, w := range e.Whens {
			if err := substituteExprCTEs(w.Condition, ctes); err != nil {
				return err
			}
			if err := substituteExprCTEs(w.Result, ctes); err != nil {
				return err
			}
		}
		return substituteExprCTEs(e.Else, ctes)
	case *FunctionCall:
		for _, a := range e.Args {
			if err := substituteExprCTEs(a, ctes); err != nil {
				return err
			}
		}
		return nil
	case *ParenExpr:
		return substituteExprCTEs(e.Expr, ctes)
	case *CastExpr:
		return substituteExprCTEs(e.Expr, ctes)
	}
	return nil
}
