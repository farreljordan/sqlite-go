package sqlparser

import "fmt"

func Parse(sql string) (Statement, error) {
	p := &parser{t: NewTokenizer(sql)}
	p.advance() // prime the lookahead
	return p.parseStatement()
}

type parser struct {
	t       *Tokenizer
	current Token
}

// advance consumes the current token and reads the next one.
func (p *parser) advance() {
	p.current = p.t.Next()
}

// expect asserts the current token is of the given type, advances, and
func (p *parser) expect(tt TokenType) (Token, error) {
	tok := p.current
	if tok.Type != tt {
		return tok, fmt.Errorf("expected token %d, got %q (%d)", tt, tok.Val, tok.Type)
	}
	p.advance()
	return tok, nil
}

// peek returns the current token type without consuming it.
func (p *parser) peek() TokenType {
	return p.current.Type
}

// parseStatement dispatches to the correct statement parser.
func (p *parser) parseStatement() (Statement, error) {
	switch p.peek() {
	case TOKEN_SELECT:
		return p.parseSelect()
	default:
		return nil, fmt.Errorf("unsupported statement starting with %q", p.current.Val)
	}
}

// parseSelect handles: SELECT <selectExprs> FROM <tableName> WHERE
func (p *parser) parseSelect() (*Select, error) {
	p.advance() // consume SELECT

	exprs, err := p.parseSelectExprs()
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(TOKEN_FROM); err != nil {
		return nil, err
	}

	table, err := p.parseTableName()
	if err != nil {
		return nil, err
	}

	var where *WhereClause

	if p.peek() == TOKEN_WHERE {
		p.advance()

		col, err := p.expect(TOKEN_IDENT)
		if err != nil {
			return nil, fmt.Errorf("expected column in WHERE")
		}

		if _, err := p.expect(TOKEN_EQ); err != nil {
			return nil, fmt.Errorf("only '=' supported in WHERE")
		}

		val := p.current
		if val.Type != TOKEN_STRING && val.Type != TOKEN_IDENT {
			return nil, fmt.Errorf("expected value in WHERE")
		}
		p.advance()

		where = &WhereClause{
			Column: col.Val,
			Value:  val.Val,
		}
	}

	return &Select{
		SelectExprs: exprs,
		From:        table,
		Where:       where,
	}, nil
}

// parseSelectExprs parses a comma-separated list of select expressions.
func (p *parser) parseSelectExprs() ([]SelectExpr, error) {
	var exprs []SelectExpr

	expr, err := p.parseSelectExpr()
	if err != nil {
		return nil, err
	}
	exprs = append(exprs, expr)

	for p.peek() == TOKEN_COMMA {
		p.advance() // consume ','
		expr, err := p.parseSelectExpr()
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, expr)
	}

	return exprs, nil
}

// parseSelectExpr parses one item: *, col, or FUNC(*).
func (p *parser) parseSelectExpr() (SelectExpr, error) {
	// bare star
	if p.peek() == TOKEN_STAR {
		p.advance()
		return &StarExpr{}, nil
	}

	// must be an identifier (column name or function name)
	tok, err := p.expect(TOKEN_IDENT)
	if err != nil {
		return nil, fmt.Errorf("expected column name or *, got %q", p.current.Val)
	}

	// function call: IDENT '(' ... ')'
	if p.peek() == TOKEN_LPAREN {
		p.advance() // consume '('
		arg, err := p.parseFuncArg()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(TOKEN_RPAREN); err != nil {
			return nil, err
		}
		return &FuncExpr{Name: tok.Val, Arg: arg}, nil
	}

	return &ColExpr{Name: tok.Val}, nil
}

// parseFuncArg reads the single argument inside a function call.
// We only support * or a column name for now.
func (p *parser) parseFuncArg() (string, error) {
	switch p.peek() {
	case TOKEN_STAR:
		p.advance()
		return "*", nil
	case TOKEN_IDENT:
		tok := p.current
		p.advance()
		return tok.Val, nil
	default:
		return "", fmt.Errorf("unsupported function argument %q", p.current.Val)
	}
}

// parseTableName reads the table name after FROM.
func (p *parser) parseTableName() (TableName, error) {
	tok, err := p.expect(TOKEN_IDENT)
	if err != nil {
		return TableName{}, fmt.Errorf("expected table name, got %q", p.current.Val)
	}
	return TableName{Name: tok.Val}, nil
}
