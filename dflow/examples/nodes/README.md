# 参考节点

本包供示例和业务实现参考。业务可以复制代码或实现自己的 `dflow.Node`／`dflow.ResumableNode`；dflow 核心不依赖本包，也不识别任何具体节点类型。

| 实现 | 行为 |
| --- | --- |
| `StartNode` | 透传显式绑定到 `input` 的完整工作流输入 |
| `EndNode`／`PassthroughNode` | 透传已解析的节点输入 |
| `ConditionNode` | 根据布尔输入 `condition` 选择 `true`／`false` 端口 |
| `JSONParseNode` | 解析顶层为对象的 `json` 字符串输入 |
| `CodeNode` | `NewCodeNode(CodeConfig)` 编译 JS，每次执行创建独立 VM，脚本返回对象 |
| `HTTPNode` | 请求 HTTP 服务，返回 body、headers、statusCode |
| `FuncNode` | 将 handler 适配为节点，并校验 handler 非空 |

起始节点由图中的唯一 root 决定，与 Go 类型无关。需要完整输入时显式绑定：

```go
dflow.NodeSpec{
    ID: "start",
    Node: &nodes.StartNode{},
    Inputs: dflow.Parameters{"input": dflow.Input()},
}
```

业务也可以使用普通节点逐字段绑定 `dflow.Input("price")`，无需 StartNode 或 EndNode。原来的 `dflow.StartNode` 等类型已移至本包；采用 StartNode 的定义需补上上述输入绑定。

JS 定义 `main({params})`（也支持 `run(params)`）；没有文件、网络或事件循环。脚本执行和输出转换均受超时约束，不支持 pending Promise。HTTP 默认 GET、30 秒、8 MiB 响应上限；重试要求幂等方法、Idempotency-Key 或显式 RetryNonIdempotent。非 2xx 响应交给业务判断，可设置 `Client` 自定义 transport 或 redirect 策略。

JSONParse、HTTP 和 JS 中的 JSON 编码由这些节点的行为决定；存储编码仍由业务 Codec 负责。
