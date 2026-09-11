package parser

import (
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
)

// Parser parses SQL statements into an AST.
type Parser struct {
	lexer     *lexer.Lexer
	curToken  lexer.Token
	peekToken lexer.Token
	errors    []*ParseError
}

// New creates a new Parser.
func New(l *lexer.Lexer) *Parser {
	p := &Parser{lexer: l}
	// Read two tokens to initialize curToken and peekToken
	p.nextToken()
	p.nextToken()
	return p
}

// Parse parses a SQL statement.
func (p *Parser) Parse() (Statement, error) {
	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}

	// Consume optional semicolon
	if p.curTokenIs(lexer.TokenSemicolon) {
		p.nextToken()
	}
	if !p.curTokenIs(lexer.TokenEOF) {
		return nil, p.curError("unexpected trailing token: " + p.curToken.Type.String())
	}

	return stmt, nil
}

// ParseExpr parses a standalone SQL expression, used for persisted generated
// column and expression-index definitions. It rejects trailing tokens so a
// malformed stored expression surfaces immediately.
func ParseExpr(input string) (Expr, error) {
	p := New(lexer.New(input))
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if !p.curTokenIs(lexer.TokenEOF) {
		return nil, p.curError("unexpected trailing token: " + p.curToken.Type.String())
	}
	return expr, nil
}

// ParseMultiple parses multiple SQL statements.
func (p *Parser) ParseMultiple() ([]Statement, error) {
	var stmts []Statement

	for !p.curTokenIs(lexer.TokenEOF) {
		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, stmt)

		// Consume optional semicolon
		if p.curTokenIs(lexer.TokenSemicolon) {
			p.nextToken()
		}
	}

	return stmts, nil
}

func (p *Parser) nextToken() {
	p.curToken = p.peekToken
	p.peekToken = p.lexer.NextToken()

	// Skip comments
	for p.peekToken.Type == lexer.TokenComment {
		p.peekToken = p.lexer.NextToken()
	}
}

func (p *Parser) curTokenIs(t lexer.TokenType) bool {
	return p.curToken.Type == t
}

func (p *Parser) peekTokenIs(t lexer.TokenType) bool {
	return p.peekToken.Type == t
}

func (p *Parser) expectPeek(t lexer.TokenType) error {
	if p.peekTokenIs(t) {
		p.nextToken()
		return nil
	}
	return p.peekError(t)
}

// curIdentIs reports whether the current token is an identifier spelling word
// case-insensitively. It lets context-sensitive keywords (GENERATED, ALWAYS,
// STORED, VIRTUAL) be recognized only in the positions where they are
// meaningful, so they remain usable as ordinary identifiers elsewhere.
func (p *Parser) curIdentIs(word string) bool {
	return p.curToken.Type == lexer.TokenIdent && strings.EqualFold(p.curToken.Literal, word)
}

func (p *Parser) peekError(t lexer.TokenType) error {
	return newError(
		"expected "+t.String()+", got "+p.peekToken.Type.String(),
		p.peekToken.Line,
		p.peekToken.Column,
		p.peekToken.Literal,
	)
}

func (p *Parser) curError(msg string) error {
	return newError(
		msg,
		p.curToken.Line,
		p.curToken.Column,
		p.curToken.Literal,
	)
}

func (p *Parser) parseStatement() (Statement, error) {
	if p.isWithStart() {
		return p.parseWithStatement()
	}
	switch p.curToken.Type {
	case lexer.TokenSELECT:
		return p.parseSelect()
	case lexer.TokenINSERT:
		return p.parseInsert()
	case lexer.TokenUPDATE:
		return p.parseUpdate()
	case lexer.TokenDELETE:
		return p.parseDelete()
	case lexer.TokenCREATE:
		return p.parseCreate()
	case lexer.TokenDROP:
		return p.parseDrop()
	case lexer.TokenALTER:
		return p.parseAlter()
	case lexer.TokenATTACH:
		return p.parseAttach()
	case lexer.TokenDETACH:
		return p.parseDetach()
	case lexer.TokenPRAGMA:
		return p.parsePragma()
	case lexer.TokenANALYZE:
		return p.parseAnalyze()
	case lexer.TokenEXPLAIN:
		return p.parseExplain()
	case lexer.TokenBEGIN:
		return p.parseBegin()
	case lexer.TokenCOMMIT:
		return p.parseCommit()
	case lexer.TokenROLLBACK:
		return p.parseRollback()
	case lexer.TokenSAVEPOINT:
		return p.parseSavepoint()
	case lexer.TokenRELEASE:
		return p.parseRelease()
	default:
		return nil, p.curError("unexpected token: " + p.curToken.Type.String())
	}
}

// parseSingleSelectTerm parses one SELECT body (SELECT … FROM … WHERE … GROUP BY … HAVING …)
// but stops before any set operator, ORDER BY, LIMIT, or OFFSET.
// Callers that want the full chain (including set ops) use parseSelect instead.
func (p *Parser) parseSingleSelectTerm() (*SelectStmt, error) {
	stmt := &SelectStmt{}

	p.nextToken() // consume SELECT

	// Check for DISTINCT / ALL
	if p.curTokenIs(lexer.TokenDISTINCT) {
		stmt.Distinct = true
		p.nextToken()
	} else if p.curTokenIs(lexer.TokenALL) {
		p.nextToken()
	}

	cols, err := p.parseSelectColumns()
	if err != nil {
		return nil, err
	}
	stmt.Columns = cols

	if p.curTokenIs(lexer.TokenFROM) {
		p.nextToken()
		tables, err := p.parseTableRefs()
		if err != nil {
			return nil, err
		}
		stmt.From = tables
	}

	if p.curTokenIs(lexer.TokenWHERE) {
		p.nextToken()
		where, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Where = where
	}

	if p.curTokenIs(lexer.TokenGROUP) {
		if err := p.expectPeek(lexer.TokenBY); err != nil {
			return nil, err
		}
		p.nextToken()
		groupBy, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		stmt.GroupBy = groupBy
	}

	if p.curTokenIs(lexer.TokenHAVING) {
		p.nextToken()
		having, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Having = having
	}

	return stmt, nil
}

// parseSelect parses a SELECT statement, including any trailing set operations
// (UNION / INTERSECT / EXCEPT) and an optional ORDER BY / LIMIT / OFFSET.
func (p *Parser) parseSelect() (*SelectStmt, error) {
	stmt, err := p.parseSingleSelectTerm()
	if err != nil {
		return nil, err
	}

	// Handle set operations: UNION [ALL], INTERSECT, EXCEPT
	if p.curTokenIs(lexer.TokenUNION) || p.curTokenIs(lexer.TokenINTERSECT) || p.curTokenIs(lexer.TokenEXCEPT) {
		compound, err := p.parseCompoundChain(stmt)
		if err != nil {
			return nil, err
		}
		return &SelectStmt{Compound: compound}, nil
	}

	// Parse ORDER BY clause
	if p.curTokenIs(lexer.TokenORDER) {
		if err := p.expectPeek(lexer.TokenBY); err != nil {
			return nil, err
		}
		p.nextToken()
		orderBy, err := p.parseOrderBy()
		if err != nil {
			return nil, err
		}
		stmt.OrderBy = orderBy
	}

	// Parse LIMIT clause
	if p.curTokenIs(lexer.TokenLIMIT) {
		p.nextToken()
		limit, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Limit = limit
	}

	// Parse OFFSET clause
	if p.curTokenIs(lexer.TokenOFFSET) {
		p.nextToken()
		offset, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Offset = offset
	}

	return stmt, nil
}

// setOpPrec returns the precedence of a set operator token.
// INTERSECT binds more tightly than UNION/EXCEPT per SQL standard.
func setOpPrec(t lexer.TokenType) int {
	if t == lexer.TokenINTERSECT {
		return 2
	}
	return 1
}

