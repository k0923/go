# xfield

xfield 用字段声明描述结构化数据，提供校验、清洗、展示转换和数据映射。支持自定义字段和取值规则，只依赖 Go 标准库，不负责存储或网络请求。

## 快速使用

```go
registry := xfield.DefaultRegistry[struct{}]()
schema, err := registry.DecodeSchema([]byte(`[
	{"type":"string","key":"name","required":true,"minLength":1},
	{"type":"array","key":"tags","items":{"type":"string"}}
]`))
if err != nil {
	return err
}
clean, err := xfield.ValidateModel(map[string]any{
	"name": "张三", "tags": []any{"Go"}, "extra": true,
}, schema, struct{}{})
// clean 只包含 name、tags。
```

完整可运行示例位于 [examples](examples/README.md)。

## 基础字段与模型

| 类型 | 支持的规则 |
| --- | --- |
| `StringField[P]` | Unicode 字符数 MinLength/MaxLength、Pattern 正则、Enum 枚举 |
| `NumberField[P]` | Min/Max 范围、Enum 枚举；支持 Go 数值类型和 json.Number，拒绝非有限数 |
| `BooleanField[P]` | 布尔类型检查 |
| `ArrayField[P]` | Items 元素声明、MinItems/MaxItems、递归解析 |
| `ObjectField[P]` | 有序 Properties、子字段 required、递归解析 |

`Meta` 包含 ID、Key、Label、Required。`Schema[P]` 是有序 FieldRef 列表，通过 `Wrap(field)` 包装直接构造的字段。JSON 使用平铺的 `{"type":"string","key":"name",...}` 格式。

`ValidateSchema` / `ValidateField` 检查空 key、重复 key、空 items、非法规则和嵌套深度。解码只构造字段，模型解析会检查声明；也可在接收声明时显式验证。

`ValidateModel` 检查完整模型，`ValidatePartial` 只跳过顶层 required 检查。提供的嵌套对象仍须满足自身 required。两者过滤未声明字段，返回新容器，按声明顺序报告错误。Required 表示键必须存在；内置字段允许显式 nil 清空。

数字比较避免将大整数转为 float64；JSON 映射输入使用 json.Number 保留精度。StringField 的 Config.EnumOptions 是展示元数据，数据约束使用 Enum。

## Parse 与 Present

| 方法 | 方向与用途 |
| --- | --- |
| `Parse(data any, params P) (any, error)` | 输入 → 校验、清洗后的内部值；每个 Field[P] 必须实现 |
| `Present(data any, params P) any` | 内部值 → 展示值；可选的 Presenter[P] 接口 |

例如资源字段可以在 Parse 中将输入转换为资源 ID，在 Present 中将 ID 转为访问 URL。包本身不持久化值。Parse/Present 处理数据值；Registry/DecodeFieldJSON 处理字段声明，两条调用链独立。

```go
clean, err := xfield.ValidateModel(input, schema, params)
if err != nil {
	return err
}
display := xfield.PresentModel(clean, schema, params)
```

P 是使用方定义的强类型参数，可包含 context、服务或 URL 生成器。Schema、Registry 和字段采用同一个 P；Parse 和 Present 可以传入不同的 P 值。无需参数时使用 struct{}。

数组和对象递归传递 params；没有 Presenter 的字段展示时保留原值。PresentModel 保留未声明字段并复制遍历到的容器，不深拷贝任意应用对象。Present 不做校验、不返回 error，不保证是 Parse 的逆操作。可能失败的展示查询由应用层处理。

声明使用期间应保持不变，自定义字段自行保证并发安全和输入不被修改。若 Parse 执行外部副作用，重复调用的幂等性由应用负责。

## 自定义字段与注册

```go
type Params struct {
	ResourceURL func(string) string
}

type ResourceField struct { xfield.Meta }

func (*ResourceField) Type() string { return "resource" }

func (*ResourceField) Parse(data any, _ Params) (any, error) {
	if data == nil {
		return nil, nil
	}
	id, ok := data.(string)
	if !ok {
		return nil, fmt.Errorf("expect resource string")
	}
	return id, nil
}

func (*ResourceField) Present(data any, params Params) any {
	id, ok := data.(string)
	if !ok || params.ResourceURL == nil {
		return data
	}
	return params.ResourceURL(id)
}

registry := xfield.DefaultRegistry[Params]()
err := registry.Register(func() xfield.Field[Params] {
	return &ResourceField{}
})
```

`DefaultRegistry[P]()` 每次返回包含五种基础类型的独立实例；`NewRegistry[P]()` 和 Registry 零值均为空。需要替换系统 string 时，从空 Registry 注册自己的 string 和所需基础类型。`RegisterBuiltins()` 一次加入所有基础类型，任何冲突都会整批失败。重复类型报错。

