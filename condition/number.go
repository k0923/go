package condition

import (
	"fmt"

	. "github.com/k0923/go/json"
)

var _ Picker[any, float64] = (*ConstNumberPicker[any])(nil)
var _ Condition[any] = (*NumberCondition[any, float64])(nil)

type NumberCondition[T any, E float64 | int] struct {
	X   G[Picker[T, E]] `json:"x"`
	Opt string          `json:"opt"`
	Y   G[Picker[T, E]] `json:"y"`
}

func (n *NumberCondition[T, E]) Match(data T) (bool, error) {
	var x E = 0
	var y E = 0
	var err error
	if n.X.Value() != nil {
		x, err = n.X.Value().Pick(data)
		if err != nil {
			return false, err
		}
	}
	if n.Y.Value() != nil {
		y, err = n.Y.Value().Pick(data)
		if err != nil {
			return false, err
		}
	}
	switch n.Opt {
	case "gt":
		return x > y, nil
	case "lt":
		return x < y, nil
	case "ge":
		return x >= y, nil
	case "le":
		return x <= y, nil
	case "eq":
		return x == y, nil
	case "ne":
		return x != y, nil
	default:
		return false, fmt.Errorf("invalid operator: %v", n.Opt)
	}
}

type ConstNumberPicker[T any] float64

func (c ConstNumberPicker[T]) Pick(from T) (float64, error) {
	return float64(c), nil
}