// parseCompoundChain collects all set-op legs and builds a left-associative tree
// respecting INTERSECT > UNION/EXCEPT precedence.
//
// The algorithm is the standard precedence-climbing / Pratt approach:
//
//	parseMin(minPrec):
//	  left = first already-parsed SELECT (passed in as `first`)
//	  while curOp.prec >= minPrec:
//	      op = curOp; consume op
//	      right = parseSingleSelect()
//	      while nextOp.prec > op.prec:   // right-bind tighter ops
//	          right = parseMin(op.prec+1) using right as seed
//	      left = Compound(left, op, right)
//	  return left
func (p *Parser) parseCompoundChain(first *SelectStmt) (*CompoundSelect, error) {
	type leg struct {
		op    SetOpType
		query *SelectStmt
	}

	// Consume a set-op token and return its SetOpType + precedence.
	consumeOp := func() (SetOpType, int, error) {
		switch p.curToken.Type {
		case lexer.TokenUNION:
			p.nextToken()
			if p.curTokenIs(lexer.TokenALL) {
				p.nextToken()
				return SetOpUnionAll, 1, nil
			}
			return SetOpUnion, 1, nil
		case lexer.TokenINTERSECT:
			p.nextToken()
			return SetOpIntersect, 2, nil
		case lexer.TokenEXCEPT:
			p.nextToken()
			return SetOpExcept, 1, nil
		}
		return 0, 0, p.curError("expected UNION, INTERSECT, or EXCEPT")
	}

	isSetOp := func() bool {
		return p.curTokenIs(lexer.TokenUNION) ||
			p.curTokenIs(lexer.TokenINTERSECT) ||
			p.curTokenIs(lexer.TokenEXCEPT)
	}

	// parseSingleSelect parses the next SELECT term (no set-ops claimed).
	parseSingleSelect := func() (*SelectStmt, error) {
		if !p.curTokenIs(lexer.TokenSELECT) {
			return nil, p.curError("expected SELECT after set operator")
		}
		return p.parseSingleSelectTerm()
	}

	// Precedence-climbing: build left-to-right tree, INTERSECT binds tighter.
	var climb func(left *SelectStmt, minPrec int) (*CompoundSelect, error)
	climb = func(left *SelectStmt, minPrec int) (*CompoundSelect, error) {
		for isSetOp() && setOpPrec(p.curToken.Type) >= minPrec {
			op, prec, err := consumeOp()
			if err != nil {
				return nil, err
			}
			right, err := parseSingleSelect()
			if err != nil {
				return nil, err
			}
			// If the right node itself resolved to a compound (via recursive parseSelect),
			// unwrap and re-climb properly.
			if right.Compound != nil {
				// right was a sub-chain; treat it as already climbed.
			} else {
				// Absorb any higher-precedence ops on the right.
				for isSetOp() && setOpPrec(p.curToken.Type) > prec {
					sub, err := climb(right, prec+1)
					if err != nil {
						return nil, err
					}
					right = &SelectStmt{Compound: sub}
					break
				}
			}
			node := &CompoundSelect{Left: left, Op: op, Right: right}
			left = &SelectStmt{Compound: node}
		}
		if left.Compound != nil {
			return left.Compound, nil
		}
		return nil, p.curError("internal: no compound built")
	}

	compound, err := climb(first, 1)
	if err != nil {
		return nil, err
	}

	// Parse trailing ORDER BY / LIMIT / OFFSET that apply to the whole compound.
	if p.curTokenIs(lexer.TokenORDER) {
		if err := p.expectPeek(lexer.TokenBY); err != nil {
			return nil, err
		}
		p.nextToken()
		orderBy, err := p.parseOrderBy()
		if err != nil {
			return nil, err
		}
		compound.OrderBy = orderBy
	}
	if p.curTokenIs(lexer.TokenLIMIT) {
		p.nextToken()
		limit, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		compound.Limit = limit
	}
	if p.curTokenIs(lexer.TokenOFFSET) {
		p.nextToken()
		offset, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		compound.Offset = offset
	}

	return compound, nil
}

func (p *Parser) parseSelectColumns() ([]SelectColumn, error) {
	var cols []SelectColumn

	for {
		col := SelectColumn{}

		if p.curTokenIs(lexer.TokenStar) {
			col.Star = true
			p.nextToken()
		} else {
			expr, err := p.parseExpr()
			if err != nil {
				return nil, err
			}

			// Qualified wildcard (table.*): the expression grammar parses this as
			// ColumnRef{Table: table, Column: "*"}. Lift it into a scoped wildcard
			// projection instead of a column reference literally named "*".
			if ref, ok := expr.(*ColumnRef); ok && ref.Table != "" && ref.Column == "*" {
				col.TableStar = ref.Table
			} else {
				col.Expr = expr

				// Check for AS alias
				if p.curTokenIs(lexer.TokenAS) {
					p.nextToken()
					if !p.curTokenIs(lexer.TokenIdent) {
						return nil, p.curError("expected identifier after AS")
					}
					col.Alias = p.curToken.Literal
					p.nextToken()
				} else if p.curTokenIs(lexer.TokenIdent) {
					// Alias without AS
					col.Alias = p.curToken.Literal
					p.nextToken()
				}
			}
		}

		cols = append(cols, col)

		if !p.curTokenIs(lexer.TokenComma) {
			break
		}
		p.nextToken() // consume comma
	}

	return cols, nil
}

func (p *Parser) parseTableRefs() ([]TableRef, error) {
	var tables []TableRef

	table, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	tables = append(tables, *table)

	// Parse JOINs or comma-separated tables
	for {
		if p.curTokenIs(lexer.TokenComma) {
			p.nextToken()
			table, err := p.parseTableRef()
			if err != nil {
				return nil, err
			}
			tables = append(tables, *table)
		} else if p.isJoinKeyword() {
			join, err := p.parseJoin()
			if err != nil {
				return nil, err
			}
			// Find the last Join in the chain and attach new join there
			lastTable := &tables[len(tables)-1]
			if lastTable.Join == nil {
				lastTable.Join = join
			} else {
				// Find the end of the join chain
				current := lastTable.Join
				for current.Table != nil && current.Table.Join != nil {
					current = current.Table.Join
				}
				// Attach to the end of the chain
				if current.Table != nil {
					current.Table.Join = join
				}
			}
		} else {
			break
		}
	}

	return tables, nil
}

func (p *Parser) parseTableRef() (*TableRef, error) {
	ref := &TableRef{}

	// Check for subquery (SELECT ...)
	if p.curTokenIs(lexer.TokenLParen) {
		p.nextToken()
		if p.curTokenIs(lexer.TokenSELECT) {
			subquery, err := p.parseSelect()
			if err != nil {
				return nil, err
			}
			ref.Subquery = subquery

			if !p.curTokenIs(lexer.TokenRParen) {
				return nil, p.curError("expected ) after subquery")
			}
			p.nextToken()

			// Subquery must have an alias
			if p.curTokenIs(lexer.TokenAS) {
				p.nextToken()
			}
			if !p.curTokenIs(lexer.TokenIdent) {
				return nil, p.curError("subquery in FROM must have an alias")
			}
			ref.Alias = p.curToken.Literal
			p.nextToken()

			return ref, nil
		}
		// Parenthesized table expression: (table1 JOIN table2) or (table1, table2)
		innerRefs, err := p.parseTableRefs()
		if err != nil {
			return nil, err
		}
		if !p.curTokenIs(lexer.TokenRParen) {
			return nil, p.curError("expected ) after table expression")
		}
		p.nextToken()
		// Flatten: use first ref as base, attach remaining as joins
		if len(innerRefs) == 0 {
			return nil, p.curError("empty table expression")
		}
		base := &innerRefs[0]
		for i := 1; i < len(innerRefs); i++ {
			cur := base
			for cur.Join != nil && cur.Join.Table != nil {
				cur = cur.Join.Table
			}
			extra := innerRefs[i]
			if cur.Join == nil {
				cur.Join = &JoinClause{Type: JoinCross, Table: &extra}
			} else {
				cur.Join.Table = &extra
			}
		}
		return base, nil
	}

	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected table name")
	}

	ref.Name = p.curToken.Literal
	p.nextToken()

	// Check for schema.table
	if p.curTokenIs(lexer.TokenDot) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, p.curError("expected table name after dot")
		}
		ref.Schema = ref.Name
		ref.Name = p.curToken.Literal
		p.nextToken()
	}

	// Check for alias
	if p.curTokenIs(lexer.TokenAS) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, p.curError("expected identifier after AS")
		}
		ref.Alias = p.curToken.Literal
		p.nextToken()
	} else if p.curTokenIs(lexer.TokenIdent) && !p.isClauseKeyword() {
		ref.Alias = p.curToken.Literal
		p.nextToken()
	}

	return ref, nil
}

func (p *Parser) isJoinKeyword() bool {
	switch p.curToken.Type {
	case lexer.TokenJOIN, lexer.TokenINNER, lexer.TokenLEFT,
		lexer.TokenRIGHT, lexer.TokenFULL, lexer.TokenCROSS,
		lexer.TokenNATURAL:
		return true
	}
	return false
}

func (p *Parser) isClauseKeyword() bool {
	switch p.curToken.Type {
	case lexer.TokenWHERE, lexer.TokenGROUP, lexer.TokenHAVING,
		lexer.TokenORDER, lexer.TokenLIMIT, lexer.TokenOFFSET,
		lexer.TokenUNION, lexer.TokenINTERSECT, lexer.TokenEXCEPT,
		lexer.TokenON, lexer.TokenUSING:
		return true
	}
	return false
}

func (p *Parser) parseJoin() (*JoinClause, error) {
	join := &JoinClause{Type: JoinInner}

	// Determine join type
	switch p.curToken.Type {
	case lexer.TokenINNER:
		join.Type = JoinInner
		p.nextToken()
	case lexer.TokenLEFT:
		join.Type = JoinLeft
		p.nextToken()
		if p.curTokenIs(lexer.TokenOUTER) {
			p.nextToken()
		}
	case lexer.TokenRIGHT:
		join.Type = JoinRight
		p.nextToken()
		if p.curTokenIs(lexer.TokenOUTER) {
			p.nextToken()
		}
	case lexer.TokenFULL:
		join.Type = JoinFull
		p.nextToken()
		if p.curTokenIs(lexer.TokenOUTER) {
			p.nextToken()
		}
	case lexer.TokenCROSS:
		join.Type = JoinCross
		p.nextToken()
	case lexer.TokenNATURAL:
		p.nextToken()
		// Could be NATURAL LEFT/RIGHT/INNER JOIN
		if p.curTokenIs(lexer.TokenLEFT) {
			join.Type = JoinLeft
			p.nextToken()
		} else if p.curTokenIs(lexer.TokenRIGHT) {
			join.Type = JoinRight
			p.nextToken()
		}
	}

	// Expect JOIN keyword
	if p.curTokenIs(lexer.TokenJOIN) {
		p.nextToken()
	} else if p.curToken.Type != lexer.TokenIdent {
		return nil, p.curError("expected JOIN")
	}

	// Parse table reference
	table, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	join.Table = table

	// Parse ON or USING clause
	if p.curTokenIs(lexer.TokenON) {
		p.nextToken()
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		join.Condition = cond
	} else if p.curTokenIs(lexer.TokenUSING) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenLParen) {
			return nil, p.curError("expected (")
		}
		p.nextToken()
		cols, err := p.parseIdentList()
		if err != nil {
			return nil, err
		}
		join.Using = cols
		if !p.curTokenIs(lexer.TokenRParen) {
			return nil, p.curError("expected )")
		}
		p.nextToken()
	}

	return join, nil
}