工厂注册时调用一次读取 Type，每次解码再创建实例。工厂应轻量、无业务副作用、并发安全，返回新的非 nil 实例和稳定的非空 Type。直接构造字段进行解析无需注册。

普通字段自动使用 JSON 解码。需要自定义解码时实现：

```go
type FieldDecoder[P any] interface {
	DecodeFieldJSON(raw json.RawMessage, decoder xfield.Decoder[P]) error
}
```

接收者由工厂创建，方法只更新配置。自定义容器使用传入的 Decoder.DecodeField 解码 items，使用 DecodeSchema 解码 properties，继承同一 Registry 和递归深度限制。也可嵌入 ObjectField[P]/ArrayField[P] 复用解码；有额外配置时覆盖此方法并调用嵌入容器的实现。

`MetaProvider` 开放元数据，嵌入 Meta 即可实现。`ObjectProvider[P]` / `ArrayProvider[P]` 开放容器遍历，`SchemaValidator` 开放自定义声明检查。

包含自定义或替换类型时使用 `registry.DecodeSchema` / `DecodeField`。普通 `json.Unmarshal` 到 FieldRef 使用独立的 DefaultRegistry，只识别默认类型。包提供 JSON 编解码；其他存储格式由应用适配。

## 映射与 ValueResolver

Mapping.From 指定输入位置，To 指定目标字段，Children 表示对象/数组内的相对映射。

```go
out, err := xfield.ApplyMappings(input, []xfield.Mapping{
	{From: "person.full_name", To: "name"},
}, schema, params, xfield.MappingOptions{
	MissingPolicy: xfield.MissingSkip,
})
```

ApplyMappings 先取值并构造独立数据，再按部分模型解析，避免单次映射重复解析子字段。需要完整模型时，再使用 ValidateModel；自定义解析有副作用时应注意额外验证会再次调用 Parse。

`ValueResolver` 决定**如何从输入中取值**，字段 Parse 决定**目标值是否合法、如何转换**：

```go
type ValueResolver interface {
	Resolve(input any, mappingType MappingType, path string) (value any, exists bool, err error)
}
```

默认 PathResolver 支持点路径，例如 person.name、items.0.name，无需设置 Resolver。exists=false 表示路径缺失；value=nil、exists=true 表示显式 null。MissingSkip 跳过缺失路径，MissingError 返回错误，显式 null 仍传入字段 Parse。

实际扩展用途包括 JSON Pointer、GJSON/JSONPath 表达式以及业务自定义列名。使用 ValueResolverFunc 可以适配已有取值函数：

```go
resolver := xfield.ValueResolverFunc(func(input any, kind xfield.MappingType, path string) (any, bool, error) {
	if kind == "json_pointer" {
		return resolvePointer(input, path) // 应用实现，完整版本见示例。
	}
	return (xfield.PathResolver{}).Resolve(input, kind, path)
})
out, err := xfield.ApplyMappings(input, []xfield.Mapping{
	{Type: "json_pointer", From: "/profile/display~1name", To: "name"},
}, schema, params, xfield.MappingOptions{Resolver: resolver})
```

输入 `{"profile":{"display/name":"张三"}}` 会得到目标 name="张三"。这类含斜杠的属性可通过 JSON Pointer 的 ~1 转义访问。[可运行的 JSON Pointer 示例](examples/value_resolver/main.go)还演示了 null 与缺失路径的区别。

自定义 Resolver 接收 ApplyMappings 解码后的输入；JSON 字符串、[]byte、json.RawMessage 会预先转换为 map/array/json.Number。Children 中的 Resolver 接收当前子对象或数组元素。默认 Resolver 不执行 GJSON 表达式；MappingTypeGJSON 需应用注入对应实现。ValidateMappings 支持目标路径和可选的简单 source 路径检查，不静态推断任意自定义表达式。

## 路径与错误

`FindFieldByPath` / `FindFieldInField` 查找字段声明；`GetData` 读取数据，缺失返回 nil。`SetData` 原地修改 map 或不扩容的根 []any；扩容/替换根切片使用 SetDataValue 并保留返回值。

点路径不支持含点的 key 或括号表达式。写入使用 map[string]any/[]any；读取和解析支持命名 map/slice。递归深度和稀疏数组索引有上限。

`ErrInvalidSchema`、`ErrInvalidValue`、`ErrUnknownType` 区分错误类别。`PathError` 携带 assets[0].image 等路径并保留原始 error，支持 errors.Is/As。
