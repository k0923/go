package xjson

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

// 全局配置存储与并发锁
var (
	bindConfigs = make(map[reflect.Type]*bindConfig)
	mu          sync.RWMutex
)

// bindConfig 存储类型绑定的配置信息
type bindConfig struct {
	Option           BindOption
	MarshalMapping   map[reflect.Type]string
	UnMarshalMapping map[string]reflect.Type
}

// BindOption 定义 JSON 字段名和初始化钩子
type BindOption struct {
	TypeKey     string
	ValueKey    string
	Initializer func(interface{}) interface{} // 存储类型擦除后的初始化函数
}

// Functional Option 定义
type OptionFunc func(opt *BindOption)

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

// WithInitializer 允许在对象创建/反序列化时注入自定义逻辑
// 泛型函数 fn 接收原始对象，返回处理后的对象 (支持对 Slice 的 append 操作)
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

// G 是 Generic 的缩写，用于多态类型的 JSON 包装
// 底层使用切片是为了方便处理 nil 值和更轻量的内存占用
type G[T any] []T

// Value 获取原始值，如果为空则返回零值
func (g G[T]) Value() T {
	if len(g) == 0 {
		var t T
		return t
	}
	return g[0]
}

// NG (New Generic) 是构建 G[T] 的入口
// 所有的反序列化和手动构造都应经过此函数，以触发 Initializer
func NG[T any](data T) G[T] {
	// 获取类型配置
	iType := reflect.TypeFor[T]()
	
	mu.RLock()
	config := bindConfigs[iType]
	mu.RUnlock()

	finalData := data

	// 如果存在配置且有初始化钩子，执行钩子
	if config != nil && config.Option.Initializer != nil {
		// 执行用户注入的逻辑 (例如依赖注入、默认值填充)
		res := config.Option.Initializer(finalData)
		// 如果钩子返回了新对象(主要针对 Slice/Map/基本类型)，更新它
		if res != nil {
			if v, ok := res.(T); ok {
				finalData = v
			}
		}
	}

	return G[T]{finalData}
}

// TypeName 获取当前值的类型名称
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
	
	// 处理接口持有的实际类型
	valueType := reflect.TypeOf(g[0])
	return config.MarshalMapping[valueType]
}

// MarshalJSON 实现序列化逻辑
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

	// 构造 {type, data} 结构
	result := map[string]interface{}{
		config.Option.TypeKey:  typeName,
		config.Option.ValueKey: g[0],
	}

	return json.Marshal(result)
}

// UnmarshalJSON 实现反序列化逻辑
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

	// 第一次解析：解析外层 Map
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil {
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
	// 去除引号 "TypeName" -> TypeName
	typeName := string(typeData[1 : len(typeData)-1])

	detailType := config.UnMarshalMapping[typeName]
	if detailType == nil {
		return fmt.Errorf("type alias '%s' is not registered", typeName)
	}

	// 创建具体类型的实例
	impValue := reflect.New(detailType)
	if err := json.Unmarshal(valueData, impValue.Interface()); err != nil {
		return err
	}

	// 类型断言回接口 T
	v, ok := impValue.Elem().Interface().(T)
	if !ok {
		return fmt.Errorf("cannot convert instance of %v to interface %v", detailType, iType)
	}

	// 使用 NG 包装，这也将触发 Initializer
	*g = NG(v)
	return nil
}

// Bind 注册接口 T 的具体实现类型
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

// ParseFromJSON 手动从 JSON 解析为对象，适用于非标准 Unmarshal 场景
func ParseFromJSON[T any](typeName string, data []byte) (G[T], error) {
	iType := reflect.TypeFor[T]()
	
	mu.RLock()
	config := bindConfigs[iType]
	mu.RUnlock()

	if config == nil {
		return nil, fmt.Errorf("type %v is not binding", iType)
	}

	detailType := config.UnMarshalMapping[typeName]
	if detailType == nil {
		return nil, fmt.Errorf("type alias '%s' is not registered", typeName)
	}

	impValue := reflect.New(detailType)
	if err := json.Unmarshal(data, impValue.Interface()); err != nil {
		return nil, err
	}

	v, ok := impValue.Elem().Interface().(T)
	if !ok {
		return nil, fmt.Errorf("cannot convert instance of %v to interface %v", detailType, iType)
	}

	return NG(v), nil
}