func (p *Parser) parseOrderBy() ([]OrderByItem, error) {
	var items []OrderByItem

	for {
		item := OrderByItem{}

		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		item.Expr = expr

		if p.curTokenIs(lexer.TokenDESC) {
			item.Desc = true
			p.nextToken()
		} else if p.curTokenIs(lexer.TokenASC) {
			p.nextToken()
		}

		// Optional NULLS FIRST / NULLS LAST.
		if p.curTokenIs(lexer.TokenIdent) && strings.EqualFold(p.curToken.Literal, "NULLS") {
			p.nextToken()
			if !p.curTokenIs(lexer.TokenIdent) {
				return nil, p.curError("expected FIRST or LAST after NULLS")
			}
			switch {
			case strings.EqualFold(p.curToken.Literal, "FIRST"):
				item.NullsOrder = NullsFirst
			case strings.EqualFold(p.curToken.Literal, "LAST"):
				item.NullsOrder = NullsLast
			default:
				return nil, p.curError("expected FIRST or LAST after NULLS")
			}
			p.nextToken()
		}

		items = append(items, item)

		if !p.curTokenIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}

	return items, nil
}

// parseInsert parses an INSERT statement.
func (p *Parser) parseInsert() (*InsertStmt, error) {
	stmt := &InsertStmt{}

	p.nextToken() // consume INSERT

	// Check for OR conflict clause
	if p.curTokenIs(lexer.TokenOR) {
		p.nextToken()
		switch p.curToken.Type {
		case lexer.TokenREPLACE:
			stmt.OnConflict = ConflictReplace
		case lexer.TokenIGNORE:
			stmt.OnConflict = ConflictIgnore
		case lexer.TokenFAIL:
			stmt.OnConflict = ConflictFail
		case lexer.TokenABORT:
			stmt.OnConflict = ConflictAbort
		case lexer.TokenROLLBACK:
			stmt.OnConflict = ConflictRollback
		default:
			return nil, p.curError("expected REPLACE, IGNORE, FAIL, ABORT, or ROLLBACK after OR")
		}
		p.nextToken()
	}

	if !p.curTokenIs(lexer.TokenINTO) {
		return nil, p.curError("expected INTO")
	}
	p.nextToken()

	// Parse table name
	table, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	stmt.Table = table

	// Parse optional column list
	if p.curTokenIs(lexer.TokenLParen) {
		p.nextToken()
		cols, err := p.parseIdentList()
		if err != nil {
			return nil, err
		}
		stmt.Columns = cols
		if !p.curTokenIs(lexer.TokenRParen) {
			return nil, p.curError("expected )")
		}
		p.nextToken()
	}

	// Parse VALUES, SELECT, or SQLite's INSERT ... WITH ... SELECT form.
	if p.curTokenIs(lexer.TokenVALUES) {
		p.nextToken()
		values, err := p.parseValuesList()
		if err != nil {
			return nil, err
		}
		stmt.Values = values
	} else if p.curTokenIs(lexer.TokenSELECT) {
		sel, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		stmt.Select = sel
	} else if p.isWithStart() {
		withStmt, err := p.parseWithStatement()
		if err != nil {
			return nil, err
		}
		sel, ok := withStmt.(*SelectStmt)
		if !ok {
			return nil, p.curError("expected SELECT after WITH")
		}
		stmt.Select = sel
	} else {
		return nil, p.curError("expected VALUES, SELECT, or WITH")
	}

	if p.curTokenIs(lexer.TokenON) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenCONFLICT) {
			return nil, p.curError("expected CONFLICT after ON")
		}
		p.nextToken()
		if p.curTokenIs(lexer.TokenLParen) {
			p.nextToken()
			target, err := p.parseIdentList()
			if err != nil {
				return nil, err
			}
			stmt.ConflictTarget = target
			if !p.curTokenIs(lexer.TokenRParen) {
				return nil, p.curError("expected )")
			}
			p.nextToken()
		}
		if !p.curTokenIs(lexer.TokenDO) {
			return nil, p.curError("expected DO after ON CONFLICT")
		}
		p.nextToken()
		if p.curTokenIs(lexer.TokenNOTHING) {
			stmt.ConflictDoNothing = true
			p.nextToken()
		} else if p.curTokenIs(lexer.TokenUPDATE) {
			p.nextToken()
			if !p.curTokenIs(lexer.TokenSET) {
				return nil, p.curError("expected SET after DO UPDATE")
			}
			p.nextToken()
			for {
				if !p.curTokenIs(lexer.TokenIdent) {
					return nil, p.curError("expected column name")
				}
				column := p.curToken.Literal
				p.nextToken()
				if !p.curTokenIs(lexer.TokenEq) {
					return nil, p.curError("expected =")
				}
				p.nextToken()
				value, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				stmt.ConflictUpdate = append(stmt.ConflictUpdate, Assignment{Column: column, Value: value})
				if !p.curTokenIs(lexer.TokenComma) {
					break
				}
				p.nextToken()
			}
		} else {
			return nil, p.curError("expected NOTHING or UPDATE after DO")
		}
	}

	if p.curTokenIs(lexer.TokenRETURNING) {
		returning, err := p.parseReturningClause()
		if err != nil {
			return nil, err
		}
		stmt.Returning = returning
	}

	return stmt, nil
}

func (p *Parser) parseValuesList() ([][]Expr, error) {
	var rows [][]Expr

	for {
		if !p.curTokenIs(lexer.TokenLParen) {
			return nil, p.curError("expected (")
		}
		p.nextToken()

		row, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)

		if !p.curTokenIs(lexer.TokenRParen) {
			return nil, p.curError("expected )")
		}
		p.nextToken()

		if !p.curTokenIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}

	return rows, nil
}

// parseUpdate parses an UPDATE statement.
func (p *Parser) parseUpdate() (*UpdateStmt, error) {
	stmt := &UpdateStmt{}

	p.nextToken() // consume UPDATE

	// Parse table name
	table, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	stmt.Table = table

	// Expect SET
	if !p.curTokenIs(lexer.TokenSET) {
		return nil, p.curError("expected SET")
	}
	p.nextToken()

	// Parse assignments
	for {
		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, p.curError("expected column name")
		}
		col := p.curToken.Literal
		p.nextToken()

		if !p.curTokenIs(lexer.TokenEq) {
			return nil, p.curError("expected =")
		}
		p.nextToken()

		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}

		stmt.Set = append(stmt.Set, Assignment{Column: col, Value: val})

		if !p.curTokenIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}

	// Parse optional FROM clause (SQLite UPDATE ... FROM).
	if p.curTokenIs(lexer.TokenFROM) {
		p.nextToken()
		from, err := p.parseTableRefs()
		if err != nil {
			return nil, err
		}
		stmt.From = from
	}

	// Parse optional WHERE
	if p.curTokenIs(lexer.TokenWHERE) {
		p.nextToken()
		where, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Where = where
	}

	if p.curTokenIs(lexer.TokenRETURNING) {
		returning, err := p.parseReturningClause()
		if err != nil {
			return nil, err
		}
		stmt.Returning = returning
	}

	return stmt, nil
}

// parseDelete parses a DELETE statement.
func (p *Parser) parseDelete() (*DeleteStmt, error) {
	stmt := &DeleteStmt{}

	p.nextToken() // consume DELETE

	if !p.curTokenIs(lexer.TokenFROM) {
		return nil, p.curError("expected FROM")
	}
	p.nextToken()

	// Parse table name
	table, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	stmt.Table = table

	// Parse optional WHERE
	if p.curTokenIs(lexer.TokenWHERE) {
		p.nextToken()
		where, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Where = where
	}

	if p.curTokenIs(lexer.TokenRETURNING) {
		returning, err := p.parseReturningClause()
		if err != nil {
			return nil, err
		}
		stmt.Returning = returning
	}

	return stmt, nil
}

// parseReturningClause parses the projection after a RETURNING keyword. The
// caller must leave curToken on the RETURNING identifier.
func (p *Parser) parseReturningClause() ([]SelectColumn, error) {
	p.nextToken() // consume RETURNING
	cols, err := p.parseSelectColumns()
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, p.curError("RETURNING requires at least one expression")
	}
	return cols, nil
}

// parseCreate parses CREATE statements.
func (p *Parser) parseCreate() (Statement, error) {
	p.nextToken() // consume CREATE

	switch p.curToken.Type {
	case lexer.TokenTABLE:
		return p.parseCreateTable()
	case lexer.TokenINDEX:
		return p.parseCreateIndex(false)
	case lexer.TokenUNIQUE:
		p.nextToken() // consume UNIQUE
		if !p.curTokenIs(lexer.TokenINDEX) {
			return nil, p.curError("expected INDEX after UNIQUE")
		}
		return p.parseCreateIndex(true)
	case lexer.TokenVIEW:
		return p.parseCreateView()
	default:
		return nil, p.curError("expected TABLE or INDEX after CREATE")
	}
}

