# xfield 示例

从仓库根目录运行，无需额外配置或依赖：

| 场景 | 命令 | 展示的功能 |
| --- | --- | --- |
| [基础模型](basic/main.go) | `go run ./xfield/examples/basic` | 直接声明字段、完整/局部校验、过滤字段、错误与路径查找 |
| [自定义字段](custom_field/main.go) | `go run ./xfield/examples/custom_field` | 直接实现接口、Parse/Present 强类型参数、嵌套展示、扩展 string |
| [公共定义与引用](definitions/main.go) | `go run ./xfield/examples/definitions` | 用户定义 Dialog、对象与数组复用、编译引用、校验与路径查找 |
| [结合 xjson](xjson/README.md) | `go run ./xfield/examples/xjson` | G 恢复多态字段、前端公共定义 JSON、初始化嵌套容器、编译引用、校验与 JSON 往返 |
| [OrderedMap 评估](orderedmap/README.md) | 在该目录运行 `go test -race ./...` | 独立模块测试 wk8 v2.1.8 的顺序、JSON、xjson/xfield 兼容性及性能 |
| [嵌套映射](mapping/main.go) | `go run ./xfield/examples/mapping` | 默认点路径、数组 Children、缺失策略、完整模型检查 |
| [自定义取值](value_resolver/main.go) | `go run ./xfield/examples/value_resolver` | ValueResolver 接入 JSON Pointer、特殊 key、区分 null 与缺失 |

每个目录是独立的 package main，演示实现保留在该目录中。字段声明的序列化由应用层处理；xjson 示例演示这个边界，其余示例中的 JSON 只用于输入或展示数据值。JSON Pointer 示例不支持 URI fragment 形式；传入映射的 From 需非空，因此整个根对象的空指针不用于 Mapping.From。
