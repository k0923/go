package condition

import (
	"fmt"

	. "github.com/k0923/go/json"
)

var _ Picker[any, string] = (*ConstStringPicker[any])(nil)
var _ Picker[any, float64] = (*ConstFloatPicker[any])(nil)
var _ Picker[any, int] = (*ConstIntPicker[any])(nil)

type ConstStringPicker[T any] string

func (c ConstStringPicker[T]) Pick(from T) (string, error) {
	return string(c), nil
}

type ConstFloatPicker[T any] float64

func (c ConstFloatPicker[T]) Pick(from T) (float64, error) {
	return float64(c), nil
}

type ConstIntPicker[T any] int

func (c ConstIntPicker[T]) Pick(from T) (int, error) {
	return int(c), nil
}

type ConstEnumPicker[T any, E string | float64 | int] []E

func (c ConstEnumPicker[T, E]) Pick(from T) ([]E, error) {
	return c, nil
}

type CalculatePicker[T any] struct {
	X   G[Picker[T, float64]] `json:"x"`
	Opt string                `json:"opt"`
	Y   G[Picker[T, float64]] `json:"y"`
}

func (c *CalculatePicker[T]) Pick(from T) (float64, error) {
	var x float64 = 0
	var y float64 = 0
	var err error
	if c.X.Value() == nil {
		return 0, fmt.Errorf("x condition is nil")
	}
	if c.Y.Value() == nil {
		return 0, fmt.Errorf("y condition is nil")
	}
	if x, err = c.X.Value().Pick(from); err != nil {
		return 0, err
	}
	if y, err = c.Y.Value().Pick(from); err != nil {
		return 0, err
	}
	switch c.Opt {
	case "add":
		return x + y, nil
	case "sub":
		return x - y, nil
	case "mul":
		return x * y, nil
	case "div":
		if y == 0 {
			return 0, fmt.Errorf("divide by zero")
		}
		return x / y, nil
	default:
		return 0, fmt.Errorf("invalid operator: %v", c.Opt)
	}
}
