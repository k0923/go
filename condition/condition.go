package condition

import "fmt"

// Condition is a predicate over T.
type Condition[T any] interface {
	Match(v T) (bool, error)
}

// CondFunc adapts an ordinary function to Condition.
type CondFunc[T any] func(T) (bool, error)

func (f CondFunc[T]) Match(v T) (bool, error) { return f(v) }

type constCond[T any] bool

func (c constCond[T]) Match(T) (bool, error) { return bool(c), nil }

// True returns a condition that always matches.
func True[T any]() Condition[T] { return constCond[T](true) }

// False returns a condition that never matches.
func False[T any]() Condition[T] { return constCond[T](false) }

type andCond[T any] []Condition[T]

func (a andCond[T]) Match(v T) (bool, error) {
	for _, c := range a {
		if ok, err := c.Match(v); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

type orCond[T any] []Condition[T]

func (o orCond[T]) Match(v T) (bool, error) {
	for _, c := range o {
		if ok, err := c.Match(v); err != nil || ok {
			return ok && err == nil, err
		}
	}
	return false, nil
}

// And matches when every condition matches. And() is True.
// Nested Ands are flattened, True operands dropped, and any False operand
// collapses the whole group to False. It panics on a nil operand.
func And[T any](conds ...Condition[T]) Condition[T] { return group(true, conds) }

// Or matches when at least one condition matches. Or() is False.
// Nested Ors are flattened, False operands dropped, and any True operand
// collapses the whole group to True. It panics on a nil operand.
func Or[T any](conds ...Condition[T]) Condition[T] { return group(false, conds) }

func group[T any](isAnd bool, conds []Condition[T]) Condition[T] {
	flat := make([]Condition[T], 0, len(conds))
	for i, c := range conds {
		switch c := c.(type) {
		case nil:
			panic(fmt.Sprintf("condition: nil operand at index %d", i))
		case constCond[T]:
			// The identity element (true for And, false for Or) is dropped;
			// the absorbing element decides the whole group.
			if bool(c) != isAnd {
				return c
			}
		case andCond[T]:
			if isAnd {
				flat = append(flat, c...)
			} else {
				flat = append(flat, c)
			}
		case orCond[T]:
			if !isAnd {
				flat = append(flat, c...)
			} else {
				flat = append(flat, c)
			}
		default:
			flat = append(flat, c)
		}
	}
	switch len(flat) {
	case 0:
		return constCond[T](isAnd)
	case 1:
		return flat[0]
	}
	if isAnd {
		return andCond[T](flat)
	}
	return orCond[T](flat)
}

type notCond[T any] struct{ c Condition[T] }

func (n notCond[T]) Match(v T) (bool, error) {
	ok, err := n.c.Match(v)
	return !ok && err == nil, err
}

// Not negates c. Errors from c are propagated with a false result.
// It panics on a nil operand.
func Not[T any](c Condition[T]) Condition[T] {
	switch c := c.(type) {
	case nil:
		panic("condition: nil operand to Not")
	case constCond[T]:
		return !c
	case notCond[T]:
		return c.c
	}
	return notCond[T]{c}
}
