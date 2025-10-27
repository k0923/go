package condition

import (
	"fmt"
	"strings"

	. "github.com/k0923/go/json"
)

var _ Condition[any] = (*StringCondition[any])(nil)

type StringCondition[T any] struct {
	X   G[Picker[T, string]] `json:"x"`
	Opt string               `json:"opt"`
	Y   G[Picker[T, string]] `json:"y"`
}

func (n *StringCondition[T]) Match(data T) (bool, error) {
	var x string = ""
	var y string = ""
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
	case "ne":
		return x != y, nil
	case "include":
		return strings.Contains(x, y), nil
	case "exclude":
		return !strings.Contains(x, y), nil
	case "start_with":
		return strings.HasPrefix(x, y), nil
	case "end_with":
		return strings.HasSuffix(x, y), nil
	default:
		return false, fmt.Errorf("invalid operator: %v", n.Opt)
	}
}
