package formula

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"strings"
)

type Node interface {
	Pos() token.Pos
	End() token.Pos
}

type Expr interface {
	Node
	Calculate(ctx context.Context) (interface{}, error)
}

type RefExpr struct {
	Postion token.Pos `json:"pos"`
	Name    string    `json:"name"`
}

func (expr *RefExpr) Calculate(ctx context.Context) (interface{}, error) {
	return ctx.Value(expr.Name), nil
}

func (expr *RefExpr) Pos() token.Pos {
	return expr.Postion
}

func (expr *RefExpr) End() token.Pos {
	return expr.Postion + token.Pos(len(expr.Name)) + 1
}

func (expr *RefExpr) String() string {
	return fmt.Sprintf(("{%s}"), expr.Name)
}

func (expr *RefExpr) Valid() error {
	return nil
}

type ConstExpr struct {
	Position token.Pos `json:"pos"`
	Value    float64   `json:"value"`
	Src      string    `json:"src"`
}

func (expr *ConstExpr) Calculate(ctx context.Context) (interface{}, error) {
	return expr.Value, nil
}

func (expr *ConstExpr) String() string {
	value := fmt.Sprintf("%f", expr.Value)
	return strings.TrimRight(strings.TrimRight(value, "0"), ".")
}

func (expr *ConstExpr) Pos() token.Pos {
	return expr.Position
}

func (expr *ConstExpr) End() token.Pos {
	return expr.Position + token.Pos(len(expr.Src))
}

type BinaryExpr struct {
	X  Expr
	Y  Expr
	Op token.Token
}

func (expr *BinaryExpr) Calculate(ctx context.Context) (interface{}, error) {
	x, err := expr.X.Calculate(ctx)
	if err != nil {
		return nil, err
	}
	y, err := expr.Y.Calculate(ctx)
	if err != nil {
		return nil, err
	}

	if x == nil || y == nil {
		return nil, nil
	}

	X, err := convertToFloat(x)
	if err != nil {
		return nil, errors.New("参数类型错误")
	}
	Y, err := convertToFloat(y)
	if err != nil {
		return nil, errors.New("参数类型错误")
	}

	switch expr.Op {
	case token.ADD:
		return X + Y, nil
	case token.SUB:
		return X - Y, nil
	case token.MUL:
		return X * Y, nil
	case token.QUO:
		if Y == 0 {
			return nil, errors.New("被除数不能为0")
		}
		return X / Y, nil
	default:
		return 0, errors.New("不支持的操作符")
	}
}

func (expr BinaryExpr) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"x":  expr.X,
		"y":  expr.Y,
		"op": expr.Op.String(),
	})
}

func (expr *BinaryExpr) String() string {
	return fmt.Sprintf("(%s%s%s)", expr.X, expr.Op, expr.Y)
}

func (expr *BinaryExpr) Pos() token.Pos {
	return expr.X.Pos()
}

func (expr *BinaryExpr) End() token.Pos {
	return expr.Y.End()
}

func convertToFloat(data interface{}) (float64, error) {
	switch result := data.(type) {
	case int:
		return float64(result), nil
	case float64:
		return result, nil
	default:
		return 0, errors.New("类型转换错误")
	}
}

type CallerExpr struct {
	Name string
	Args []Expr
	pos  token.Pos
}

func (expr *CallerExpr) Calculate(ctx context.Context) (interface{}, error) {
	fn := fns[expr.Name]
	if fn == nil {
		return nil, errors.New("函数不存在")
	}

	args := make([]interface{}, len(expr.Args), len(expr.Args))
	for i, expr := range expr.Args {
		if result, err := expr.Calculate(ctx); err != nil {
			return nil, err
		} else {
			args[i] = result
		}
	}
	return fn.Calculate(args)
}

func (expr *CallerExpr) Pos() token.Pos {
	return expr.pos
}

func (expr *CallerExpr) End() token.Pos {
	if len(expr.Args) == 0 {
		return expr.pos + 2
	}
	return expr.Args[len(expr.Args)-1].End() + 1
}

func (expr *CallerExpr) String() string {
	sb := strings.Builder{}
	sb.WriteString(expr.Name)
	sb.WriteRune('(')
	for i, arg := range expr.Args {
		sb.WriteString(fmt.Sprintf("%s", arg))
		if i != len(expr.Args)-1 {
			sb.WriteRune(',')
		}
	}
	sb.WriteRune(')')
	return sb.String()
}

type GroupExpr struct {
	Expr
}

func (expr GroupExpr) MarshalJSON() ([]byte, error) {
	return json.Marshal(expr.Expr)
}

type ExprGroup struct {
	expr Expr
	cur  *BinaryExpr
}

func (grp *ExprGroup) Valid() error {
	if grp.expr == nil {
		return errors.New("formular is not completed 1")
	}
	if b, ok := grp.expr.(*BinaryExpr); ok {
		if b.X == nil || b.Y == nil {
			return errors.New("formular is not completed 2")
		}
	}
	return nil
}

func (grp *ExprGroup) Expr() Expr {
	return &GroupExpr{
		Expr: grp.expr,
	}
}

func (grp *ExprGroup) AddOperator(token token.Token) error {
	if grp.expr == nil {
		return errors.New("operator should not be in front of express")
	}
	if grp.cur != nil && grp.cur.Y == nil {
		return errors.New("expression para is not valid,O0")
	}
	grp.expr = grp.getExpr(grp.expr, token)
	return nil
}

func (grp *ExprGroup) AddExpr(expr Expr) error {
	if grp.expr == nil {
		grp.expr = expr
	} else {
		if grp.cur == nil {
			return errors.New("expression para is not valid,A0")
		}
		if grp.cur.Y != nil {
			return errors.New("expression para is not valid,A1")
		}
		grp.cur.Y = expr
	}
	return nil
}

func (grp *ExprGroup) getExpr(expr Expr, token token.Token) *BinaryExpr {
	if b, ok := expr.(*BinaryExpr); ok {
		if token.Precedence() > b.Op.Precedence() {
			b.Y = grp.getExpr(b.Y, token)
			return b
		}
	}
	result := &BinaryExpr{
		X:  expr,
		Op: token,
	}
	grp.cur = result
	return result
}
