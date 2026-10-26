# xfield

xfield 用字段接口描述结构化数据，提供声明校验、数据清洗、展示转换和数据映射。支持公共定义与引用、自定义字段和取值规则，只依赖 Go 标准库。

字段声明的序列化由使用方负责，可以在应用层接入 xjson 或其他编解码器。xfield 不负责字段 JSON 解码、类型工厂注册、存储或网络请求。

[结合 xjson 的可运行示例](examples/xjson/README.md)演示前端 JSON 配置如何恢复具体字段、编译公共引用，并在保存时保留 definitions/ref。

## 快速使用

```go
schema := xfield.Schema[struct{}]{
	"name": &xfield.StringField[struct{}]{
		Meta: xfield.Meta{Required: true},
	},
	"tags": &xfield.ArrayField[struct{}]{
		Items: &xfield.StringField[struct{}]{},
	},
}
clean, err := xfield.ValidateModel(map[string]any{
	"name": "张三", "tags": []any{"Go"}, "extra": true,
}, schema, struct{}{})
// clean 只包含 name、tags。
```

完整可运行示例位于 [examples](examples/README.md)。

## 字段接口与模型

```go
type Field[P any] interface {
	Type() string
	Parse(data any, params P) (any, error)
}

type Schema[P any] map[string]Field[P]
```

`Schema[P]` 将字段名映射到声明，直接放入具体字段，无需 Wrap 或 FieldRef。字段名由 map 的 key 提供，Meta 不重复保存 Key。`ObjectField.Properties` 是 Schema，`ArrayField.Items` 是 Field。路径查找直接按 key 访问；校验、解析和展示按字段名排序，保持执行顺序与首个错误稳定。

前端 definitions、fields、properties 可以保持数组，用于交互排序。应用层转换为运行时 map，转换时检查重复名称；编辑和保存保留原始数组。完整转换过程见 xjson 示例。

| 类型 | 支持的规则 |
| --- | --- |
| `StringField[P]` | Unicode 字符数 MinLength/MaxLength、Pattern 正则、Enum 枚举 |
| `NumberField[P]` | Min/Max 范围、Enum 枚举；支持 Go 数值类型和 json.Number，拒绝非有限数 |
| `BooleanField[P]` | 布尔类型检查 |
| `ArrayField[P]` | Items 元素声明、MinItems/MaxItems、递归解析 |
| `ObjectField[P]` | 按名称索引的 Properties、子字段 required、递归解析 |
| `RefField[P]` | 引用 SchemaDocument 中的公共定义，编译后使用 |

`Meta` 包含 ID、Label、Required。Required 表示键必须存在；内置字段允许显式 nil 清空。NumberField 不限制整数，时间先后和角色 ID 等跨字段业务规则由应用处理。

`ValidateSchema` / `ValidateField` 检查空属性名、空 items、非法规则和嵌套深度。数组输入中的重复名称由转换层检查，map 自身只保存每个名称的一条声明。`ValidateModel` 检查完整模型，`ValidatePartial` 只跳过顶层 required；提供的嵌套对象仍须满足自身 required。两者过滤未声明字段，返回新容器。

数字比较避免将大整数转为 float64。StringField 的 Config.EnumOptions 是展示元数据，数据约束使用 Enum。

## 公共定义与引用

```go
doc := xfield.SchemaDocument[struct{}]{
	Definitions: map[string]xfield.Field[struct{}]{
		"dialog": &xfield.ObjectField[struct{}]{
			Properties: xfield.Schema[struct{}]{
				"content": &xfield.StringField[struct{}]{
					Meta: xfield.Meta{Required: true},
				},
			},
		},
	},
	Fields: xfield.Schema[struct{}]{
		"dialog": &xfield.RefField[struct{}]{
			Meta: xfield.Meta{Required: true},
			Ref: "dialog",
		},
		"mappedDialogs": &xfield.ArrayField[struct{}]{
			Items: &xfield.RefField[struct{}]{Ref: "dialog"},
		},
	},
}
schema, err := xfield.CompileDocument(doc)
// 检查 err 后，将 schema 交给 ValidateModel / PresentModel / ApplyMappings。
```

公共定义的名称是普通字符串，例如 `dialog`，要求非空且不包含空白。定义可为对象、数组、叶子字段或其他定义的引用。引用位置的 Meta 决定该位置的 Label、Required 等，不覆盖公共定义；使用位置的字段名由 Schema 的 key 决定。

CompileDocument 检查所有定义，包括未使用的定义；拒绝缺失引用、循环引用和过深结构。它返回可直接使用的 Schema，保留被引用字段的类型、Parse、Present 及容器遍历行为。未编译的引用不能用于校验。

