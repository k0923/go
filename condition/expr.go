package condition

// Expr computes a V from a T. It replaces the old Picker abstraction.
type Expr[T, V any] interface {
	Eval(v T) (V, error)
}

// ExprFunc adapts a fallible function to Expr.
type ExprFunc[T, V any] func(T) (V, error)

func (f ExprFunc[T, V]) Eval(v T) (V, error) { return f(v) }

type constExpr[T, V any] struct{ v V }

func (c constExpr[T, V]) Eval(T) (V, error) { return c.v, nil }

// Const returns an Expr that always yields v. Operators recognise constants
// and evaluate them inline, without an interface call.
func Const[T, V any](v V) Expr[T, V] { return constExpr[T, V]{v} }

type fieldExpr[T, V any] func(T) V

func (f fieldExpr[T, V]) Eval(v T) (V, error) { return f(v), nil }

// Field wraps an infallible accessor. It panics if get is nil.
func Field[T, V any](get func(T) V) Expr[T, V] {
	if get == nil {
		panic("condition: nil accessor passed to Field")
	}
	return fieldExpr[T, V](get)
}

// operand caches a constant so hot paths skip the interface call.
type operand[T, V any] struct {
	expr    Expr[T, V]
	val     V
	isConst bool
}

func newOperand[T, V any](e Expr[T, V], role string) operand[T, V] {
	switch e := e.(type) {
	case nil:
		panic("condition: nil " + role + " operand")
	case constExpr[T, V]:
		return operand[T, V]{val: e.v, isConst: true}
	}
	return operand[T, V]{expr: e}
}

func (o *operand[T, V]) eval(v T) (V, error) {
	if o.isConst {
		return o.val, nil
	}
	return o.expr.Eval(v)
}

// Number is the set of types supported by the arithmetic operators.
type Number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr |
		~float32 | ~float64
}

type arithOp uint8

const (
	opAdd arithOp = iota
	opSub
	opMul
	opDiv
)

type arithExpr[T any, N Number] struct {
	op   arithOp
	x, y operand[T, N]
}

func (e *arithExpr[T, N]) Eval(v T) (N, error) {
	a, err := e.x.eval(v)
	if err != nil {
		return 0, err
	}
	b, err := e.y.eval(v)
	if err != nil {
		return 0, err
	}
	switch e.op {
	case opAdd:
		return a + b, nil
	case opSub:
		return a - b, nil
	case opMul:
		return a * b, nil
	default:
		if b == 0 {
			return 0, ErrDivideByZero
		}
		return a / b, nil
	}
}

// Add returns x + y.
func Add[T any, N Number](x, y Expr[T, N]) Expr[T, N] { return newArith(opAdd, x, y) }

// Sub returns x - y.
func Sub[T any, N Number](x, y Expr[T, N]) Expr[T, N] { return newArith(opSub, x, y) }

// Mul returns x * y.
func Mul[T any, N Number](x, y Expr[T, N]) Expr[T, N] { return newArith(opMul, x, y) }

// Div returns x / y, or ErrDivideByZero when y evaluates to zero.
func Div[T any, N Number](x, y Expr[T, N]) Expr[T, N] { return newArith(opDiv, x, y) }

func newArith[T any, N Number](op arithOp, x, y Expr[T, N]) Expr[T, N] {
	e := &arithExpr[T, N]{op: op, x: newOperand(x, "left"), y: newOperand(y, "right")}
	if e.x.isConst && e.y.isConst {
		var zero T
		// A constant division by zero is kept so the error surfaces at Eval.
		if r, err := e.Eval(zero); err == nil {
			return constExpr[T, N]{r}
		}
	}
	return e
}

// Len yields the length of a slice.
func Len[T, E any](items Expr[T, []E]) Expr[T, int] {
	o := newOperand(items, "list")
	if o.isConst {
		return constExpr[T, int]{len(o.val)}
	}
	return ExprFunc[T, int](func(v T) (int, error) {
		s, err := o.expr.Eval(v)
		return len(s), err
	})
}
