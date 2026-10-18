# xfield 示例

从仓库根目录运行，无需额外配置或依赖：

| 场景 | 命令 | 展示的功能 |
| --- | --- | --- |
| [基础模型](basic/main.go) | `go run ./xfield/examples/basic` | 字段解码、完整/局部校验、过滤字段、错误与路径查找 |
| [自定义字段](custom_field/main.go) | `go run ./xfield/examples/custom_field` | 简化注册、Parse/Present 强类型参数、嵌套展示、替换系统 string |
| [自定义字段 JSON 往返](serialization/main.go) | `go run ./xfield/examples/serialization` | 构造自定义 email 字段、打印 Schema JSON、恢复顶层及嵌套字段配置、验证重新编码和数据解析 |
| [嵌套映射](mapping/main.go) | `go run ./xfield/examples/mapping` | 默认点路径、数组 Children、缺失策略、完整模型检查 |
| [自定义取值](value_resolver/main.go) | `go run ./xfield/examples/value_resolver` | ValueResolver 接入 JSON Pointer、特殊 key、区分 null 与缺失 |

每个目录是独立的 package main，演示实现保留在该目录中。JSON Pointer 示例不支持 URI fragment 形式；传入映射的 From 需非空，因此整个根对象的空指针不用于 Mapping.From。