func (p *Parser) parseCreateTable() (*CreateTableStmt, error) {
	stmt := &CreateTableStmt{}

	p.nextToken() // consume TABLE

	// Check for IF NOT EXISTS
	if p.curTokenIs(lexer.TokenIF) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenNOT) {
			return nil, p.curError("expected NOT")
		}
		p.nextToken()
		if !p.curTokenIs(lexer.TokenEXISTS) {
			return nil, p.curError("expected EXISTS")
		}
		stmt.IfNotExists = true
		p.nextToken()
	}

	// Parse table name
	table, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	stmt.Table = table

	// Expect (
	if !p.curTokenIs(lexer.TokenLParen) {
		return nil, p.curError("expected (")
	}
	p.nextToken()

	// Parse column definitions and constraints
	for {
		if p.curTokenIs(lexer.TokenRParen) {
			break
		}

		// Check for table constraint
		if p.isTableConstraintStart() {
			constraint, err := p.parseTableConstraint()
			if err != nil {
				return nil, err
			}
			stmt.Constraints = append(stmt.Constraints, *constraint)
		} else {
			// Column definition
			col, err := p.parseColumnDef()
			if err != nil {
				return nil, err
			}
			stmt.Columns = append(stmt.Columns, *col)
		}

		if !p.curTokenIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}

	if !p.curTokenIs(lexer.TokenRParen) {
		return nil, p.curError("expected )")
	}
	p.nextToken()

	return stmt, nil
}

func (p *Parser) isTableConstraintStart() bool {
	switch p.curToken.Type {
	case lexer.TokenPRIMARY, lexer.TokenFOREIGN, lexer.TokenUNIQUE,
		lexer.TokenCHECK, lexer.TokenCONSTRAINT:
		return true
	}
	return false
}

func (p *Parser) parseColumnDef() (*ColumnDef, error) {
	col := &ColumnDef{}

	if !p.curIsName() {
		return nil, p.curError("expected column name")
	}
	col.Name = p.curToken.Literal
	p.nextToken()

	// Parse data type
	dataType, err := p.parseDataType()
	if err != nil {
		return nil, err
	}
	col.Type = *dataType

	// Parse column constraints
	for {
		if consumed, err := p.tryParseGeneratedColumn(col); err != nil {
			return nil, err
		} else if consumed {
			continue
		}
		constraint, ok, err := p.parseColumnConstraint()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if constraint != nil {
			col.Constraints = append(col.Constraints, *constraint)
		}
	}

	return col, nil
}

// tryParseGeneratedColumn parses a GENERATED ALWAYS AS (expr) [STORED|VIRTUAL]
// clause, or the shorthand AS (expr) [STORED|VIRTUAL]. It reports whether a
// clause was consumed. Generated columns cannot be written by the user.
func (p *Parser) tryParseGeneratedColumn(col *ColumnDef) (bool, error) {
	started := false
	if p.curIdentIs("GENERATED") {
		started = true
		p.nextToken()
		if !p.curIdentIs("ALWAYS") {
			return false, p.curError("expected ALWAYS after GENERATED")
		}
		p.nextToken()
	}
	if !p.curTokenIs(lexer.TokenAS) {
		if started {
			return false, p.curError("expected AS in generated column definition")
		}
		return false, nil
	}
	p.nextToken() // consume AS
	if !p.curTokenIs(lexer.TokenLParen) {
		return false, p.curError("expected ( after AS")
	}
	p.nextToken()
	expr, err := p.parseExpr()
	if err != nil {
		return false, err
	}
	if !p.curTokenIs(lexer.TokenRParen) {
		return false, p.curError("expected ) after generated expression")
	}
	p.nextToken()

	col.GeneratedExpr = expr
	if p.curIdentIs("STORED") {
		col.GeneratedStored = true
		p.nextToken()
	} else if p.curIdentIs("VIRTUAL") {
		col.GeneratedStored = false
		p.nextToken()
	}
	return true, nil
}

// identTypeAliases maps non-reserved type names (lexed as plain identifiers)
// to their canonical data type name. These are stored with SQLite text
// affinity and never gain native PostgreSQL semantics.
var identTypeAliases = map[string]string{
	"UUID": "UUID",
}

func (p *Parser) parseDataType() (*DataType, error) {
	dt := &DataType{}

	if p.curTokenIs(lexer.TokenIdent) {
		// A few well-known type names (e.g. UUID) are not reserved keywords and
		// lex as identifiers. Recognize them as type aliases so they can be used
		// in column definitions.
		name, ok := identTypeAliases[strings.ToUpper(p.curToken.Literal)]
		if !ok {
			return nil, p.curError("expected data type")
		}
		dt.Name = name
		p.nextToken()
	} else {
		if !p.isDataTypeKeyword() {
			return nil, p.curError("expected data type")
		}
		dt.Name = strings.ToUpper(p.curToken.Literal)
		p.nextToken()
	}

	// Check for precision/scale
	if p.curTokenIs(lexer.TokenLParen) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenNumber) {
			return nil, p.curError("expected number for precision")
		}
		// Parse precision (simplified - just store in Precision)
		dt.Precision = parseInt(p.curToken.Literal)
		p.nextToken()

		if p.curTokenIs(lexer.TokenComma) {
			p.nextToken()
			if !p.curTokenIs(lexer.TokenNumber) {
				return nil, p.curError("expected number for scale")
			}
			dt.Scale = parseInt(p.curToken.Literal)
			p.nextToken()
		}

		if !p.curTokenIs(lexer.TokenRParen) {
			return nil, p.curError("expected )")
		}
		p.nextToken()
	}

	return dt, nil
}

func (p *Parser) isDataTypeKeyword() bool {
	switch p.curToken.Type {
	case lexer.TokenINTEGER, lexer.TokenINT, lexer.TokenTINYINT, lexer.TokenSMALLINT, lexer.TokenMEDIUMINT, lexer.TokenBIGINT,
		lexer.TokenREAL, lexer.TokenFLOAT, lexer.TokenDOUBLE,
		lexer.TokenNUMERIC, lexer.TokenDECIMAL,
		lexer.TokenTEXT, lexer.TokenVARCHAR, lexer.TokenCHAR, lexer.TokenCHARACTER, lexer.TokenCLOB, lexer.TokenNCHAR, lexer.TokenNVARCHAR,
		lexer.TokenBLOB, lexer.TokenBOOLEAN,
		lexer.TokenDATE, lexer.TokenTIME, lexer.TokenTIMESTAMP, lexer.TokenDATETIME, lexer.TokenJSON, lexer.TokenJSONB:
		return true
	}
	return false
}

func (p *Parser) parseColumnConstraint() (*ColumnConstraint, bool, error) {
	constraint := &ColumnConstraint{}

	switch p.curToken.Type {
	case lexer.TokenPRIMARY:
		p.nextToken()
		if !p.curTokenIs(lexer.TokenKEY) {
			return nil, false, p.curError("expected KEY after PRIMARY")
		}
		constraint.Type = ConstraintPrimaryKey
		p.nextToken()
		if action, ok, err := p.parseOnConflictAction(); err != nil {
			return nil, false, err
		} else if ok {
			constraint.OnConflict = action
			constraint.HasOnConflict = true
		}

	case lexer.TokenNOT:
		p.nextToken()
		if !p.curTokenIs(lexer.TokenNULL) {
			return nil, false, p.curError("expected NULL after NOT")
		}
		constraint.Type = ConstraintNotNull
		p.nextToken()

	case lexer.TokenUNIQUE:
		constraint.Type = ConstraintUnique
		p.nextToken()
		if action, ok, err := p.parseOnConflictAction(); err != nil {
			return nil, false, err
		} else if ok {
			constraint.OnConflict = action
			constraint.HasOnConflict = true
		}

	case lexer.TokenDEFAULT:
		p.nextToken()
		// parseUnaryExpr accepts a signed numeric literal (-1, +5) and falls
		// through to primary expressions, including parenthesized expressions.
		// It stops before NOT/NULL, so a following NOT NULL constraint is left
		// for the enclosing constraint loop.
		expr, err := p.parseUnaryExpr()
		if err != nil {
			return nil, false, err
		}
		constraint.Type = ConstraintDefault
		constraint.Default = expr

	case lexer.TokenREFERENCES:
		p.nextToken()
		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, false, p.curError("expected table name after REFERENCES")
		}
		constraint.Type = ConstraintForeignKey
		constraint.RefTable = p.curToken.Literal
		p.nextToken()
		if p.curTokenIs(lexer.TokenLParen) {
			p.nextToken()
			if !p.curTokenIs(lexer.TokenIdent) {
				return nil, false, p.curError("expected column name")
			}
			constraint.RefColumn = p.curToken.Literal
			p.nextToken()
			if !p.curTokenIs(lexer.TokenRParen) {
				return nil, false, p.curError("expected )")
			}
			p.nextToken()
		}

	case lexer.TokenAUTOINCREMENT:
		constraint.Type = ConstraintAutoIncrement
		p.nextToken()

	case lexer.TokenCHECK:
		p.nextToken()
		if !p.curTokenIs(lexer.TokenLParen) {
			return nil, false, p.curError("expected ( after CHECK")
		}
		p.nextToken()
		check, err := p.parseExpr()
		if err != nil {
			return nil, false, err
		}
		if !p.curTokenIs(lexer.TokenRParen) {
			return nil, false, p.curError("expected ) after CHECK expression")
		}
		p.nextToken()
		constraint.Type = ConstraintCheck
		constraint.Check = check

	case lexer.TokenNULL:
		// Explicit NULL is a no-op: columns are nullable by default, so an
		// explicit NULL declaration carries no constraint. Consume it so
		// "col TYPE NULL" parses without inventing a fake nullable constraint.
		// NOT NULL is applied whenever it appears, regardless of ordering.
		p.nextToken()
		return nil, true, nil

	default:
		return nil, false, nil
	}

	return constraint, true, nil
}

