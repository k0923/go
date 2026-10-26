# 业务侧 xjson 编码

在模块根目录运行：

```sh
go run ./dflow/examples/xjson
go test ./dflow/examples/xjson
```

示例创建临时 SQLite 数据库，运行结束后自动删除，无需外部服务。输出：

```text
JSON 定义发布、挂起和类型恢复成功：
{
  "approved": true,
  "json": "{\"approved\": true}",
  "summary": "结果：财务 确认金额 42",
  "total": 42
}
```

[workflow.json](workflow.json) 是业务定义：JS 计算金额 → 财务确认并挂起 → 收到回调后输出结果。流程分三次打开数据库，模拟发布进程、回调处理进程和恢复 worker：

1. xjson 解码定义 DTO，恢复节点配置和 Parameter；配置构造可执行节点、编译 JS 并注入服务。`dflow.Compile` 校验拓扑、版本和引用，SQLite 按节点保存发布记录，执行到 review 挂起。
2. 重新打开数据库，读取 Request 通知。`xjson.G[dflow.Suspension]` 将请求恢复成 `*ReviewRequest`；`Wakeup` 保存 `*ReviewReply` 回调并生成 Wake 通知。
3. 再次打开数据库，从已保存的 publication ID 按需重建节点和服务。`Resume` 收到原具体类型的挂起数据和回调，继续驱动 end。

[schema.go](schema.go) 中的 JSON tags、类型别名与 `xjson.Bind` 都由业务定义。脚本和结束节点使用 [参考节点包](../nodes/README.md)，review 节点由本示例实现；dflow 核心只通过节点契约调用它们：

- `xjson.G[NodeConfig]` 保存节点配置；`BuildNode` 显式构造节点，可返回 JS 编译或配置错误。脚本节点保存源码，review 节点保存 approver，服务实例在每次打开数据库后注入。
- `xjson.G[dflow.Parameter]` 绑定业务参数适配器，复用 Constant、Reference、Template 的运行行为。节点与参数采用扁平格式，例如 `{"type":"reference","path":["calculate","total"]}`。
- `xjson.G[dflow.Suspension]` 和 `xjson.G[Callback]` 保存具体业务请求和回调，采用默认的 `{"type":"review","data":{...}}` 格式。`Callback` 是本应用的回调接口。

[codec.go](codec.go) 实现业务 `sqlite.Codec`，显式转换发布记录、节点状态、执行元数据和回调。它保留 `Ports` 的 null/空数组差异，拒绝未支持的节点、参数、回调和存储值。xjson 每个接口类型只能 Bind 一次，业务在初始化阶段完成注册。

输入、输出及 Constant.Value 的 `any` 值在这个示例中遵循普通 JSON 语义：数字恢复为 float64，struct 恢复为对象；示例因此使用 float64 输入。需要保留这些位置的 Go 具体类型时，应扩展业务 DTO 或另选存储编码。JSON 序列化服务客户端、函数或编译后的 JS 程序不能恢复可执行依赖，应用应保存配置并重建。

dflow 的 Go 类型不定义 JSON 存储格式。业务可以调整 DTO 字段名、布局和类型别名；修改已有数据库的编码格式时，业务负责版本兼容与迁移。
