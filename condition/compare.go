package condition

import (
	"cmp"
	"slices"
	"strings"
)

// foldConst evaluates c once if all its operands are constant.
func foldConst[T any](c Condition[T], allConst bool) Condition[T] {
	if !allConst {
		return c
	}
	var zero T
	ok, err := c.Match(zero)
	if err != nil {
		return c
	}
	return constCond[T](ok)
}

type eqCond[T any, V comparable] struct {
	neg  bool
	x, y operand[T, V]
}

func (c *eqCond[T, V]) Match(v T) (bool, error) {
	a, err := c.x.eval(v)
	if err != nil {
		return false, err
	}
	b, err := c.y.eval(v)
	if err != nil {
		return false, err
	}
	return (a == b) != c.neg, nil
}

// Eq matches when x == y.
func Eq[T any, V comparable](x, y Expr[T, V]) Condition[T] { return newEq(false, x, y) }

// Ne matches when x != y.
func Ne[T any, V comparable](x, y Expr[T, V]) Condition[T] { return newEq(true, x, y) }

func newEq[T any, V comparable](neg bool, x, y Expr[T, V]) Condition[T] {
	c := &eqCond[T, V]{neg: neg, x: newOperand(x, "left"), y: newOperand(y, "right")}
	return foldConst(c, c.x.isConst && c.y.isConst)
}

type ordOp uint8

const (
	opLt ordOp = iota
	opLe
	opGt
	opGe
)

type ordCond[T any, V cmp.Ordered] struct {
	op   ordOp
	x, y operand[T, V]
}

func (c *ordCond[T, V]) Match(v T) (bool, error) {
	a, err := c.x.eval(v)
	if err != nil {
		return false, err
	}
	b, err := c.y.eval(v)
	if err != nil {
		return false, err
	}
	switch c.op {
	case opLt:
		return a < b, nil
	case opLe:
		return a <= b, nil
	case opGt:
		return a > b, nil
	default:
		return a >= b, nil
	}
}

// Lt matches when x < y. Comparisons involving NaN are false.
func Lt[T any, V cmp.Ordered](x, y Expr[T, V]) Condition[T] { return newOrd(opLt, x, y) }

// Le matches when x <= y.
func Le[T any, V cmp.Ordered](x, y Expr[T, V]) Condition[T] { return newOrd(opLe, x, y) }

// Gt matches when x > y.
func Gt[T any, V cmp.Ordered](x, y Expr[T, V]) Condition[T] { return newOrd(opGt, x, y) }

// Ge matches when x >= y.
func Ge[T any, V cmp.Ordered](x, y Expr[T, V]) Condition[T] { return newOrd(opGe, x, y) }

func newOrd[T any, V cmp.Ordered](op ordOp, x, y Expr[T, V]) Condition[T] {
	c := &ordCond[T, V]{op: op, x: newOperand(x, "left"), y: newOperand(y, "right")}
	return foldConst(c, c.x.isConst && c.y.isConst)
}

type strOp uint8

const (
	opContains strOp = iota
	opPrefix
	opSuffix
)

type strCond[T any, S ~string] struct {
	op   strOp
	x, y operand[T, S]
}

func (c *strCond[T, S]) Match(v T) (bool, error) {
	a, err := c.x.eval(v)
	if err != nil {
		return false, err
	}
	b, err := c.y.eval(v)
	if err != nil {
		return false, err
	}
	switch c.op {
	case opContains:
		return strings.Contains(string(a), string(b)), nil
	case opPrefix:
		return strings.HasPrefix(string(a), string(b)), nil
	default:
		return strings.HasSuffix(string(a), string(b)), nil
	}
}

// Contains matches when sub is within s.
func Contains[T any, S ~string](s, sub Expr[T, S]) Condition[T] { return newStr(opContains, s, sub) }

// HasPrefix matches when s begins with prefix.
func HasPrefix[T any, S ~string](s, prefix Expr[T, S]) Condition[T] {
	return newStr(opPrefix, s, prefix)
}

// HasSuffix matches when s ends with suffix.
func HasSuffix[T any, S ~string](s, suffix Expr[T, S]) Condition[T] {
	return newStr(opSuffix, s, suffix)
}

func newStr[T any, S ~string](op strOp, x, y Expr[T, S]) Condition[T] {
	c := &strCond[T, S]{op: op, x: newOperand(x, "left"), y: newOperand(y, "right")}
	return foldConst(c, c.x.isConst && c.y.isConst)
}

// linearSetMax is the largest set scanned linearly; bigger sets use a map.
// For a handful of elements a scan beats hashing.
const linearSetMax = 8

type inCond[T any, V comparable] struct {
	neg  bool
	x    operand[T, V]
	list []V
	set  map[V]struct{}
}

func (c *inCond[T, V]) Match(v T) (bool, error) {
	a, err := c.x.eval(v)
	if err != nil {
		return false, err
	}
	var found bool
	if c.set != nil {
		_, found = c.set[a]
	} else {
		found = slices.Contains(c.list, a)
	}
	return found != c.neg, nil
}

// In matches when x equals one of values. The set is built once, so
// membership is O(1) for large sets. In with no values is False.
func In[T any, V comparable](x Expr[T, V], values ...V) Condition[T] {
	return newIn(false, x, values)
}

// NotIn matches when x equals none of values. NotIn with no values is True.
func NotIn[T any, V comparable](x Expr[T, V], values ...V) Condition[T] {
	return newIn(true, x, values)
}

func newIn[T any, V comparable](neg bool, x Expr[T, V], values []V) Condition[T] {
	c := &inCond[T, V]{neg: neg, x: newOperand(x, "left")}
	if len(values) == 0 {
		// Still validate x above so a nil operand panics consistently.
		return constCond[T](neg)
	}
	if len(values) > linearSetMax {
		c.set = make(map[V]struct{}, len(values))
		for _, v := range values {
			c.set[v] = struct{}{}
		}
	} else {
		c.list = slices.Clone(values)
	}
	return foldConst(c, c.x.isConst)
}

type inListCond[T any, V comparable] struct {
	neg     bool
	x       operand[T, V]
	choices operand[T, []V]
}

func (c *inListCond[T, V]) Match(v T) (bool, error) {
	a, err := c.x.eval(v)
	if err != nil {
		return false, err
	}
	list, err := c.choices.eval(v)
	if err != nil {
		return false, err
	}
	return slices.Contains(list, a) != c.neg, nil
}

// InList matches when x is an element of the list computed from the input.
// Use In when the candidates are known up front.
func InList[T any, V comparable](x Expr[T, V], list Expr[T, []V]) Condition[T] {
	return &inListCond[T, V]{x: newOperand(x, "left"), choices: newOperand(list, "list")}
}

// NotInList matches when x is not an element of the computed list.
func NotInList[T any, V comparable](x Expr[T, V], list Expr[T, []V]) Condition[T] {
	return &inListCond[T, V]{neg: true, x: newOperand(x, "left"), choices: newOperand(list, "list")}
}