// parseOnConflictAction parses an optional constraint clause of the form
// ON CONFLICT IGNORE|REPLACE|FAIL|ABORT|ROLLBACK. It reports ok=false and leaves
// the cursor unchanged when no ON CONFLICT follows.
func (p *Parser) parseOnConflictAction() (ConflictAction, bool, error) {
	if !p.curTokenIs(lexer.TokenON) {
		return ConflictAbort, false, nil
	}
	p.nextToken()
	if !p.curTokenIs(lexer.TokenCONFLICT) {
		return ConflictAbort, false, p.curError("expected CONFLICT after ON")
	}
	p.nextToken()
	switch p.curToken.Type {
	case lexer.TokenREPLACE:
		p.nextToken()
		return ConflictReplace, true, nil
	case lexer.TokenIGNORE:
		p.nextToken()
		return ConflictIgnore, true, nil
	case lexer.TokenFAIL:
		p.nextToken()
		return ConflictFail, true, nil
	case lexer.TokenABORT:
		p.nextToken()
		return ConflictAbort, true, nil
	case lexer.TokenROLLBACK:
		p.nextToken()
		return ConflictRollback, true, nil
	default:
		return ConflictAbort, false, p.curError("expected REPLACE, IGNORE, FAIL, ABORT, or ROLLBACK after ON CONFLICT")
	}
}

// consumeReferentialAction consumes one foreign-key referential action
// (CASCADE, RESTRICT, SET NULL, SET DEFAULT, NO ACTION). It is a no-op for the
// engine, which does not enforce foreign keys.
func (p *Parser) consumeReferentialAction() {
	if p.curTokenIs(lexer.TokenSET) {
		p.nextToken() // NULL or DEFAULT
		p.nextToken()
		return
	}
	if p.curTokenIs(lexer.TokenIdent) && strings.EqualFold(p.curToken.Literal, "NO") {
		p.nextToken()
		if p.curTokenIs(lexer.TokenIdent) {
			p.nextToken() // ACTION
		}
		return
	}
	p.nextToken() // CASCADE, RESTRICT, ...
}

func (p *Parser) parseTableConstraint() (*TableConstraint, error) {
	constraint := &TableConstraint{}

	// Check for CONSTRAINT name
	if p.curTokenIs(lexer.TokenCONSTRAINT) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, p.curError("expected constraint name")
		}
		constraint.Name = p.curToken.Literal
		p.nextToken()
	}

	switch p.curToken.Type {
	case lexer.TokenPRIMARY:
		p.nextToken()
		if !p.curTokenIs(lexer.TokenKEY) {
			return nil, p.curError("expected KEY after PRIMARY")
		}
		p.nextToken()
		constraint.Type = ConstraintPrimaryKey
		cols, err := p.parseParenIdentList()
		if err != nil {
			return nil, err
		}
		constraint.Columns = cols
		if action, ok, err := p.parseOnConflictAction(); err != nil {
			return nil, err
		} else if ok {
			constraint.OnConflict = action
			constraint.HasOnConflict = true
		}

	case lexer.TokenUNIQUE:
		p.nextToken()
		constraint.Type = ConstraintUnique
		cols, err := p.parseParenIdentList()
		if err != nil {
			return nil, err
		}
		constraint.Columns = cols
		if action, ok, err := p.parseOnConflictAction(); err != nil {
			return nil, err
		} else if ok {
			constraint.OnConflict = action
			constraint.HasOnConflict = true
		}

	case lexer.TokenFOREIGN:
		p.nextToken()
		if !p.curTokenIs(lexer.TokenKEY) {
			return nil, p.curError("expected KEY after FOREIGN")
		}
		p.nextToken()
		constraint.Type = ConstraintForeignKey
		cols, err := p.parseParenIdentList()
		if err != nil {
			return nil, err
		}
		constraint.Columns = cols

		if !p.curTokenIs(lexer.TokenREFERENCES) {
			return nil, p.curError("expected REFERENCES")
		}
		p.nextToken()
		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, p.curError("expected table name")
		}
		constraint.RefTable = p.curToken.Literal
		p.nextToken()
		refCols, err := p.parseParenIdentList()
		if err != nil {
			return nil, err
		}
		constraint.RefColumns = refCols

		// Consume and ignore referential actions (ON DELETE/UPDATE ...). The
		// engine does not enforce foreign keys, but the clause must parse.
		for p.curTokenIs(lexer.TokenON) {
			p.nextToken()
			if !p.curTokenIs(lexer.TokenDELETE) && !p.curTokenIs(lexer.TokenUPDATE) {
				return nil, p.curError("expected DELETE or UPDATE after ON")
			}
			p.nextToken()
			p.consumeReferentialAction()
		}

	case lexer.TokenCHECK:
		p.nextToken()
		constraint.Type = ConstraintCheck
		if !p.curTokenIs(lexer.TokenLParen) {
			return nil, p.curError("expected (")
		}
		p.nextToken()
		check, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		constraint.Check = check
		if !p.curTokenIs(lexer.TokenRParen) {
			return nil, p.curError("expected )")
		}
		p.nextToken()

	default:
		return nil, p.curError("expected constraint type")
	}

	return constraint, nil
}

func (p *Parser) parseParenIdentList() ([]string, error) {
	if !p.curTokenIs(lexer.TokenLParen) {
		return nil, p.curError("expected (")
	}
	p.nextToken()
	cols, err := p.parseIdentList()
	if err != nil {
		return nil, err
	}
	if !p.curTokenIs(lexer.TokenRParen) {
		return nil, p.curError("expected )")
	}
	p.nextToken()
	return cols, nil
}

// parseCreateIndex parses CREATE INDEX statements.
func (p *Parser) parseCreateIndex(unique bool) (*CreateIndexStmt, error) {
	stmt := &CreateIndexStmt{Unique: unique}

	p.nextToken() // consume INDEX

	// Check for IF NOT EXISTS
	if p.curTokenIs(lexer.TokenIF) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenNOT) {
			return nil, p.curError("expected NOT")
		}
		p.nextToken()
		if !p.curTokenIs(lexer.TokenEXISTS) {
			return nil, p.curError("expected EXISTS")
		}
		stmt.IfNotExists = true
		p.nextToken()
	}

	// Parse index name
	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected index name")
	}
	stmt.Name = p.curToken.Literal
	p.nextToken()

	// Expect ON
	if !p.curTokenIs(lexer.TokenON) {
		return nil, p.curError("expected ON")
	}
	p.nextToken()

	// Parse table name
	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected table name")
	}
	stmt.Table = p.curToken.Literal
	p.nextToken()

	// Expect (
	if !p.curTokenIs(lexer.TokenLParen) {
		return nil, p.curError("expected (")
	}
	p.nextToken()

	// Parse column list. Each element is either a bare column name or an
	// arbitrary deterministic expression (e.g. lower(email)).
	for {
		if p.curTokenIs(lexer.TokenRParen) {
			break
		}

		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		col := IndexColumn{}
		if ref, ok := expr.(*ColumnRef); ok && ref.Table == "" {
			col.Name = ref.Column
		} else {
			col.Expr = expr
			col.Name = FormatExpr(expr)
		}

		// Check for ASC/DESC
		if p.curTokenIs(lexer.TokenASC) {
			p.nextToken()
		} else if p.curTokenIs(lexer.TokenDESC) {
			col.Desc = true
			p.nextToken()
		}

		stmt.Columns = append(stmt.Columns, col)

		if p.curTokenIs(lexer.TokenComma) {
			p.nextToken()
		} else {
			break
		}
	}

	if !p.curTokenIs(lexer.TokenRParen) {
		return nil, p.curError("expected )")
	}
	p.nextToken()

	return stmt, nil
}

// parseDrop parses DROP statements.
func (p *Parser) parseDrop() (Statement, error) {
	p.nextToken() // consume DROP

	switch p.curToken.Type {
	case lexer.TokenTABLE:
		return p.parseDropTable()
	case lexer.TokenINDEX:
		return p.parseDropIndex()
	case lexer.TokenVIEW:
		return p.parseDropView()
	default:
		return nil, p.curError("expected TABLE or INDEX after DROP")
	}
}

func (p *Parser) parseDropTable() (*DropTableStmt, error) {
	stmt := &DropTableStmt{}

	p.nextToken() // consume TABLE

	// Check for IF EXISTS
	if p.curTokenIs(lexer.TokenIF) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenEXISTS) {
			return nil, p.curError("expected EXISTS")
		}
		stmt.IfExists = true
		p.nextToken()
	}

	// Parse table names
	for {
		table, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		stmt.Tables = append(stmt.Tables, table)

		if !p.curTokenIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}

	return stmt, nil
}

func (p *Parser) parseDropIndex() (*DropIndexStmt, error) {
	stmt := &DropIndexStmt{}

	p.nextToken() // consume INDEX

	// Check for IF EXISTS
	if p.curTokenIs(lexer.TokenIF) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenEXISTS) {
			return nil, p.curError("expected EXISTS")
		}
		stmt.IfExists = true
		p.nextToken()
	}

	// Parse index name
	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected index name")
	}
	stmt.Name = p.curToken.Literal
	p.nextToken()

	return stmt, nil
}

