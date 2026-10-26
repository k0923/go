# xfield 结合 xjson

从仓库根目录运行：

```sh
go run ./xfield/examples/xjson
go test ./xfield/examples/xjson
```

前端交互使用数组，底层 xfield 使用 map。示例将两种结构通过显式转换连接起来。

[schema.json](schema.json) 是前端提交的完整 DialogResult 配置：definitions 是 `{name, field}` 数组，fields 和对象的 properties 是带 key 的平铺字段数组。数组元素 items 是一个字段配置。

```json
{
  "definitions": [
    {
      "name": "dialog",
      "field": {
        "type": "object",
        "properties": [
          {"type": "string", "key": "content", "required": true}
        ]
      }
    }
  ],
  "fields": [
    {"type": "ref", "key": "dialog", "ref": "dialog"},
    {
      "type": "array",
      "key": "mappedDialogs",
      "items": {"type": "ref", "ref": "dialog"}
    }
  ]
}
```

[schema.go](schema.go) 定义应用层 FieldConfig 接口与 JSON DTO：

```go
type FieldJSON = xjson.G[FieldConfig]
type SchemaJSON []FieldJSON

type DefinitionJSON struct {
    Name  string    `json:"name"`
    Field FieldJSON `json:"field"`
}

type DocumentJSON struct {
    Definitions []DefinitionJSON `json:"definitions,omitempty"`
    Fields      SchemaJSON       `json:"fields"`
}
```

处理流程：

1. `json.Unmarshal` 解码 DocumentJSON，每个 G 根据 type 恢复具体 JSON 配置。
2. `wire.Document()` 显式转换，返回 `(SchemaDocument, error)`。definitions 按 name 转为 map，fields/properties 按 key 转为 Schema map，items 递归转换为 Field。空名称、重复名称、null 字段和过深结构会报错，不会静默覆盖。
3. `CompileDocument` 解析公共定义引用，检查定义名、缺失或循环引用及字段规则。
4. `ValidateModel` 校验 DialogResult，接受 dialog=null 和多段 mappedDialogs，过滤未声明属性。
5. `json.Marshal(wire)` 保存原始数组配置，保留公共定义、引用及数组顺序。

JSON 层的 FieldConfig 实现 BuildField，转换后得到普通 xfield.Field。字符串、数字和布尔配置复用核心规则结构；对象、数组和引用配置转换为相应的原生 xfield 声明。转换不依赖 WithInitializer 或自定义 FieldCompiler，并可以直接返回错误。

前端字段的 key 保存在 KeyJSON 中，转换后成为 Schema 的 map key；核心 Meta 不保存 Key。definitions 的 name 同样只用于定义表索引，数组 items 无需 key。

`Bind` 配合 `WithFlatLayout` 保持 `{"type":"string","key":"speaker",...}` 格式。绑定表只包含 string、number、boolean、object、array、ref；用户的 dialog 随配置传入，不加入全局绑定。绑定在应用启动时执行一次，同一个 Go 接口类型不能重复 Bind。

原始 DocumentJSON 用于前端编辑和保存，运行时 Schema 用于查找、校验、展示和映射。转换和编译不会改变原始数组，JSON 往返保留 definitions、fields 和嵌套 properties 的数组顺序。map 不承担前端展示顺序，底层按属性名排序执行校验；JSON 空白和对象属性顺序不保证与输入完全一致。
