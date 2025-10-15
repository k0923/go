package condition

import (
	"fmt"

	. "github.com/k0923/go/json"
)

var _ Condition[any] = (*ArrayCondition[any])(nil)
var _ Condition[any] = (*CountCondition[any])(nil)

type ArrayCondition[T any] struct {
	X   G[Picker[T, []T]] `json:"x"`
	Opt string            `json:"opt"`
	Y   G[Condition[T]]   `json:"y"`
}

func (cond *ArrayCondition[T]) Match(data T) (bool, error) {
	if cond.Y.Value() == nil {
		return false, fmt.Errorf("y condition is nil")
	}
	var x []T = nil
	var err error
	if cond.X.Value() != nil {
		x, err = cond.X.Value().Pick(data)
		if err != nil {
			return false, err
		}
	}

	switch cond.Opt {
	case "any":
		for _, item := range x {
			result, err := cond.Y.Value().Match(item)
			if err != nil {
				return false, err
			}
			if result {
				return true, nil
			}
		}
		return false, nil
	case "all":
		for _, item := range x {
			result, err := cond.Y.Value().Match(item)
			if err != nil {
				return false, err
			}
			if !result {
				return false, nil
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("invalid operator: %v", cond.Opt)
	}

}

type CountCondition[T any] struct {
	NumberCondition[T, int]
}