func (p *Parser) parseCreateView() (*CreateViewStmt, error) {
	stmt := &CreateViewStmt{}

	p.nextToken() // consume VIEW

	if p.curTokenIs(lexer.TokenIF) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenNOT) {
			return nil, p.curError("expected NOT")
		}
		p.nextToken()
		if !p.curTokenIs(lexer.TokenEXISTS) {
			return nil, p.curError("expected EXISTS")
		}
		stmt.IfNotExists = true
		p.nextToken()
	}

	// Parse view name directly — do NOT use parseTableRef here because it
	// greedily interprets the AS keyword as an alias, consuming "AS SELECT".
	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected view name")
	}
	stmt.View = &TableRef{Name: p.curToken.Literal}
	p.nextToken()

	if !p.curTokenIs(lexer.TokenAS) {
		return nil, p.curError("expected AS after view name")
	}
	p.nextToken() // consume AS

	sel, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	stmt.Select = sel

	return stmt, nil
}

func (p *Parser) parseDropView() (*DropViewStmt, error) {
	stmt := &DropViewStmt{}

	p.nextToken() // consume VIEW

	if p.curTokenIs(lexer.TokenIF) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenEXISTS) {
			return nil, p.curError("expected EXISTS")
		}
		stmt.IfExists = true
		p.nextToken()
	}

	for {
		view, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		stmt.Views = append(stmt.Views, view)

		if !p.curTokenIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}

	return stmt, nil
}

// parseAlter parses an ALTER statement.
func (p *Parser) parseAlter() (Statement, error) {
	p.nextToken() // consume ALTER

	if p.curTokenIs(lexer.TokenTABLE) {
		return p.parseAlterTable()
	}

	return nil, p.curError("expected TABLE after ALTER")
}

// parseAlterTable parses an ALTER TABLE statement.
func (p *Parser) parseAlterTable() (*AlterTableStmt, error) {
	stmt := &AlterTableStmt{}

	p.nextToken() // consume TABLE

	// Parse table name
	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected table name")
	}
	stmt.Table = p.curToken.Literal
	p.nextToken()

	// Parse action
	switch p.curToken.Type {
	case lexer.TokenADD:
		return p.parseAlterTableAdd(stmt)
	case lexer.TokenDROP:
		return p.parseAlterTableDrop(stmt)
	case lexer.TokenRENAME:
		return p.parseAlterTableRename(stmt)
	default:
		return nil, p.curError("expected ADD, DROP, or RENAME")
	}
}

// parseAlterTableAdd parses ALTER TABLE ADD COLUMN.
func (p *Parser) parseAlterTableAdd(stmt *AlterTableStmt) (*AlterTableStmt, error) {
	p.nextToken() // consume ADD

	// COLUMN keyword is optional
	if p.curTokenIs(lexer.TokenCOLUMN) {
		p.nextToken()
	}

	ifNotExists := false
	if p.curTokenIs(lexer.TokenIF) {
		ifNotExists = true
		p.nextToken()
		if !p.curTokenIs(lexer.TokenNOT) {
			return nil, p.curError("expected NOT after IF")
		}
		p.nextToken()
		if !p.curTokenIs(lexer.TokenEXISTS) {
			return nil, p.curError("expected EXISTS after IF NOT")
		}
		p.nextToken()
	}

	// Parse column definition
	col, err := p.parseColumnDef()
	if err != nil {
		return nil, err
	}

	stmt.Action = &AddColumnAction{Column: col, IfNotExists: ifNotExists}
	return stmt, nil
}

// parseAlterTableDrop parses ALTER TABLE DROP COLUMN.
func (p *Parser) parseAlterTableDrop(stmt *AlterTableStmt) (*AlterTableStmt, error) {
	p.nextToken() // consume DROP

	// COLUMN keyword is optional in some databases but required in SQLite
	if p.curTokenIs(lexer.TokenCOLUMN) {
		p.nextToken()
	}

	// Parse column name
	if !p.curIsName() {
		return nil, p.curError("expected column name")
	}

	stmt.Action = &DropColumnAction{Column: p.curToken.Literal}
	p.nextToken()

	return stmt, nil
}

// parseAlterTableRename parses ALTER TABLE RENAME.
func (p *Parser) parseAlterTableRename(stmt *AlterTableStmt) (*AlterTableStmt, error) {
	p.nextToken() // consume RENAME

	// Check for RENAME TO (table rename) or RENAME COLUMN (column rename)
	if p.curTokenIs(lexer.TokenTO) {
		// RENAME TO newname
		p.nextToken()
		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, p.curError("expected new table name")
		}
		stmt.Action = &RenameTableAction{NewName: p.curToken.Literal}
		p.nextToken()
	} else if p.curTokenIs(lexer.TokenCOLUMN) {
		// RENAME COLUMN oldname TO newname
		p.nextToken()
		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, p.curError("expected old column name")
		}
		oldName := p.curToken.Literal
		p.nextToken()

		if !p.curTokenIs(lexer.TokenTO) {
			return nil, p.curError("expected TO")
		}
		p.nextToken()

		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, p.curError("expected new column name")
		}
		newName := p.curToken.Literal
		p.nextToken()

		stmt.Action = &RenameColumnAction{OldName: oldName, NewName: newName}
	} else {
		return nil, p.curError("expected TO or COLUMN after RENAME")
	}

	return stmt, nil
}

// parsePragma parses a PRAGMA statement.
// Formats: PRAGMA name; PRAGMA name(arg); PRAGMA name = value;
func (p *Parser) parsePragma() (*PragmaStmt, error) {
	stmt := &PragmaStmt{}

	p.nextToken() // consume PRAGMA

	// Parse pragma name
	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected pragma name")
	}
	stmt.Name = strings.ToLower(p.curToken.Literal)
	p.nextToken()

	// Check for argument in parentheses: PRAGMA table_info(tablename)
	if p.curTokenIs(lexer.TokenLParen) {
		p.nextToken()
		if p.curTokenIs(lexer.TokenIdent) || p.curTokenIs(lexer.TokenString) {
			stmt.Arg = p.curToken.Literal
			p.nextToken()
		}
		if !p.curTokenIs(lexer.TokenRParen) {
			return nil, p.curError("expected )")
		}
		p.nextToken()
	}

	// Check for value assignment: PRAGMA name = value
	if p.curTokenIs(lexer.TokenEq) {
		p.nextToken()
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Value = val
	}

	return stmt, nil
}

// parseAnalyze parses ANALYZE [schema.]table. PizzaSQL does not maintain
// optimizer statistics, so the statement is accepted and ignored at execution.
func (p *Parser) parseAnalyze() (*AnalyzeStmt, error) {
	stmt := &AnalyzeStmt{}
	p.nextToken() // consume ANALYZE

	if p.curTokenIs(lexer.TokenIdent) {
		stmt.Name = p.curToken.Literal
		p.nextToken()
		if p.curTokenIs(lexer.TokenDot) {
			p.nextToken()
			if !p.curTokenIs(lexer.TokenIdent) {
				return nil, p.curError("expected table name after .")
			}
			stmt.Name = p.curToken.Literal
			p.nextToken()
		}
	}
	return stmt, nil
}

// parseExplain parses an EXPLAIN statement.
func (p *Parser) parseExplain() (*ExplainStmt, error) {
	stmt := &ExplainStmt{}

	p.nextToken() // consume EXPLAIN

	// Check for QUERY PLAN. PLAN is not a reserved lexer keyword so it can be a
	// column name; recognize it contextually here.
	if p.curTokenIs(lexer.TokenQUERY) {
		p.nextToken()
		if !p.curIdentIs("PLAN") {
			return nil, p.curError("expected PLAN after QUERY")
		}
		stmt.QueryPlan = true
		p.nextToken()
	}

	// Parse the statement being explained
	innerStmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	stmt.Statement = innerStmt

	return stmt, nil
}

// Transaction statement parsing

func (p *Parser) parseBegin() (*BeginStmt, error) {
	stmt := &BeginStmt{}

	p.nextToken() // consume BEGIN

	// Optional TRANSACTION keyword
	if p.curTokenIs(lexer.TokenTRANSACTION) {
		p.nextToken()
	}

	return stmt, nil
}

func (p *Parser) parseCommit() (*CommitStmt, error) {
	p.nextToken() // consume COMMIT

	// Optional TRANSACTION keyword
	if p.curTokenIs(lexer.TokenTRANSACTION) {
		p.nextToken()
	}

	return &CommitStmt{}, nil
}

func (p *Parser) parseRollback() (*RollbackStmt, error) {
	stmt := &RollbackStmt{}

	p.nextToken() // consume ROLLBACK

	// Check for ROLLBACK TO [SAVEPOINT] name
	if p.curTokenIs(lexer.TokenTO) {
		p.nextToken()
		// Optional SAVEPOINT keyword
		if p.curTokenIs(lexer.TokenSAVEPOINT) {
			p.nextToken()
		}
		if !p.curTokenIs(lexer.TokenIdent) {
			return nil, p.curError("expected savepoint name")
		}
		stmt.Savepoint = p.curToken.Literal
		p.nextToken()
	} else if p.curTokenIs(lexer.TokenTRANSACTION) {
		// Optional TRANSACTION keyword
		p.nextToken()
	}

	return stmt, nil
}

func (p *Parser) parseSavepoint() (*SavepointStmt, error) {
	p.nextToken() // consume SAVEPOINT

	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected savepoint name")
	}

	stmt := &SavepointStmt{Name: p.curToken.Literal}
	p.nextToken()

	return stmt, nil
}