保留原始 SchemaDocument 用于编辑和保存，使用编译结果处理数据。编译不修改原始声明，但可能共享不可变的叶子字段和公共定义；声明使用期间应保持不变。

## Parse、Present 与自定义字段

应用自定义字段直接实现 Field；可选地实现 Presenter，无需注册：

```go
type ResourceField struct { xfield.Meta }

func (*ResourceField) Type() string { return "resource" }

func (*ResourceField) Parse(data any, params Params) (any, error) {
	// 检查、清洗并返回内部值，不修改输入。
	return data, nil
}

func (*ResourceField) Present(data any, params Params) any {
	// 例如将资源 ID 转换为访问 URL。
	return data
}
```

P 是使用方定义的强类型参数，可包含 context、服务或 URL 生成器。数组和对象递归传递 params。无需参数时使用 struct{}。Parse 和 Present 可传入不同的 P 值。

`PresentModel` 保留未声明字段并复制遍历到的容器，不深拷贝任意应用对象。没有 Presenter 的字段保留原值。Present 不做校验、不返回 error，不保证是 Parse 的逆操作。

`MetaProvider` 开放元数据，嵌入 Meta 即可实现。`ObjectProvider[P]` / `ArrayProvider[P]` 开放容器遍历，`SchemaValidator` 开放自定义声明检查，`ReferenceProvider` 开放引用名称。

自定义容器如果包含引用，应实现 `FieldCompiler[P]`：通过传入的 compile 函数编译每个子字段，返回保留自身类型、元数据、规则和 Parse/Present 行为的新容器。普通叶子字段、没有引用的自定义容器无需实现。xfield 不通过反射复制应用对象。

自定义字段自行保证并发安全、输入不被修改。若 Parse 有外部副作用，重复调用的幂等性由应用负责。

## 映射与取值

```go
out, err := xfield.ApplyMappings(input, []xfield.Mapping{
	{From: "person.full_name", To: "name"},
}, schema, params, xfield.MappingOptions{
	MissingPolicy: xfield.MissingSkip,
})
```

Mapping.From 指定输入位置，To 指定目标字段，Children 表示对象/数组内的相对映射。ApplyMappings 先取值并构造独立数据，再按部分模型解析，避免单次映射重复解析子字段。需要完整模型时，再使用 ValidateModel；自定义解析有副作用时应注意额外验证会再次调用 Parse。

`ValueResolver` 决定如何从输入取值，字段 Parse 决定目标值是否合法及如何转换：

```go
type ValueResolver interface {
	Resolve(input any, mappingType MappingType, path string) (value any, exists bool, err error)
}
```

默认 PathResolver 支持点路径。exists=false 表示路径缺失；value=nil、exists=true 表示显式 null。MissingSkip 跳过缺失路径，MissingError 返回错误；显式 null 仍传入 Parse。

通过 `ValueResolverFunc` 或自己的实现可以接入 JSON Pointer、GJSON/JSONPath、业务列名。MappingTypeGJSON 需应用提供 Resolver，默认 Resolver 不执行 GJSON 表达式。ValidateMappings 支持目标路径和可选的简单 source 路径检查，不静态推断任意表达式。

映射输入仍可使用 JSON 字符串、[]byte、json.RawMessage，它们作为数据值预先转换为 map/array/json.Number，保留数字精度。这与字段声明的序列化无关。Children 中的 Resolver 接收当前子对象或数组元素。[JSON Pointer 示例](examples/value_resolver/main.go)演示特殊 key、null 和缺失路径。

## 路径、错误与迁移

`FindFieldByPath` / `FindFieldInField` 查找字段声明；`GetData` 读取数据，缺失返回 nil。`SetData` 原地修改 map 或不扩容的根 []any；扩容/替换根切片使用 SetDataValue 并保留返回值。

点路径不支持含点的 key 或括号表达式。写入使用 map[string]any/[]any；读取和解析支持命名 map/slice。递归深度和稀疏数组索引有上限。

`ErrInvalidSchema`、`ErrInvalidValue` 区分声明与数据错误。`PathError` 携带 assets[0].image 等路径并保留原始 error，支持 errors.Is/As。

旧版 `Wrap` / `FieldRef` 改为直接使用 Field；`.Value()` 改为直接访问字段。Registry、Factory、Decoder、FieldDecoder、字段 JSON 编解码及基于 JSON 的 FieldEqual 已移除，声明编解码交给应用层。

旧的 Schema 切片改为 `map[string]Field[P]`；将 `Meta.Key` 移至 map key，按位置的访问改为按字段名访问。Meta.Key 和 KeyOf 已移除。前端字段数组的 key 保留在应用 DTO 中。
