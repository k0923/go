package xjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
)

// -----------------------------------------------------------------------------
// 全局配置存储 (针对“读多写少/只写一次”极致优化)
// -----------------------------------------------------------------------------

type configMap map[reflect.Type]*bindConfig

var (
	// globalConfigs 使用 atomic.Value 存储 configMap
	// 读操作：atomic.Load (极快，纳秒级，无锁竞争)
	// 写操作：Copy-On-Write (慢，但在 init 阶段无所谓)
	globalConfigs atomic.Value

	// bindMu 仅用于保护 Bind 时的并发写安全
	bindMu sync.Mutex
)

func init() {
	// 初始化一个空的 map
	globalConfigs.Store(make(configMap))
}

// loadConfig 无锁快速读取配置
func loadConfig(typ reflect.Type) *bindConfig {
	configs := globalConfigs.Load().(configMap)
	return configs[typ]
}

// -----------------------------------------------------------------------------
// 类型定义与配置结构
// -----------------------------------------------------------------------------

type MarshalFunc func(v any) ([]byte, error)
type UnmarshalFunc func(data []byte, v any) error

type bindConfig struct {
	Option           BindOption
	MarshalMapping   map[reflect.Type]string
	UnMarshalMapping map[string]reflect.Type
}

type BindOption struct {
	TypeKey      string
	ValueKey     string
	Initializer  func(interface{}) interface{}
	MarshalAPI   MarshalFunc
	UnmarshalAPI UnmarshalFunc
}

type OptionFunc func(opt *BindOption)

// -----------------------------------------------------------------------------
// G[T] 泛型结构 (Slice of Struct)
// -----------------------------------------------------------------------------

// gData 内部数据载体
type gData[T any] struct {
	v      T
	isNull bool
}

// G 是一个泛型切片。
// 1. nil (len=0) -> Undefined (omitempty 生效)
// 2. len>0, isNull=true -> Null
// 3. len>0, isNull=false -> Value
type G[T any] []gData[T]

// NG 构建函数
func NG[T any](data T) G[T] {
	// 1. 尝试应用初始化钩子
	finalData := applyInitializer(data)

	// 2. 构造切片，无额外指针分配
	return []gData[T]{
		{v: finalData, isNull: false},
	}
}

func (g G[T]) Value() T {
	if len(g) > 0 && !g[0].isNull {
		return g[0].v
	}
	var zero T
	return zero
}

// IsZero 配合 omitempty 使用
func (g G[T]) IsZero() bool {
	return len(g) == 0
}

func (g G[T]) TypeName() string {
	if len(g) == 0 || g[0].isNull {
		return ""
	}

	// 无锁读取配置
	config := loadConfig(reflect.TypeFor[T]())
	if config == nil {
		return ""
	}

	valueType := reflect.TypeOf(g[0].v)
	return config.MarshalMapping[valueType]
}

func (g G[T]) TypeKeyName() string {
	config := loadConfig(reflect.TypeFor[T]())
	if config == nil {
		return ""
	}
	return config.Option.TypeKey
}

func (g G[T]) ValueKeyName() string {
	config := loadConfig(reflect.TypeFor[T]())
	if config == nil {
		return ""
	}
	return config.Option.ValueKey
}

func (g G[T]) DetailType(typeName string) reflect.Type {
	config := loadConfig(reflect.TypeFor[T]())
	if config == nil {
		return nil
	}
	return config.UnMarshalMapping[typeName]
}

// -----------------------------------------------------------------------------
// JSON 序列化 (无锁)
// -----------------------------------------------------------------------------

func (g G[T]) MarshalJSON() ([]byte, error) {
	// 1. Undefined
	if len(g) == 0 {
		return []byte("null"), nil
	}

	// 2. Explicit Null
	if g[0].isNull || any(g[0].v) == nil {
		return []byte("null"), nil
	}

	// 3. 获取配置 (无锁)
	iType := reflect.TypeFor[T]()
	config := loadConfig(iType)
	if config == nil {
		return nil, fmt.Errorf("xjson: type %v is not binding", iType)
	}

	// 4. 类型映射
	val := g[0].v
	valueType := reflect.TypeOf(val)
	typeName, ok := config.MarshalMapping[valueType]
	if !ok {
		return nil, fmt.Errorf("xjson: concrete type %v is not registered for interface %v", valueType, iType)
	}

	// 5. 序列化
	marshalFn := json.Marshal
	if config.Option.MarshalAPI != nil {
		marshalFn = config.Option.MarshalAPI
	}

	wrapper := map[string]interface{}{
		config.Option.TypeKey:  typeName,
		config.Option.ValueKey: val,
	}
	return marshalFn(wrapper)
}

// -----------------------------------------------------------------------------
// JSON 反序列化 (无锁)
// -----------------------------------------------------------------------------

