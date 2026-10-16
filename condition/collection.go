package condition

type quantifier uint8

const (
	qAny quantifier = iota
	qAll
	qNone
)

// quantCond applies pred to each element. Unlike the old ArrayCondition the
// element type E is independent of the input type T.
type quantCond[T, E any] struct {
	q     quantifier
	items operand[T, []E]
	pred  Condition[E]
}

func (c *quantCond[T, E]) Match(v T) (bool, error) {
	items, err := c.items.eval(v)
	if err != nil {
		return false, err
	}
	// Any stops at the first match; All and None stop at the first element
	// that decides the outcome.
	stopOn := c.q != qAll
	for _, item := range items {
		ok, err := c.pred.Match(item)
		if err != nil {
			return false, err
		}
		if ok == stopOn {
			return c.q == qAny, nil
		}
	}
	return c.q != qAny, nil
}

// Any matches when pred matches at least one element. Empty lists are false.
func Any[T, E any](items Expr[T, []E], pred Condition[E]) Condition[T] {
	return newQuant(qAny, items, pred)
}

// All matches when pred matches every element. Empty lists are true.
func All[T, E any](items Expr[T, []E], pred Condition[E]) Condition[T] {
	return newQuant(qAll, items, pred)
}

// None matches when pred matches no element. Empty lists are true.
func None[T, E any](items Expr[T, []E], pred Condition[E]) Condition[T] {
	return newQuant(qNone, items, pred)
}

func newQuant[T, E any](q quantifier, items Expr[T, []E], pred Condition[E]) Condition[T] {
	if pred == nil {
		panic("condition: nil element predicate")
	}
	return &quantCond[T, E]{q: q, items: newOperand(items, "list"), pred: pred}
}
