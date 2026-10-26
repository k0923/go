# dflow

dflow 是节点级持久化的 DAG 执行框架。引擎每次只推进一个节点（`Drive`）：执行节点、持久化结果，并返回变就绪的下游节点 ID，交给调用方决定如何继续——同进程递归、入队交给消息队列、或停止。框架管理节点状态、租约、依赖到达和通知原子性；业务管理定义发布、类型构造、存储格式、调度编排和外部操作。

框架不提供并发 drain，也没有工作流整体状态；并发与失败处理都交给业务。执行权按节点划分：同一 run 的不同节点可以在多个 goroutine、worker 或高可用实例上同时 `Drive`。任一节点失败即为终态，调用方据此停止当前驱动（fail-fast）。

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

// 单步原语：Start 返回唯一 start 节点，Drive 执行一个节点并返回新就绪的下游。
start, err := engine.Start(ctx, map[string]any{"price": 21})
if err != nil { return err }
executed, err := engine.Drive(ctx, start)
if err != nil { return err }
// executed.Ready 是本步新就绪的下游节点，交给业务继续驱动或入队。
_ = executed
```

同进程把整张图驱动到静止，可直接用参考驱动包 [driver](driver/driver.go)：`driver.Run(ctx, engine, input)`、`driver.Drain(ctx, engine)`、`driver.Resume(ctx, engine, nodeID, token, response...)`。它只是对 `Start`/`Drive`/`Resume`/`Recover`/`Wakeup` 的串行 BFS 封装；生产系统可换成把就绪节点 ID 入队、多 worker 各自 `Drive`。

`Compile` 校验节点非空、拓扑无环、边和 ID 唯一、全图只有一个 root/start 节点，以及输入引用是否来自祖先节点。配置实现 `NodeValidator.Validate() error` 时也会提前校验。`Definition.Version` 必填，业务必须在节点实现、参数或拓扑变化时更新版本；`Manifest.Hash` 使用该版本作为执行身份，不自动序列化定义计算 hash。

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

原始输入缺失与上游引用缺失都会作为节点失败持久化为 Failed，并返回 NodeError；存储读取故障仍返回 InfrastructureError，保留 Running 供恢复。失败后的重试策略仍由业务决定。

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

`engine.Wakeup(ctx, nodeID, token, response)` 原子保存首次回调和 Wake 通知，不占用节点租约。重复/过期回调返回 false，未知 token 返回 `ErrWaitNotFound`。Wakeup 后该节点重新变为可驱动。

消费携带 token 的 Wake 时，使用 `engine.Resume(ctx, nodeID, token)`：它只推进该挂起轮次的一个节点，返回 Executed 和后继 Ready；旧 token 不会调用新一轮的 Resume，即使新回调已经到达。普通 `Drive(ctx, nodeID)` 仍按节点 ID 推进当前可驱动状态，适合普通任务投递和 Recover，未增加参数。

参考驱动 `driver.Resume(ctx, engine, nodeID, token, response...)` 封装了「可选投递回调 + 按 token 恢复该节点 + 驱动可达后继」，可传一个显式 nil 响应。重复投递当前 token 的回调仍可消费尚未执行的持久化响应。

`Resume` 使用普通 `ClaimNode` 取得受租约保护的快照，再核对 token。旧 token 可能短暂 claim/release 并增加 generation，但不会调用错误轮次的 Resume，也不会覆盖当前挂起和响应。token 感知的 claim 属于具体调度/存储策略，不是核心 Infrastructure 契约。

恢复时调用 Resume，不重新进入 Run；`ResumeInput` 包含 Token、恢复后的具体 Suspension 和 Response。Resume 可再次挂起，产生新 token。失败或进程中断保留挂起数据和响应以便重试。节点副作用仍需幂等，框架不能保证外部操作 exactly-once。

## Infrastructure 和恢复

Infrastructure 绑定一个业务执行 scope，不在每个方法中传递 RunID/TenantID。实现负责：

- 节点租约（`ClaimNode(ttl)`/`RenewNode`/`ReleaseNode`）、generation fencing、节点版本 CAS 和重复终态提交保护。
- 原子保存节点结果、依赖到达、就绪标记和 Request 通知，并返回本次新就绪的下游节点 ID。
- 原子保存首次响应和独立 Wake 通知。
- 存储编码、具体类型恢复和快照隔离；不支持的值应返回错误。

通知的轮询、确认和投递方式属于具体实现。memory 与 SQLite 分别在自己的包中声明 `Outbox` 能力接口，并由各自的 `Infrastructure` 实现；自定义 Infrastructure 可使用消息队列、变更流或自己的 outbox API，不需要实现框架层的统一 Outbox 接口。Request 按业务 run scope + NodeID + Token 去重；外部服务接受 Request 后 Ack，token 对应的 Resume 被安全处理后 Ack Wake。

`Start` 幂等地初始化或校验已有执行并返回唯一 start 节点 ID，多实例并发调用安全；`Drive` 执行一个节点、持久化结果并返回新就绪的下游；`Recover` 不重新初始化地列出当前可驱动且未被有效租约占用的节点（就绪、租约过期的 Running、以及可被新一轮重试的 Failed）用于进程恢复；`Wakeup` 保存回调。Engine 只按需读取节点和原输入路径，不加载整份状态或定义。`Executed.Ready` 和 `Recover` 只报告可驱动的节点 ID；驱动与排程由业务完成。失败即终态，当前驱动据此停止；是否重试由业务发起新一轮 `Start`/`Drive`/`Recover` 决定。

没有工作流整体 Status。业务通过 `GetNode` 或可选 `SnapshotReader.Load` 判断完成条件。`Options` 控制节点超时（默认 5 分钟）、节点租约选项和可观测性 `Hooks`。

### 节点并发与高可用

每次 `Drive(nodeID)` 只持有该节点的租约，不存在 run 级全局锁：

1. **前置判断 + 抢占**：`ClaimNode(nodeID, ttl)` 在一个原子操作中校验节点可驱动（Ready、租约过期的 Running 或 Failed）且租约空闲/过期，然后递增 generation、置 Running 并占有租约。不满足条件返回 nil，`Drive` 返回零值 `Executed` 和 nil error，因此重复投递同一节点 ID 是安全的，只有一个实例会执行。
2. **执行期续租**：节点 `Run/Resume` 期间每 TTL/3 调用一次 `RenewNode`；单次续租请求最多等待一个 TTL。续租失败（被其他实例 fencing 或存储不可用）会取消节点 ctx，`Drive` 返回 `InfrastructureError`，节点保持 Running，等待重试或接管，不会被记为 Failed。
3. **提交即结束租约**：`Commit` 在同一事务中校验节点 generation 与存储侧当前到期时间，保存结果、写入下游 arrival 并结束租约。过期 owner 的晚到提交返回 `lease.ErrConflict`。多个前驱从不同实例并发提交时，下游只会就绪一次，并且只出现在完成最后一条依赖的那次提交的 `Executed.Ready` 中。
4. **崩溃恢复**：实例宕机后，租约在 TTL 内无人续期而过期，节点重新出现在 `Recover` 结果中，可由任意实例接管；主动失败的 `Drive` 会立即 `ReleaseNode`，无需等待 TTL。

每次 Drive 自己的本地互斥锁协调 Renew 与 Commit：在途续租结束后才能提交；真实续租错误会阻止提交，即使取消信号尚未传回执行 goroutine。提交开始后不再续租，避免提交清除租约后出现虚假的 renewal conflict。该锁不在节点或流程之间共享；提交时存储仍校验有效租约，慢提交过期会正常报冲突。

租约只保证同一时刻最多一个有效 owner，并拒绝过期写入；它无法撤销副作用已发生、但结果尚未提交时丢失租约的那次执行，因此节点副作用仍需幂等。

`Hooks` 观测节点生命周期用于 metrics、trace 和日志：`OnClaim`（节点被 claim 并置 Running 后）、`OnStart`（调用 Run/Resume 前）、`OnFinish`（完成 panic 恢复、超时与返回值校验后、Commit 前，带 `Outcome`、`Duration` 和失败时的 `Err`）。每次实际调用 Run/Resume 对应一次 OnFinish，Duration 只计节点调用；跳过节点、输入解析失败不会调用节点，也不产生 OnStart/OnFinish。OnFinish 不表示结果已成功持久化。所有字段可选，nil 跳过。Hook 是旁路，不影响调度、提交或重试；hook 内部 panic 会被框架 recover 并忽略，不污染执行。`NodeEvent.Attempt` 携带节点租约的 generation，同一节点 Attempt 递增即表示发生了重试或接管。

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

## 业务组合

子流程、循环、审批和长任务都由业务基于同一挂起协议实现，框架不提供专用 childflow 类型。

父节点在 Run 中返回业务自定义 Suspension；业务消费持久化 Request，根据 run scope + NodeID + Token 幂等创建或推进外部执行。外部执行完成后调用 Wakeup 保存结果，再用相同 token 调用 Resume。父节点在 Resume 中解释响应并返回普通 Result，也可以再次挂起以实现循环。

子执行 ID、发布版本、完成条件、失败映射、取消传播、循环上限和结果聚合属于业务语义。大输入与累计结果可保存对象存储引用，避免把完整历史反复写入 Suspension。

运行示例见 [examples](examples/README.md)。

```sh
go test ./dflow/...
go test -race ./dflow/...
go vet ./dflow/...
```