func (g *G[T]) UnmarshalJSON(data []byte) error {
	// 1. 处理 Null
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*g = []gData[T]{{isNull: true}}
		return nil
	}

	// 2. 获取配置 (无锁)
	iType := reflect.TypeFor[T]()
	config := loadConfig(iType)
	if config == nil {
		return fmt.Errorf("xjson: type %v is not binding", iType)
	}

	unmarshalFn := json.Unmarshal
	if config.Option.UnmarshalAPI != nil {
		unmarshalFn = config.Option.UnmarshalAPI
	}

	// 3. 解析外层
	var wrapper map[string]json.RawMessage
	if err := unmarshalFn(data, &wrapper); err != nil {
		return err
	}

	// 4. 检查数据部分
	valueData := wrapper[config.Option.ValueKey]
	if len(valueData) == 0 || bytes.Equal(valueData, []byte("null")) {
		*g = []gData[T]{{isNull: true}}
		return nil
	}

	// 5. 解析类型名
	typeDataRaw, ok := wrapper[config.Option.TypeKey]
	if !ok {
		return fmt.Errorf("xjson: missing type field '%s'", config.Option.TypeKey)
	}
	var typeName string
	if err := unmarshalFn(typeDataRaw, &typeName); err != nil {
		return fmt.Errorf("xjson: invalid type field: %w", err)
	}

	// 6. 查找具体类型
	detailType := config.UnMarshalMapping[typeName]
	if detailType == nil {
		return fmt.Errorf("xjson: type alias '%s' is not registered", typeName)
	}

	// 7. 解析具体对象
	impValue := reflect.New(detailType)
	if err := unmarshalFn(valueData, impValue.Interface()); err != nil {
		return err
	}

	v, ok := impValue.Elem().Interface().(T)
	if !ok {
		return fmt.Errorf("xjson: cannot convert %v to interface %v", detailType, iType)
	}

	// 8. 初始化钩子
	v = applyInitializerWithConfig(v, config)

	// 9. 赋值
	*g = []gData[T]{{v: v, isNull: false}}
	return nil
}

// -----------------------------------------------------------------------------
// 配置注册 (Copy-On-Write 写时复制)
// -----------------------------------------------------------------------------

func Bind[T any](data map[string]T, opts ...OptionFunc) {
	// 1. 准备配置对象
	opt := BindOption{TypeKey: "type", ValueKey: "data"}
	for _, f := range opts {
		f(&opt)
	}

	tp := reflect.TypeFor[T]()
	newCfg := &bindConfig{
		Option:           opt,
		MarshalMapping:   make(map[reflect.Type]string),
		UnMarshalMapping: make(map[string]reflect.Type),
	}

	for k, v := range data {
		vt := reflect.TypeOf(v)
		if vt == nil {
			continue
		}
		newCfg.MarshalMapping[vt] = k
		newCfg.UnMarshalMapping[k] = vt
	}

	// 2. 加锁进行更新 (防止多个 init 并发写入冲突)
	bindMu.Lock()
	defer bindMu.Unlock()

	// 3. 复制旧 Map (Copy)
	oldMap := globalConfigs.Load().(configMap)
	newMap := make(configMap, len(oldMap)+1)
	for k, v := range oldMap {
		newMap[k] = v
	}

	// 4. 写入新配置 (Write)
	if _, exist := newMap[tp]; exist {
		panic(fmt.Sprintf("xjson: duplicate bind config for type %s", tp.String()))
	}
	newMap[tp] = newCfg

	// 5. 原子替换 (Publish)
	globalConfigs.Store(newMap)
}

// -----------------------------------------------------------------------------
// 辅助函数
// -----------------------------------------------------------------------------

// ParseFromJSON 手动解析辅助函数 (无锁)
func ParseFromJSON[T any](typeName string, data []byte) (G[T], error) {
	// 直接原子读取，无锁
	iType := reflect.TypeFor[T]()
	config := loadConfig(iType)

	if config == nil {
		return nil, fmt.Errorf("xjson: type %v is not binding", iType)
	}

	unmarshalFn := json.Unmarshal
	if config.Option.UnmarshalAPI != nil {
		unmarshalFn = config.Option.UnmarshalAPI
	}

	detailType := config.UnMarshalMapping[typeName]
	if detailType == nil {
		return nil, fmt.Errorf("xjson: type alias '%s' is not registered", typeName)
	}

	impValue := reflect.New(detailType)
	if err := unmarshalFn(data, impValue.Interface()); err != nil {
		return nil, err
	}

	v, ok := impValue.Elem().Interface().(T)
	if !ok {
		return nil, fmt.Errorf("xjson: cannot convert %v to interface %v", detailType, iType)
	}

	// 复用 NG 逻辑，这里无需再查 config，直接应用钩子更好，但复用 NG 最简单
	// 考虑到 NG 内部会再查一次 config (极快)，为了代码简洁直接调用 NG
	return NG(v), nil
}

// 内部辅助
func applyInitializer[T any](data T) T {
	config := loadConfig(reflect.TypeFor[T]())
	return applyInitializerWithConfig(data, config)
}

func applyInitializerWithConfig[T any](data T, config *bindConfig) T {
	if config != nil && config.Option.Initializer != nil {
		res := config.Option.Initializer(data)
		if res != nil {
			if v, ok := res.(T); ok {
				return v
			}
		}
	}
	return data
}

// 配置函数选项
func WithTypeKey(typeKey string) OptionFunc {
	return func(opt *BindOption) { opt.TypeKey = typeKey }
}
func WithValueKey(valueKey string) OptionFunc {
	return func(opt *BindOption) { opt.ValueKey = valueKey }
}
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
func WithJSONHandler(m MarshalFunc, u UnmarshalFunc) OptionFunc {
	return func(opt *BindOption) {
		opt.MarshalAPI = m
		opt.UnmarshalAPI = u
	}
}
