package condition

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