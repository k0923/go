package json

import (
	"encoding/json"
)

type Optional[T any] []*T

func (opt Optional[T]) IsNotSet() bool {
	return len(opt) == 0
}

func (opt Optional[T]) IsNull() bool {
	return len(opt) == 1 && opt[0] == nil
}

func (opt Optional[T]) IsSet() bool {
	return len(opt) > 0
}

// 置为null
func (opt *Optional[T]) SetNull() {
	*opt = make(Optional[T], 1)
	(*opt)[0] = nil
}

// 置空
func (opt *Optional[T]) Clear() {
	*opt = nil
}

func (opt Optional[T]) Value() T {
	if result := opt.Ptr(); result != nil {
		return *result
	}
	var v T
	return v
}

func (opt Optional[T]) Ptr() *T {
	if len(opt) == 1 && opt[0] != nil {
		return opt[0]
	}
	return nil
}

func (opt Optional[T]) MarshalJSON() ([]byte, error) {
	if result := opt.Ptr(); result != nil {
		return json.Marshal(result)
	}
	return []byte("null"), nil
}

func (opt *Optional[T]) UnmarshalJSON(data []byte) error {
	*opt = make(Optional[T], 0)
	if len(data) == 4 && string(data) == "null" {
		*opt = append(*opt, nil)
		return nil
	}
	var target T
	err := json.Unmarshal(data, &target)
	if err != nil {
		return err
	}
	*opt = append(*opt, &target)
	return nil
}

func N[T any](data T) Optional[T] {
	return Optional[T]{
		&data,
	}
}

func Null[T any]() Optional[T] {
	return Optional[T]{
		nil,
	}
}
