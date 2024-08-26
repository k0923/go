package formula

import (
	"errors"
	"fmt"
	"go/token"
	"strconv"
)

type ParserError struct {
	pos token.Pos
	end token.Pos
	err error
}

func (err *ParserError) Error() string {
	msg := "unknow error"
	if err.err != nil {
		msg = err.err.Error()
	}
	return fmt.Sprintf("%s,pos:%v,end:%v", msg, err.pos, err.end)
}

func ParseExpr(expression string) (Expr, error) {
	p := &Parser{
		scanner: NewFormulaScanner(expression),
	}
	return p.scanGroup(token.EOF, false)
}

type Parser struct {
	pos     token.Pos
	tok     token.Token
	lit     string
	scanner *Scanner
}

func (parser *Parser) next() {
	parser.pos, parser.tok, parser.lit = parser.scanner.Scan()
}

func (parser *Parser) scanRef() (*RefExpr, error) {
	pos := parser.pos
	parser.next()
	name := parser.lit
	parser.next()
	if parser.tok != token.RBRACE {
		return nil, fmt.Errorf("variable %s is not valid", name)
	}
	return &RefExpr{
		Postion: pos,
		Name:    name,
	}, nil
}

func (parser *Parser) scanConst() (*ConstExpr, error) {
	result, err := strconv.ParseFloat(string(parser.lit), 64)
	if err != nil {
		return nil, fmt.Errorf("connot convert:%s to number", parser.lit)
	}

	return &ConstExpr{
		Position: parser.pos - token.Pos(len(parser.lit)),
		Value:    result,
		Src:      parser.lit,
	}, nil
}

func (parser *Parser) scanGroup(end token.Token, isParen bool) (Expr, error) {
	group := &ExprGroup{}
	var pos token.Pos
	var parserErr error
loop:
	for {
		parser.next()
		pos = parser.pos
		if parser.tok == end {
			break loop
		}
		switch {
		case parser.tok.IsLiteral():
			if cst, err := parser.scanConst(); err != nil {
				parserErr = err
				break loop
			} else if err := group.AddExpr(cst); err != nil {
				parserErr = err
				break loop
			}
			break
		case parser.tok.IsOperator():
			if err := group.AddOperator(parser.tok); err != nil {
				parserErr = err
				break loop
			}
			break
		default:
			switch parser.tok {
			case token.IDENT:
				if fn, err := parser.scanFn(); err != nil {
					parserErr = err
					break loop
				} else if err := group.AddExpr(fn); err != nil {
					parserErr = err
					break loop
				}
			case token.LBRACE:
				if ref, err := parser.scanRef(); err != nil {
					parserErr = err
					break loop
				} else {
					if err := ref.Valid(); err != nil {
						parserErr = err
						break loop
					}
					if err := group.AddExpr(ref); err != nil {
						parserErr = err
						break loop
					}
				}
			case token.LPAREN:
				if grp, err := parser.scanGroup(token.RPAREN, true); err != nil {
					parserErr = err
					break loop
				} else if err := group.AddExpr(grp); err != nil {
					parserErr = err
					break loop
				}
			case token.RPAREN:
				break loop
			case token.EOF:
				parserErr = errors.New("expression is not completed")
				break loop
			default:
				parserErr = fmt.Errorf("unsupported token:%s", parser.tok)
			}
		}
	}

	if parserErr != nil {
		return nil, &ParserError{
			pos: pos,
			end: parser.pos,
			err: parserErr,
		}
	}
	if err := group.Valid(); err != nil {
		return nil, &ParserError{
			pos: pos,
			end: parser.pos,
			err: err,
		}
	}
	return group.Expr(), nil
}

func (parser *Parser) scanFn() (*CallerExpr, error) {
	fnName := parser.lit
	parser.next()
	if parser.tok != token.LPAREN {
		return nil, errors.New("there should be a left lparen after function name")
	}

	expr := &CallerExpr{
		Name: fnName,
		Args: make([]Expr, 0),
	}

	fn := fns[fnName]
	if fn == nil {
		return nil, fmt.Errorf("function:%s is not existed", fnName)
	}
loop:
	for {
		if grp, err := parser.scanGroup(token.COMMA, false); err != nil {
			return nil, err
		} else {
			if grp != nil {
				expr.Args = append(expr.Args, grp)
			}
		}
		switch parser.tok {
		case token.RPAREN:
			break loop
		case token.COMMA:
			continue
		default:
			return nil, errors.New("function is not completed")
		}
	}

	if err := fn.Valid(expr.Args); err != nil {
		return nil, err
	}

	return expr, nil

}
