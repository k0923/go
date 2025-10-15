package condition

import (
	"fmt"

	. "github.com/k0923/go/json"
)

var _ Picker[any, bool] = (*ConstBoolPicker[any])(nil)
var _ Condition[any] = (*BoolCondition[any])(nil)

type BoolCondition[T any] struct {
	X   G[Picker[T, bool]] `json:"x"`
	Opt string             `json:"opt"`
	Y   G[Picker[T, bool]] `json:"y"`
}

func (n *BoolCondition[T]) Match(data T) (bool, error) {
	var x bool = false
	var y bool = false
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
	case "eq":
		return x == y, nil
	default:
		return false, fmt.Errorf("invalid operator: %v", n.Opt)
	}
}

type ConstBoolPicker[T any] bool

func (c ConstBoolPicker[T]) Pick(from T) (bool, error) {
	return bool(c), nil
}