func (p *Parser) parseRelease() (*ReleaseStmt, error) {
	p.nextToken() // consume RELEASE

	// Optional SAVEPOINT keyword
	if p.curTokenIs(lexer.TokenSAVEPOINT) {
		p.nextToken()
	}

	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected savepoint name")
	}

	stmt := &ReleaseStmt{Name: p.curToken.Literal}
	p.nextToken()

	return stmt, nil
}

// parseAttach parses an ATTACH DATABASE statement.
// Syntax: ATTACH [DATABASE] 'filepath' AS alias
func (p *Parser) parseAttach() (*AttachStmt, error) {
	stmt := &AttachStmt{}

	p.nextToken() // consume ATTACH

	// Optional DATABASE keyword
	if p.curTokenIs(lexer.TokenDATABASE) {
		p.nextToken()
	}

	// Parse file path (string literal)
	if !p.curTokenIs(lexer.TokenString) {
		return nil, p.curError("expected database file path (string)")
	}
	stmt.FilePath = p.curToken.Literal
	p.nextToken()

	// Expect AS keyword
	if !p.curTokenIs(lexer.TokenAS) {
		return nil, p.curError("expected AS")
	}
	p.nextToken()

	// Parse database alias
	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected database alias")
	}
	stmt.Alias = p.curToken.Literal
	p.nextToken()

	return stmt, nil
}

// parseDetach parses a DETACH DATABASE statement.
// Syntax: DETACH [DATABASE] alias
func (p *Parser) parseDetach() (*DetachStmt, error) {
	stmt := &DetachStmt{}

	p.nextToken() // consume DETACH

	// Optional DATABASE keyword
	if p.curTokenIs(lexer.TokenDATABASE) {
		p.nextToken()
	}

	// Parse database alias
	if !p.curTokenIs(lexer.TokenIdent) {
		return nil, p.curError("expected database alias")
	}
	stmt.Alias = p.curToken.Literal
	p.nextToken()

	return stmt, nil
}

// Expression parsing with operator precedence

func (p *Parser) parseExpr() (Expr, error) {
	return p.parseOrExpr()
}

func (p *Parser) parseOrExpr() (Expr, error) {
	left, err := p.parseAndExpr()
	if err != nil {
		return nil, err
	}

	for p.curTokenIs(lexer.TokenOR) {
		op := p.curToken.Type
		p.nextToken()
		right, err := p.parseAndExpr()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Left: left, Op: op, Right: right}
	}

	return left, nil
}

func (p *Parser) parseAndExpr() (Expr, error) {
	left, err := p.parseNotExpr()
	if err != nil {
		return nil, err
	}

	for p.curTokenIs(lexer.TokenAND) {
		op := p.curToken.Type
		p.nextToken()
		right, err := p.parseNotExpr()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Left: left, Op: op, Right: right}
	}

	return left, nil
}

func (p *Parser) parseNotExpr() (Expr, error) {
	if p.curTokenIs(lexer.TokenNOT) {
		p.nextToken()
		operand, err := p.parseNotExpr()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: lexer.TokenNOT, Operand: operand}, nil
	}

	return p.parseComparisonExpr()
}

func (p *Parser) parseComparisonExpr() (Expr, error) {
	left, err := p.parseBitwiseExpr()
	if err != nil {
		return nil, err
	}

	// Handle IS NULL / IS NOT NULL and IS [NOT] DISTINCT FROM.
	if p.curTokenIs(lexer.TokenIS) {
		p.nextToken()
		not := false
		if p.curTokenIs(lexer.TokenNOT) {
			not = true
			p.nextToken()
		}
		if p.curTokenIs(lexer.TokenDISTINCT) {
			p.nextToken()
			if !p.curTokenIs(lexer.TokenFROM) {
				return nil, p.curError("expected FROM after IS [NOT] DISTINCT")
			}
			p.nextToken()
			right, err := p.parseBitwiseExpr()
			if err != nil {
				return nil, err
			}
			return &IsDistinctExpr{Left: left, Right: right, Not: not}, nil
		}
		if !p.curTokenIs(lexer.TokenNULL) {
			return nil, p.curError("expected NULL or DISTINCT FROM after IS")
		}
		p.nextToken()
		return &IsNullExpr{Left: left, Not: not}, nil
	}

	// Handle IN / NOT IN
	not := false
	if p.curTokenIs(lexer.TokenNOT) {
		not = true
		p.nextToken()
	}

	if p.curTokenIs(lexer.TokenIN) {
		p.nextToken()
		return p.parseInExpr(left, not)
	}

	// Handle BETWEEN
	if p.curTokenIs(lexer.TokenBETWEEN) {
		p.nextToken()
		return p.parseBetweenExpr(left, not)
	}

	// Handle LIKE
	if p.curTokenIs(lexer.TokenLIKE) {
		p.nextToken()
		return p.parseLikeExpr(left, not)
	}

	// If we consumed NOT but didn't find IN/BETWEEN/LIKE, it's an error
	if not {
		return nil, p.curError("expected IN, BETWEEN, or LIKE after NOT")
	}

	// Handle comparison operators
	if isComparisonOp(p.curToken.Type) {
		op := p.curToken.Type
		p.nextToken()
		right, err := p.parseBitwiseExpr()
		if err != nil {
			return nil, err
		}
		return &BinaryExpr{Left: left, Op: op, Right: right}, nil
	}

	return left, nil
}

func isComparisonOp(t lexer.TokenType) bool {
	switch t {
	case lexer.TokenEq, lexer.TokenNeq, lexer.TokenLt,
		lexer.TokenLte, lexer.TokenGt, lexer.TokenGte:
		return true
	}
	return false
}

// parseBitwiseExpr parses the SQLite bitwise layer: << >> & | bind more tightly
// than comparisons but more loosely than + and -.
func (p *Parser) parseBitwiseExpr() (Expr, error) {
	left, err := p.parseAddExpr()
	if err != nil {
		return nil, err
	}

	for {
		switch p.curToken.Type {
		case lexer.TokenShiftLeft, lexer.TokenShiftRight, lexer.TokenBitAnd, lexer.TokenBitOr:
			op := p.curToken.Type
			p.nextToken()
			right, err := p.parseAddExpr()
			if err != nil {
				return nil, err
			}
			left = &BinaryExpr{Left: left, Op: op, Right: right}
		default:
			return left, nil
		}
	}
}

func (p *Parser) parseInExpr(left Expr, not bool) (Expr, error) {
	expr := &InExpr{Left: left, Not: not}

	if !p.curTokenIs(lexer.TokenLParen) {
		return nil, p.curError("expected (")
	}
	p.nextToken()

	// Check for subquery
	if p.curTokenIs(lexer.TokenSELECT) {
		sel, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		expr.Subquery = sel
	} else if !p.curTokenIs(lexer.TokenRParen) {
		// Value list (empty list is allowed — always false)
		values, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		expr.Values = values
	}

	if !p.curTokenIs(lexer.TokenRParen) {
		return nil, p.curError("expected )")
	}
	p.nextToken()

	return expr, nil
}

func (p *Parser) parseBetweenExpr(left Expr, not bool) (Expr, error) {
	low, err := p.parseBitwiseExpr()
	if err != nil {
		return nil, err
	}

	if !p.curTokenIs(lexer.TokenAND) {
		return nil, p.curError("expected AND in BETWEEN")
	}
	p.nextToken()

	high, err := p.parseBitwiseExpr()
	if err != nil {
		return nil, err
	}

	return &BetweenExpr{Left: left, Not: not, Low: low, High: high}, nil
}

func (p *Parser) parseLikeExpr(left Expr, not bool) (Expr, error) {
	pattern, err := p.parseBitwiseExpr()
	if err != nil {
		return nil, err
	}

	expr := &LikeExpr{Left: left, Not: not, Pattern: pattern}

	// Check for ESCAPE
	if p.curTokenIs(lexer.TokenESCAPE) {
		p.nextToken()
		esc, err := p.parseBitwiseExpr()
		if err != nil {
			return nil, err
		}
		expr.Escape = esc
	}

	return expr, nil
}

func (p *Parser) parseAddExpr() (Expr, error) {
	left, err := p.parseMulExpr()
	if err != nil {
		return nil, err
	}
	for p.curTokenIs(lexer.TokenPlus) || p.curTokenIs(lexer.TokenMinus) || p.curTokenIs(lexer.TokenConcat) {
		op := p.curToken.Type
		p.nextToken()
		right, err := p.parseMulExpr()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Left: left, Op: op, Right: right}
	}

	return left, nil
}

func (p *Parser) parseMulExpr() (Expr, error) {
	left, err := p.parseUnaryExpr()
	if err != nil {
		return nil, err
	}

	for p.curTokenIs(lexer.TokenStar) || p.curTokenIs(lexer.TokenSlash) || p.curTokenIs(lexer.TokenPercent) {
		op := p.curToken.Type
		p.nextToken()
		right, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Left: left, Op: op, Right: right}
	}

	return left, nil
}

func (p *Parser) parseUnaryExpr() (Expr, error) {
	if p.curTokenIs(lexer.TokenMinus) || p.curTokenIs(lexer.TokenPlus) || p.curTokenIs(lexer.TokenBitNot) {
		op := p.curToken.Type
		p.nextToken()
		operand, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: op, Operand: operand}, nil
	}

	return p.parsePrimaryExpr()
}

