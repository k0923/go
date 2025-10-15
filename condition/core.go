package condition



type Condition[T any] interface {
	Match(data T) (bool, error)
}

type Picker[T any, E any] interface {
	Pick(from T) (E, error)
}
