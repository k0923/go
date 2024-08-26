package formula

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

type functions map[string]Function

func (fs functions) register(fn Function) error {
	fnName := strings.ToUpper(reflect.TypeOf(fn).Name())
	if _, exist := fs[fnName]; exist {
		return fmt.Errorf("duplicate function name:%s found", fnName)
	}
	fs[fnName] = fn
	return nil
}

func (fs functions) hasFn(name string) bool {
	return fs[name] != nil
}

var fns functions = functions{}

func Register(fn Function) error {
	return fns.register(fn)
}

type Function interface {
	Valid(args []Expr) error
	Calculate(args []interface{}) (interface{}, error)
}

func processFloatArgs(args []interface{}, handler func(float64) error) (int, error) {
	for i, arg := range args {
		switch para := arg.(type) {
		case int:
			if err := handler(float64(para)); err != nil {
				return i, err
			}
			break
		case float64:
			if err := handler(float64(para)); err != nil {
				return i, err
			}
			break
		default:
			return i, errors.New("参数类型不是数字类型")
		}
	}
	return -1, nil
}

type Min int

func (m Min) Valid(args []Expr) error {
	if len(args) < 2 {
		return errors.New("函数的参数不能少于2个")
	}
	return nil
}

func (m Min) Calculate(args []interface{}) (interface{}, error) {
	var result float64 = 0
	hasSetResult := false
	if _, err := processFloatArgs(args, func(f float64) error {
		if !hasSetResult {
			hasSetResult = true
			result = f
			return nil
		}
		if f < result {
			result = f
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

type Max int

func (m Max) Valid(args []Expr) error {
	if len(args) < 2 {
		return errors.New("函数的参数不能少于2个")
	}
	return nil
}

func (m Max) Calculate(args []interface{}) (interface{}, error) {
	var result float64 = 0
	hasSetResult := false
	if _, err := processFloatArgs(args, func(f float64) error {
		if hasSetResult == false {
			hasSetResult = true
			result = f
			return nil
		}
		if f > result {
			result = f
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

type Avg int

func (Avg) Valid(args []Expr) error {
	if len(args) < 2 {
		return errors.New("函数的参数不能少于2个")
	}
	return nil
}

func (Avg) Calculate(args []interface{}) (interface{}, error) {
	var result float64
	if _, err := processFloatArgs(args, func(f float64) error {
		result += f
		return nil
	}); err != nil {
		return nil, err
	}
	return result / float64(len(args)), nil
}

type Sum int

func (Sum) Valid(args []Expr) error {
	if len(args) < 2 {
		return errors.New("函数的参数不能少于2个")
	}
	return nil
}

func (Sum) Calculate(args []interface{}) (interface{}, error) {
	var result float64
	if _, err := processFloatArgs(args, func(f float64) error {
		result += f
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}
