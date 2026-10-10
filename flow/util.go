package flow

import "encoding/json"

func Unmarshal[T any](data []byte) (T, error) {
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	return result, nil
}

func SafeRun(f func(), errHandler func(err any)) {
	defer func() {
		if r := recover(); r != nil && errHandler != nil {
			errHandler(r)
		}
	}()
	f()
}


func Any[T any](list []T, pred func(T) bool) bool {
	for _, item := range list {
		if pred(item) {
			return true
		}
	}
	return false
}