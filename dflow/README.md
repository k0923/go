# dflow

dflow 是节点级持久化的 DAG 执行框架，支持并发、端口分支、挂起恢复、子流程和循环。节点接收解析后的输入，返回输出或挂起对象。框架管理节点状态、租约、依赖到达和通知原子性；业务管理定义发布、类型构造、存储格式和外部操作。

## 使用

```go
graph, err := dflow.Compile(dflow.Definition{
    Version: "order-v1",
    Nodes: []dflow.NodeSpec{
        {ID: "calculate", Node: dflow.NodeFunc(
            func(ctx context.Context, input map[string]any) (dflow.Result, error) {
                return dflow.Result{Output: input["price"].(int) * 2}, nil
            }),
            Inputs: dflow.Parameters{"price": dflow.Input("price")},
        },
        {ID: "out", Node: &dflow.EndNode{},
            Inputs: dflow.Parameters{"total": dflow.Ref("calculate")}},
    },
    Edges: []dflow.Edge{{SourceNodeID: "calculate", TargetNodeID: "out"}},
})
if err != nil { return err }
engine, err := dflow.NewEngine(graph, memory.New(), dflow.Options{})
if err != nil { return err }
state, err := engine.Run(ctx, map[string]any{"price": 21})
```

`Compile` 校验节点非空、拓扑无环、边和 ID 唯一，以及输入引用是否来自祖先节点。配置实现 `NodeValidator.Validate() error` 时也会提前校验。`Definition.Version` 必填，业务必须在节点实现、参数或拓扑变化时更新版本；`Manifest.Hash` 使用该版本作为执行身份，不自动序列化定义计算 hash。

`Graph` 可复用；节点配置和 Parameter 不可变，可能被多个执行并发使用。业务依赖直接注入节点。函数节点可直接使用 `NodeFunc`。

## 节点

```go
type Node interface {
    Run(context.Context, map[string]any) (Result, error)
}
type ResumableNode interface {
    Node
    Resume(context.Context, map[string]any, ResumeInput) (Result, error)
}
```

`Result.Output` 支持存储实现接受的 Go 值，不做 JSON 规范化。`Ports == nil` 激活所有出边；非 nil 空数组不激活出边；非空数组选择对应端口。未激活节点标记为 skipped，继续传播依赖到达；对 skipped 节点输出的引用返回 `ErrMissingValue`。同一前驱的多条端口边只计一次依赖到达。

默认实现：

| 实现 | 功能 |
| --- | --- |
| `StartNode` | 无显式 Inputs 时返回完整工作流输入 |
| `EndNode` / `PassthroughNode` | 返回节点输入 |
| `ConditionNode` | 根据布尔输入 condition 选择 true/false 端口 |
| `JSONParseNode` | 解析 json 字符串输入 |
| `CodeNode` | 通过 `NewCodeNode(CodeConfig)` 编译 JS，每次执行创建独立 VM |
| `HTTPNode` | 请求 HTTP 服务，返回 body、headers、statusCode |
| `SubflowNode` | 声明子流程挂起 |
| `LoopNode` | 按 foreach/until 模式逐轮调用子流程 |

JS 定义 `main({params})`（也支持 `run(params)`）；没有文件、网络或事件循环。脚本执行和输出转换均受超时约束，不支持 pending Promise。HTTP 默认 GET、30 秒、8 MiB 响应上限；重试要求幂等方法、Idempotency-Key 或显式 RetryNonIdempotent。HTTP 非 2xx 仍返回响应供业务判断；默认使用 `http.DefaultClient`，可直接设置 Client。

JSONParse、HTTP、JS 和 JSON 模板内部的编码是节点功能，不是框架的定义或状态存储协议。

## 参数

```go
type Parameter interface {
    References() ([][]string, error)
    Resolve(context.Context, ValueResolver) (any, error)
}
type ValueResolver interface {
    Resolve(context.Context, ...string) (any, error)
}
```

三个默认实现分别只承载对应数据：

| 实现 | 数据 | 便捷函数 |
| --- | --- | --- |
| `ConstantParameter` | Value | `Constant(value)` |
| `ReferenceParameter` | Path | `Ref(nodeID, path...)`、`Input(path...)` |
| `TemplateParameter` | Text、JSON | `Template(text)`、`PlainTemplate(text)` |

`Ref("node", "items", "0", "name")` 读取祖先输出；`Input("name")` 读取工作流输入。路径支持 string key 的 map、数组/切片索引和导出 struct 的 Go 字段名。保留原始 Go 类型；存在的 nil 值有效，缺失路径返回 `ErrMissingValue`。

`Template("hello {{node.name}}")` 插入 JSON 字面量，字符串包含引号；`PlainTemplate` 插入纯文本，nil 渲染为空字符串。只处理原模板一次，取值中的 `{{...}}` 不会再次解析。

自定义 Parameter 需在 `References` 中声明所有读取路径；Compile 校验祖先关系，执行时只加载这些依赖。返回空路径或 nil Parameter 会拒绝执行。节点和参数必须把收到的输入及上游值视为只读；需要修改时由业务复制。

