# xfield 示例

从仓库根目录运行，无需额外配置或依赖：

| 场景 | 命令 | 展示的功能 |
| --- | --- | --- |
| [基础模型](basic/main.go) | `go run ./xfield/examples/basic` | 直接声明字段、完整/局部校验、过滤字段、错误与路径查找 |
| [自定义字段](custom_field/main.go) | `go run ./xfield/examples/custom_field` | 直接实现接口、Parse/Present 强类型参数、嵌套展示、扩展 string |
| [公共定义与引用](definitions/main.go) | `go run ./xfield/examples/definitions` | 用户定义 Dialog、对象与数组复用、编译引用、校验与路径查找 |
| [结合 xjson](xjson/README.md) | `go run ./xfield/examples/xjson` | 前端定义与字段数组、递归转换为数组 Schema、重复检查、引用编译、保留数组顺序 |
| [多种 Schema 容器](containers/main.go) | `go run ./xfield/examples/containers` | 普通 map、有序 map、数组适配同一接口，容器决定编码形式和执行顺序 |
| [OrderedMap 评估](orderedmap/README.md) | 在该目录运行 `go test -race ./...` | 独立模块测试 wk8 v2.1.8 的顺序、JSON、xjson/xfield 兼容性及性能 |
| [嵌套映射](mapping/main.go) | `go run ./xfield/examples/mapping` | 路径分段、数组 Children、完整模型检查 |
| [自定义取值](value_resolver/main.go) | `go run ./xfield/examples/value_resolver` | MappingFunc 接入 JSON Pointer，并由业务处理 null 与缺失 |

每个场景目录是独立的 package main，演示实现保留在示例目录中；`internal/schema` 提供示例共享的普通 map 适配器。字段声明的序列化由应用层处理；xjson 示例演示多态解码，containers 示例展示容器的编码形式。JSON Pointer 示例不支持 URI fragment 形式。
