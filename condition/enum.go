package condition

import (
	"fmt"
	"slices"

	. "github.com/k0923/go/json"
)

type EnumCondition[T any, E string | float64 | int] struct {
	X   G[Picker[T, E]]   `json:"x"`
	Opt string            `json:"opt"`
	Y   G[Picker[T, []E]] `json:"y"`
}

func (n *EnumCondition[T, E]) Match(data T) (bool, error) {
	if n.X.Value() == nil {
		return false, fmt.Errorf("x condition is nil")
	}
	if n.Y.Value() == nil {
		return false, fmt.Errorf("y condition is nil")
	}
	x, err := n.X.Value().Pick(data)
	if err != nil {
		return false, err
	}
	y, err := n.Y.Value().Pick(data)
	if err != nil {
		return false, err
	}
	switch n.Opt {
	case "in":
		return slices.Contains(y, x), nil
	case "not_in":
		return !slices.Contains(y, x), nil
	default:
		return false, fmt.Errorf("invalid operator: %v", n.Opt)
	}
}

type StringEnumPicker []string