## 挂起与恢复

```go
type Suspension interface { Type() string }

// 在 ResumableNode.Run 中：
return dflow.Result{Suspend: &dflow.ValueSuspension{
    Kind: "transcode",
    Data: job,
}}, nil
```

业务也可直接定义审批、上传等类型实现 Suspension。框架不注册类型，也不要求 MarshalJSON。挂起与 Output/Ports 互斥，只有 ResumableNode 可挂起；typed nil 和空 Type 会拒绝执行。

框架为每轮挂起生成 token。Infrastructure 原子保存 suspended 节点状态、`SuspensionState{Token, Payload}` 和携带同一具体类型的 Request 通知。业务消费 Request 并按 Type 路由，接受操作后 Ack；外部操作使用业务 run scope + nodeID + token 作为幂等键。

`engine.Wakeup(ctx, nodeID, token, response)` 原子保存首次回调和 Wake 通知。重复/过期回调返回 false，未知 token 返回 `ErrWaitNotFound`。业务消费 Wake 后调用 `ResumeNode(ctx, nodeID, token)`，成功后 Ack。也可直接 `ResumeNode(ctx, nodeID, token, response)`，包括一个显式 nil 响应。

恢复时调用 Resume，不重新进入 Run；`ResumeInput` 包含 Token、恢复后的具体 Suspension 和 Response。Resume 可再次挂起，产生新 token。失败或进程中断保留挂起数据和响应以便重试。节点副作用仍需幂等，框架不能保证外部操作 exactly-once。

## Infrastructure 和恢复

Infrastructure 绑定一个业务执行 scope，不在每个方法中传递 RunID/TenantID。实现负责：

- 租约 generation fencing、节点版本 CAS 和重复终态提交保护。
- 原子保存节点结果、依赖到达、就绪标记和 Request 通知。
- 原子保存首次响应和独立 Wake 通知。
- 存储编码、具体类型恢复和快照隔离；不支持的值应返回错误。

`Run` 初始化或校验已有执行，消费就绪任务；`Drain` 恢复已有执行；`ResumeNode` 只驱动指定节点及可达后继。Engine 只按需读取节点和原输入路径，不加载整份状态或定义。返回 State.Nodes 仅包含本次驱动触及的节点；nil error 表示当前没有可执行任务。

没有工作流整体 Status。业务通过 `GetNode` 或可选 `SnapshotReader.Load` 判断完成条件。`Options` 控制并发数（默认 4）、节点超时（默认 5 分钟）和租约选项。

`DefinitionProvider` 按发布版本返回可执行的 PublishedNode。业务从配置构造 Node、Parameter 并注入依赖；重启后按节点重建。编译 Graph 本身也实现这个接口，可传给 `NewEngine`。

`memory.New()` 提供内存实现，通过复制导出字段、map、slice 和 pointer 隔离快照，保留具体类型并支持循环引用。函数和未导出字段共享，必须不可变；含私有可变对象的业务应使用自有存储实现。

`sqlite.Open(ctx, path, scope, codec)` 提供持久化事务、索引和 outbox，必须传入业务 `sqlite.Codec`：

```go
type Codec interface {
    Encode(any) ([]byte, error)
    Decode([]byte, any) error
}
```

Codec 必须恢复 Suspension、回调数据，以及发布记录中的 Node/Parameter 的具体类型；节点中的服务客户端或函数由业务重建。可使用包含 `xjson.G[dflow.Node]`、`xjson.G[dflow.Parameter]`、`xjson.G[dflow.Suspension]` 的业务 DTO。dflow 不依赖 xjson，不提供默认编码协议。更换协议时由业务迁移存量记录或使用新数据库。

## 子流程与循环

SubflowNode 配置 Publication 和 OutputNode，以解析后的节点输入发起 ChildSuspension。LoopNode 同样配置发布和返回节点，另有 Mode（foreach 默认，或 until）及 MaxIterations（默认 1000）。

ChildSuspension 和 LoopSuspension 实现 `ChildInvocation.ChildCall()`。业务将 Request 通知交给 `ChildDispatcher.Handle`；Bind 返回子流程 Engine 和独立 Infrastructure，并固定子发布版本。子 scope 使用 `ChildExecution.ScopeKey()`，包括父 scope、节点 ID 和本轮 token，避免重投重复创建子执行。

foreach 输入 items，每轮子输入为 index、item、value、params，累计子输出数组。until 输入 value，要求子输出 `{done: bool, value: any}`。轮次和累计值保存在 LoopSuspension，恢复后继续下一轮。

Handle 返回 false 表示子流程尚未完成，保留 Request；子流程推进后重试。返回 true 表示父响应已保存或 token 已过期，可 Ack。子输出通过具体 `ChildResponse` 返回；存储必须保留该类型。限制嵌套深度和累计数据大小，大结果可使用对象存储引用。

运行示例见 [examples](examples/README.md)。

```sh
go test ./dflow/...
go test -race ./dflow/...
go vet ./dflow/...
```
