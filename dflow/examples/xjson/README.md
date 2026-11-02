# 业务侧 xjson 编码

在模块根目录运行：

```sh
go run ./dflow/examples/xjson
go test ./dflow/examples/xjson
```

示例创建临时 SQLite 数据库，运行结束后自动删除，无需外部服务。输出：

```text
JSON 定义构建、挂起和恢复成功：
{
  "approved": true,
  "json": "{\"approved\": true}",
  "summary": "结果：财务 确认金额 42",
  "total": 42
}
```

[workflow.json](workflow.json) 是业务定义：JS 计算金额 → 财务确认并挂起 → 收到回调后输出结果。流程分三步，模拟初始化进程、业务回调处理进程和继续执行 worker：

1. xjson 解码定义 DTO，恢复节点配置和参数配置；配置构造可执行节点、编译 JS 并注入服务。`nodes.Compile` 通过核心 `dflow.Compile` 校验拓扑和引用，SQLite 初始化 run 状态，执行到 review 挂起。
2. 重新打开数据库，读取 review 节点的 `Suspended` 状态和已持久化 output。业务系统据此完成审批并得到回调值。
3. 再次打开数据库，应用从 JSON 重新构建相同 publication，传入回调值并显式驱动 review 节点，随后 end 被推进。

[schema.go](schema.go) 中的 JSON tags、类型别名与 `xjson.Bind` 都由业务定义。脚本和结束节点使用 [参考节点包](../nodes/README.md)，review 节点由本示例实现；dflow 核心只通过 `RuntimeNode` 调用编译后的 publication：

- `xjson.G[NodeConfig]` 保存节点配置；`BuildNode` 显式构造节点，可返回 JS 编译或配置错误。脚本节点保存源码，review 节点保存 approver，服务实例在每次构建 publication 时注入。
- `xjson.G[ParamConfig]` 绑定业务参数适配器，构造 `nodes.Const`、`nodes.Input`、`nodes.Ref` 和模板参数。节点与参数采用扁平格式，例如 `{"type":"reference","path":["calculate","total"]}`。
- 可选引用通过业务 DTO 的 `optional` 字段表达。本例第一次执行时 `approved` 不存在，review 返回 `AsyncResult{Done:false}`；第二次执行时传入 `approved:true`，review 返回完成结果。

xjson 每个接口类型只能 Bind 一次，业务在初始化阶段完成注册。当前 SQLite 示例使用存储适配器的 gob 编码保存节点状态；JSON 只负责业务定义格式，不定义 dflow 的持久化格式。

输入、输出及 Constant.Value 的 `any` 值在这个示例中遵循普通 JSON 语义：数字恢复为 float64，struct 恢复为对象；示例因此使用 float64 输入。需要保留这些位置的 Go 具体类型时，应扩展业务 DTO 或另选存储编码。JSON 序列化服务客户端、函数或编译后的 JS 程序不能恢复可执行依赖，应用应保存配置并重建。

dflow 的 Go 类型不定义 JSON 存储格式。业务可以调整 DTO 字段名、布局和类型别名；修改已有数据库的编码格式时，业务负责版本兼容与迁移。
