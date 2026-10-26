# xfield 结合 xjson

从仓库根目录运行：

```sh
go run ./xfield/examples/xjson
go test ./xfield/examples/xjson
```

[schema.json](schema.json) 是前端提交的完整 DialogResult 配置。`user:dialog` 在 definitions 中定义一次，dialog 和 mappedDialogs.items 引用它。

示例处理流程：

1. `json.Unmarshal` 解码 DocumentJSON，每个 `xjson.G[xfield.Field[Params]]` 根据 type 恢复具体字段。
2. `WithInitializer` 将对象的 properties、数组的 items 转换为运行时 Field 接口。
3. `wire.Document()` 生成 SchemaDocument，`CompileDocument` 解析公共定义引用。
4. `ValidateModel` 校验 DialogResult，接受 dialog=null 和多段 mappedDialogs，过滤未声明属性。
5. `json.Marshal(wire)` 保存原始配置，再解码、编译并验证相同数据。

[schema.go](schema.go) 是应用层适配。DocumentJSON、ObjectJSON、ArrayJSON 中每个多态字段位置都使用 G；原生 xfield.Field 接口保持不变。对象和数组适配器还实现 FieldCompiler，让嵌套引用可以编译。RefJSON 负责 ref 的 JSON 字段名。

`Bind` 配合 `WithFlatLayout` 保持 `{"type":"string","key":"speaker",...}` 这样的平铺格式，`WithInitializer` 在子字段解码后建立容器的运行时属性。绑定在应用启动时执行一次，同一个接口类型不能重复 Bind。添加应用自定义字段时，将它加入这一张绑定表即可。

绑定表只包含 string、number、boolean、object、array、ref 等系统字段种类。用户的 user:dialog 是配置里的公共定义，交由 xfield 解析引用，不需要加入 xjson 的全局绑定表。

保存的是原始 DocumentJSON；编译后的 Schema 用来处理数据。重新保存 DTO 会保留 definitions/ref，JSON 的空白和对象键顺序不保证与原文件相同。