func (p *Parser) parsePrimaryExpr() (Expr, error) {
	switch p.curToken.Type {
	case lexer.TokenNumber:
		expr := &LiteralExpr{Type: lexer.TokenNumber, Value: p.curToken.Literal}
		p.nextToken()
		return expr, nil

	case lexer.TokenString:
		expr := &LiteralExpr{Type: lexer.TokenString, Value: p.curToken.Literal}
		p.nextToken()
		return expr, nil

	case lexer.TokenBlob:
		expr := &LiteralExpr{Type: lexer.TokenBlob, Value: p.curToken.Literal}
		p.nextToken()
		return expr, nil

	case lexer.TokenNULL:
		expr := &LiteralExpr{Type: lexer.TokenNULL, Value: "NULL"}
		p.nextToken()
		return expr, nil

	case lexer.TokenTRUE:
		expr := &LiteralExpr{Type: lexer.TokenTRUE, Value: "TRUE"}
		p.nextToken()
		return expr, nil

	case lexer.TokenFALSE:
		expr := &LiteralExpr{Type: lexer.TokenFALSE, Value: "FALSE"}
		p.nextToken()
		return expr, nil

	case lexer.TokenLParen:
		p.nextToken()
		// Check for subquery
		if p.curTokenIs(lexer.TokenSELECT) {
			sel, err := p.parseSelect()
			if err != nil {
				return nil, err
			}
			if !p.curTokenIs(lexer.TokenRParen) {
				return nil, p.curError("expected )")
			}
			p.nextToken()
			return &SubqueryExpr{Query: sel}, nil
		}
		// Regular parenthesized expression
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if !p.curTokenIs(lexer.TokenRParen) {
			return nil, p.curError("expected )")
		}
		p.nextToken()
		return &ParenExpr{Expr: expr}, nil

	case lexer.TokenCASE:
		return p.parseCaseExpr()

	case lexer.TokenCAST:
		return p.parseCastExpr()

	case lexer.TokenEXISTS:
		return p.parseExistsExpr()

	case lexer.TokenCOALESCE, lexer.TokenNULLIF, lexer.TokenIF, lexer.TokenREPLACE, lexer.TokenGLOB:
		// These keywords can be used as function names
		return p.parseKeywordFunction()

	case lexer.TokenJSON, lexer.TokenJSONB:
		// json(expr) / jsonb(expr) are JSON1 constructor functions. They lex as
		// data-type keywords but are also callable.
		return p.parseIdentOrFunction()

	case lexer.TokenDATE, lexer.TokenTIME, lexer.TokenTIMESTAMP, lexer.TokenDATETIME:
		// Date/time functions (date(), time(), datetime(), ...) lex as data-type
		// keywords but are callable.
		return p.parseIdentOrFunction()

	case lexer.TokenKEY:
		// KEY is a lexer keyword (PRIMARY KEY) that SQLite also permits as a
		// column name, e.g. store.key.
		return p.parseIdentOrFunction()

	case lexer.TokenIdent:
		return p.parseIdentOrFunction()

	case lexer.TokenStar:
		// For COUNT(*)
		expr := &LiteralExpr{Type: lexer.TokenStar, Value: "*"}
		p.nextToken()
		return expr, nil

	default:
		return nil, p.curError("unexpected token in expression: " + p.curToken.Type.String())
	}
}

func (p *Parser) parseIdentOrFunction() (Expr, error) {
	name := p.curToken.Literal
	p.nextToken()

	// Check for function call
	if p.curTokenIs(lexer.TokenLParen) {
		return p.parseFunctionCall(name)
	}

	// Check for table.column
	if p.curTokenIs(lexer.TokenDot) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenIdent) && !p.curTokenIs(lexer.TokenStar) {
			return nil, p.curError("expected column name after dot")
		}
		col := p.curToken.Literal
		p.nextToken()
		return &ColumnRef{Table: name, Column: col}, nil
	}

	return &ColumnRef{Column: name}, nil
}

func (p *Parser) parseKeywordFunction() (Expr, error) {
	// Handle keywords that can be used as function names (COALESCE, NULLIF, IF, REPLACE, GLOB)
	name := strings.ToUpper(p.curToken.Literal)
	p.nextToken()

	if !p.curTokenIs(lexer.TokenLParen) {
		return nil, p.curError("expected ( after " + name)
	}

	return p.parseFunctionCall(name)
}

func (p *Parser) parseFunctionCall(name string) (Expr, error) {
	fn := &FunctionCall{Name: strings.ToUpper(name)}

	p.nextToken() // consume (

	// Check for DISTINCT or ALL
	if p.curTokenIs(lexer.TokenDISTINCT) {
		fn.Distinct = true
		p.nextToken()
	} else if p.curTokenIs(lexer.TokenALL) {
		// ALL is the default behavior, just skip it
		p.nextToken()
	}

	// Check for * (COUNT(*))
	if p.curTokenIs(lexer.TokenStar) {
		fn.Star = true
		p.nextToken()
	} else if !p.curTokenIs(lexer.TokenRParen) {
		// Parse arguments
		args, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		fn.Args = args
	}

	if !p.curTokenIs(lexer.TokenRParen) {
		return nil, p.curError("expected )")
	}
	p.nextToken()

	// Window function: func(...) OVER (PARTITION BY ... ORDER BY ...).
	if p.curTokenIs(lexer.TokenIdent) && strings.EqualFold(p.curToken.Literal, "OVER") {
		return p.parseWindowSpec(fn)
	}

	return fn, nil
}

// parseWindowSpec parses the OVER (...) clause of a window function. Only
// PARTITION BY and ORDER BY are supported; frame clauses are not.
func (p *Parser) parseWindowSpec(fn *FunctionCall) (Expr, error) {
	p.nextToken() // consume OVER
	if !p.curTokenIs(lexer.TokenLParen) {
		return nil, p.curError("expected ( after OVER")
	}
	p.nextToken()

	window := &WindowExpr{Func: fn}

	if p.curTokenIs(lexer.TokenIdent) && strings.EqualFold(p.curToken.Literal, "PARTITION") {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenBY) {
			return nil, p.curError("expected BY after PARTITION")
		}
		p.nextToken()
		exprs, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		window.PartitionBy = exprs
	}

	if p.curTokenIs(lexer.TokenORDER) {
		p.nextToken()
		if !p.curTokenIs(lexer.TokenBY) {
			return nil, p.curError("expected BY after ORDER")
		}
		p.nextToken()
		orderBy, err := p.parseOrderBy()
		if err != nil {
			return nil, err
		}
		window.OrderBy = orderBy
	}

	if !p.curTokenIs(lexer.TokenRParen) {
		return nil, p.curError("expected ) after window specification")
	}
	p.nextToken()
	return window, nil
}

func (p *Parser) parseCaseExpr() (Expr, error) {
	expr := &CaseExpr{}

	p.nextToken() // consume CASE

	// Check for simple CASE (CASE operand WHEN ...)
	if !p.curTokenIs(lexer.TokenWHEN) {
		operand, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		expr.Operand = operand
	}

	// Parse WHEN clauses
	for p.curTokenIs(lexer.TokenWHEN) {
		p.nextToken()
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if !p.curTokenIs(lexer.TokenTHEN) {
			return nil, p.curError("expected THEN")
		}
		p.nextToken()
		result, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		expr.Whens = append(expr.Whens, WhenClause{Condition: cond, Result: result})
	}

	// Parse optional ELSE
	if p.curTokenIs(lexer.TokenELSE) {
		p.nextToken()
		elseExpr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		expr.Else = elseExpr
	}

	// Expect END
	if !p.curTokenIs(lexer.TokenEND) {
		return nil, p.curError("expected END")
	}
	p.nextToken()

	return expr, nil
}

func (p *Parser) parseCastExpr() (Expr, error) {
	p.nextToken() // consume CAST

	if !p.curTokenIs(lexer.TokenLParen) {
		return nil, p.curError("expected (")
	}
	p.nextToken()

	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	if !p.curTokenIs(lexer.TokenAS) {
		return nil, p.curError("expected AS")
	}
	p.nextToken()

	dataType, err := p.parseDataType()
	if err != nil {
		return nil, err
	}

	if !p.curTokenIs(lexer.TokenRParen) {
		return nil, p.curError("expected )")
	}
	p.nextToken()

	return &CastExpr{Expr: expr, Type: *dataType}, nil
}

func (p *Parser) parseExistsExpr() (Expr, error) {
	p.nextToken() // consume EXISTS

	if !p.curTokenIs(lexer.TokenLParen) {
		return nil, p.curError("expected (")
	}
	p.nextToken()

	if !p.curTokenIs(lexer.TokenSELECT) {
		return nil, p.curError("expected SELECT in EXISTS")
	}

	sel, err := p.parseSelect()
	if err != nil {
		return nil, err
	}

	if !p.curTokenIs(lexer.TokenRParen) {
		return nil, p.curError("expected )")
	}
	p.nextToken()

	return &ExistsExpr{Subquery: sel}, nil
}

func (p *Parser) parseExprList() ([]Expr, error) {
	var exprs []Expr

	for {
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, expr)

		if !p.curTokenIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}

	return exprs, nil
}

func (p *Parser) parseIdentList() ([]string, error) {
	var idents []string

	for {
		if !p.curIsName() {
			return nil, p.curError("expected identifier")
		}
		idents = append(idents, p.curToken.Literal)
		p.nextToken()

		if !p.curTokenIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}

	return idents, nil
}

// curIsName reports whether the current token can serve as a name (column or
// table identifier). A few lexer keywords, notably KEY, are commonly used as
// column names in the GoatCounter schema and SQLite allows them.
func (p *Parser) curIsName() bool {
	if p.curTokenIs(lexer.TokenIdent) {
		return true
	}
	switch p.curToken.Type {
	case lexer.TokenKEY:
		return true
	}
	return false
}

// Helper functions

func parseInt(s string) int {
	var n int
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
