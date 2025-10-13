package json

import (
	"encoding/json"
	"fmt"
	"reflect"
)

type CustomSerializer interface {
	Marshal(tp string, data interface{}) ([]byte, error)
	Unmarshal(data json.RawMessage) (string, []byte, error) // 输入一个原始的JSON数据，需要返回对应的类型域和数据域
}

var defaultS = defaultSerializer{}

type defaultSerializer struct{}

func (s defaultSerializer) Marshal(tp string, data interface{}) ([]byte, error) {
	return json.Marshal(marshalProxy{
		Type: tp,
		Data: data,
	})
}

func (s defaultSerializer) Unmarshal(data json.RawMessage) (string, []byte, error) {
	var proxy unmarshalProxy
	if err := json.Unmarshal(data, &proxy); err != nil {
		return "", nil, err
	}
	return proxy.Type, proxy.Data, nil
}

var customSerializer = make(map[reflect.Type]CustomSerializer)
var hub = make(map[reflect.Type]map[string]reflect.Type)
var hubMap = make(map[reflect.Type]map[reflect.Type]string)

type G[T any] []T

func (g G[T]) Value() T {
	if len(g) == 0 {
		var t T
		return t
	}
	return g[0]
}

func (g G[T]) MarshalJSON() ([]byte, error) {
	if len(g) == 0 {
		return []byte("null"), nil
	}

	value := reflect.ValueOf(g[0])
	if !value.IsValid() {
		return []byte("null"), nil
	}

	tp := value.Type()
	if tp.Kind() == reflect.Ptr && value.IsNil() {
		return []byte("null"), nil
	}
	typeofT := getInterfaceType[T]()
	if subHub, exist := hubMap[typeofT]; exist {
		if tpName, exist := subHub[tp]; exist {
			s := customSerializer[typeofT]
			if s == nil {
				s = defaultS
			}

			return s.Marshal(tpName, g[0])
		}
	}
	return nil, fmt.Errorf("type %T is not binding", g[0])
}

func (g *G[T]) UnmarshalJSON(data []byte) error {
	if len(data) == 4 && string(data) == "null" {
		return nil
	}
	typeofT := getInterfaceType[T]()
	s := customSerializer[typeofT]
	if s == nil {
		s = defaultS
	}
	tpName, data, err := s.Unmarshal(data)
	if err != nil {
		return err
	}
	subMap := hub[typeofT]
	if subMap == nil {
		return fmt.Errorf("interface type:%T has no binding info,%v", typeofT, subMap)
	}
	tp := subMap[tpName]
	if tp == nil {
		return fmt.Errorf("type:%s is not binding", tpName)
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) == 4 && string(data) == "null" {
		return nil
	}

	result := reflect.New(tp)
	if err := json.Unmarshal(data, result.Interface()); err != nil {
		return err
	}

	switch data := result.Elem().Interface().(type) {
	case T:
		gen := NG(data)
		*g = gen
		return nil
	case *T:
		gen := NG(*data)
		*g = gen
		return nil
	default:
		return fmt.Errorf("type is not valid")
	}
}

func NG[T any](data T) G[T] {
	return G[T]{data}
}

func getType(tp reflect.Type) reflect.Type {
	if tp == nil {
		return nil
	}

	if tp.Kind() == reflect.Ptr {
		return tp
	}
	return reflect.PointerTo(tp)
}

func getInterfaceType[T any]() reflect.Type {
	tp := new(T)
	return reflect.TypeOf(tp).Elem()
}

func Bind[T any](data map[string]T, options ...CustomSerializer) {
	typeofT := getInterfaceType[T]()
	if len(options) > 0 {
		customSerializer[typeofT] = options[0]
	}
	for k, v := range data {
		tp := reflect.TypeOf(v)
		if tp == nil {
			panic(fmt.Errorf("type:%s is not valid", k))
		}
		if subHub, exist := hub[typeofT]; exist {
			if _, exist := subHub[k]; exist {
				panic(fmt.Errorf("duplicate type:%s binding1", k))
			} else {
				subHub[k] = tp
			}
		} else {
			hub[typeofT] = map[string]reflect.Type{k: tp}
		}
		if _, exist := hubMap[tp]; exist {
			panic(fmt.Errorf("duplicate type:%s binding2", k))
		}

		if subHub, exist := hubMap[typeofT]; exist {
			if _, exist := subHub[tp]; exist {
				panic(fmt.Errorf("duplicate type:%s binding3", k))
			} else {
				subHub[tp] = k
			}
		} else {
			hubMap[typeofT] = map[reflect.Type]string{
				tp: k,
			}
		}
	}
}

type marshalProxy struct {
	Type string      `json:"type,omitempty"`
	Data interface{} `json:"data,omitempty"`
}

type unmarshalProxy struct {
	Type string          `json:"type,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type generic[T any] struct {
	Data T
}
