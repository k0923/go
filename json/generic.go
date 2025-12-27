package xjson

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

// 定义通用的函数签名，兼容标准库和第三方库(jsoniter, sonic, etc)
type MarshalFunc func(v any) ([]byte, error)
type UnmarshalFunc func(data []byte, v any) error

var (
	bindConfigs = make(map[reflect.Type]*bindConfig)
	mu          sync.RWMutex
)

type bindConfig struct {
	Option           BindOption
	MarshalMapping   map[reflect.Type]string
	UnMarshalMapping map[string]reflect.Type
}

type BindOption struct {
	TypeKey     string
	ValueKey    string
	Initializer func(interface{}) interface{}

	// 自定义 JSON 引擎入口
	MarshalAPI   MarshalFunc
	UnmarshalAPI UnmarshalFunc
}

type OptionFunc func(opt *BindOption)

// ---------------- 配置函数 ----------------

func WithTypeKey(typeKey string) OptionFunc {
	return func(opt *BindOption) {
		opt.TypeKey = typeKey
	}
}

func WithValueKey(valueKey string) OptionFunc {
	return func(opt *BindOption) {
		opt.ValueKey = valueKey
	}
}

// WithInitializer 保持之前的初始化钩子功能
func WithInitializer[T any](fn func(T) T) OptionFunc {
	return func(opt *BindOption) {
		opt.Initializer = func(raw interface{}) interface{} {
			if v, ok := raw.(T); ok {
				return fn(v)
			}
			return raw
		}
	}
}

// WithJSONHandler 允许替换底层的 JSON 序列化/反序列化实现
// 场景：使用性能更好的 json-iterator, sonic, go-json 等替代 encoding/json
func WithJSONHandler(m MarshalFunc, u UnmarshalFunc) OptionFunc {
	return func(opt *BindOption) {
		opt.MarshalAPI = m
		opt.UnmarshalAPI = u
	}
}

// ---------------- 核心逻辑 ----------------

type G[T any] []T

func (g G[T]) Value() T {
	if len(g) == 0 {
		var t T
		return t
	}
	return g[0]
}

// NG 构建入口，触发 Initializer
func NG[T any](data T) G[T] {
	iType := reflect.TypeFor[T]()

	mu.RLock()
	config := bindConfigs[iType]
	mu.RUnlock()

	finalData := data
	if config != nil && config.Option.Initializer != nil {
		res := config.Option.Initializer(finalData)
		if res != nil {
			if v, ok := res.(T); ok {
				finalData = v
			}
		}
	}
	return G[T]{finalData}
}

// TypeName 获取类型名
func (g G[T]) TypeName() string {
	if len(g) == 0 {
		return ""
	}
	iType := reflect.TypeFor[T]()
	mu.RLock()
	config := bindConfigs[iType]
	mu.RUnlock()

	if config == nil {
		return ""
	}
	valueType := reflect.TypeOf(g[0])
	return config.MarshalMapping[valueType]
}

// MarshalJSON 序列化
func (g G[T]) MarshalJSON() ([]byte, error) {
	if len(g) == 0 || any(g[0]) == nil {
		return []byte("null"), nil
	}

	iType := reflect.TypeFor[T]()
	mu.RLock()
	config := bindConfigs[iType]
	mu.RUnlock()

	if config == nil {
		return nil, fmt.Errorf("type %v is not binding", iType)
	}

	valueType := reflect.TypeOf(g[0])
	typeName := config.MarshalMapping[valueType]
	if typeName == "" {
		return nil, fmt.Errorf("concrete type %v is not registered for interface %v", valueType, iType)
	}

	// 确定使用哪个 Marshal 函数
	marshalFn := json.Marshal
	if config.Option.MarshalAPI != nil {
		marshalFn = config.Option.MarshalAPI
	}

	// 构造包装结构
	result := map[string]interface{}{
		config.Option.TypeKey:  typeName,
		config.Option.ValueKey: g[0],
	}

	return marshalFn(result)
}

// UnmarshalJSON 反序列化
func (g *G[T]) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || (len(data) == 4 && string(data) == "null") {
		return nil
	}

	iType := reflect.TypeFor[T]()
	mu.RLock()
	config := bindConfigs[iType]
	mu.RUnlock()

	if config == nil {
		return fmt.Errorf("type %v is not binding", iType)
	}

	// 确定使用哪个 Unmarshal 函数
	unmarshalFn := json.Unmarshal
	if config.Option.UnmarshalAPI != nil {
		unmarshalFn = config.Option.UnmarshalAPI
	}

	// 第一次解析：解析外层结构
	// 注意：这里仍然依赖 json.RawMessage，因为它是目前 Go 生态通用的“延迟解析”载体
	var result map[string]json.RawMessage
	if err := unmarshalFn(data, &result); err != nil {
		return err
	}

	// 提取数据部分
	valueData := result[config.Option.ValueKey]
	if len(valueData) == 0 || (len(valueData) == 4 && string(valueData) == "null") {
		return nil
	}

	// 提取类型标识
	typeData := result[config.Option.TypeKey]
	if len(typeData) < 2 {
		return fmt.Errorf("invalid json format, missing type field: %s", data)
	}
	typeName := string(typeData[1 : len(typeData)-1])

	detailType := config.UnMarshalMapping[typeName]
	if detailType == nil {
		return fmt.Errorf("type alias '%s' is not registered", typeName)
	}

	// 创建具体类型的实例
	impValue := reflect.New(detailType)

	// 第二次解析：将 RawMessage 解析为具体类型
	// 这里同样使用用户配置的解析引擎
	if err := unmarshalFn(valueData, impValue.Interface()); err != nil {
		return err
	}

	v, ok := impValue.Elem().Interface().(T)
	if !ok {
		return fmt.Errorf("cannot convert instance of %v to interface %v", detailType, iType)
	}

	*g = NG(v)
	return nil
}

func Bind[T any](data map[string]T, opts ...OptionFunc) {
	opt := BindOption{
		TypeKey:  "type",
		ValueKey: "data",
	}
	for _, f := range opts {
		f(&opt)
	}

	tp := reflect.TypeFor[T]()
	mu.Lock()
	defer mu.Unlock()

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

// ParseFromJSON 辅助函数
func ParseFromJSON[T any](typeName string, data []byte) (G[T], error) {
	iType := reflect.TypeFor[T]()
	mu.RLock()
	config := bindConfigs[iType]
	mu.RUnlock()

	if config == nil {
		return nil, fmt.Errorf("type %v is not binding", iType)
	}

	// 确定引擎
	unmarshalFn := json.Unmarshal
	if config.Option.UnmarshalAPI != nil {
		unmarshalFn = config.Option.UnmarshalAPI
	}

	detailType := config.UnMarshalMapping[typeName]
	if detailType == nil {
		return nil, fmt.Errorf("type alias '%s' is not registered", typeName)
	}

	impValue := reflect.New(detailType)
	if err := unmarshalFn(data, impValue.Interface()); err != nil {
		return nil, err
	}

	v, ok := impValue.Elem().Interface().(T)
	if !ok {
		return nil, fmt.Errorf("cannot convert instance of %v to interface %v", detailType, iType)
	}

	return NG(v), nil
}
