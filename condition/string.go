package condition

type Equaler[T float64 | string | bool | int] interface {
	Equals(source T, target T) bool
}

type StringProcessor[T float64 | string | bool] struct{}

func (s StringProcessor[T]) Equals(source T, target T) bool {
	return source == target
}
