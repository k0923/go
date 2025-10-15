package condition

import (
	. "github.com/k0923/go/json"
)

var _ Condition[any] = (*GroupCondition[any])(nil)

type GroupCondition[T any] struct {
	Opt        string
	Conditions []G[Condition[T]]
}

func (g *GroupCondition[T]) Match(data T) (bool, error) {
	if len(g.Conditions) == 0 {
		return true, nil
	}
	for _, condition := range g.Conditions {
		if condition.Value() == nil {
			continue
		}
		result, err := condition.Value().Match(data)
		if err != nil {
			return false, err
		}
		if g.Opt == "and" {
			if !result {
				return false, nil
			}
		} else {
			if result {
				return true, nil
			}
		}
	}
	return true, nil
}
