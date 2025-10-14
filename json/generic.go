package json

import (
	"encoding/json"
	"fmt"
	"reflect"
)

var bindConfigs = make(map[reflect.Type]*bindConfig)

type bindConfig struct {
	Option           BindOption
	MarshalMapping   map[reflect.Type]string
	UnMarshalMapping map[string]reflect.Type
}

type BindOption struct {
	TypeKey  string
	ValueKey string
}

func WithTypeKey(typeKey string) func(opt *BindOption) {
	return func(opt *BindOption) {
		opt.TypeKey = typeKey
	}
}

func WithValueKey(valueKey string) func(opt *BindOption) {
	return func(opt *BindOption) {
		opt.ValueKey = valueKey
	}
}

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
	if any(g[0]) == nil {
		return []byte("null"), nil
	}
	iType := reflect.TypeFor[T]()
	config := bindConfigs[iType]
	if config == nil {
		return nil, fmt.Errorf("type %T is not binding", iType)
	}
	typeName := config.MarshalMapping[reflect.TypeOf(g[0])]
	if typeName == "" {
		return nil, fmt.Errorf("type %T is not binding", g[0])
	}

	result := map[string]interface{}{
		config.Option.TypeKey:  typeName,
		config.Option.ValueKey: g[0],
	}

	return json.Marshal(result)
}

func (g *G[T]) UnmarshalJSON(data []byte) error {
	if len(data) == 4 && string(data) == "null" {
		return nil
	}
	iType := reflect.TypeFor[T]()
	config := bindConfigs[iType]
	if config == nil {
		return fmt.Errorf("type %T is not binding", iType)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}

	valueData := result[config.Option.ValueKey]
	if len(valueData) == 0 {
		return nil
	}
	if len(valueData) == 4 && string(valueData) == "null" {
		return nil
	}

	typeData := result[config.Option.TypeKey]
	if len(typeData) < 2 {
		return fmt.Errorf("invalid json %s", data)
	}
	typeName := string(typeData[1 : len(typeData)-1])

	detailType := config.UnMarshalMapping[typeName]
	if detailType == nil {
		return fmt.Errorf("type:%s is not binding", typeName)
	}

	impValue := reflect.New(detailType)
	if err := json.Unmarshal([]byte(valueData), impValue.Interface()); err != nil {
		return err
	}
	v, ok := impValue.Elem().Interface().(T)
	if !ok {
		return fmt.Errorf("can not convert %v to %T", impValue, v)
	}

	gen := NG(v)
	*g = gen
	return nil
}

func NG[T any](data T) G[T] {
	return G[T]{data}
}

func Bind[T any](data map[string]T, opts ...func(opt *BindOption)) {
	opt := BindOption{
		TypeKey:  "type",
		ValueKey: "data",
	}
	for _, f := range opts {
		f(&opt)
	}
	tp := reflect.TypeFor[T]()
	if _, exist := bindConfigs[tp]; exist {
		panic(fmt.Sprintf("duplicate bind config for type %s", tp.String()))
	}
	bindConfigs[tp] = &bindConfig{
		Option:           opt,
		MarshalMapping:   make(map[reflect.Type]string),
		UnMarshalMapping: make(map[string]reflect.Type),
	}

	for k, v := range data {
		valueType := reflect.TypeOf(v)
		bindConfigs[tp].MarshalMapping[valueType] = k
		bindConfigs[tp].UnMarshalMapping[k] = valueType
	}
}
