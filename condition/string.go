package condition

import (
	"fmt"
	"strings"

	. "github.com/k0923/go/json"
)

var _ Picker[any, string] = (*ConstStringPicker[any])(nil)
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
	case "contains":
		return strings.Contains(x, y), nil
	case "startswith":
		return strings.HasPrefix(x, y), nil
	case "endswith":
		return strings.HasSuffix(x, y), nil
	default:
		return false, fmt.Errorf("invalid operator: %v", n.Opt)
	}
}

type ConstStringPicker[T any] string

func (c ConstStringPicker[T]) Pick(from T) (string, error) {
	return string(c), nil
}
