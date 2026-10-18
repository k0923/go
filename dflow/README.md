# dflow

`dflow` 是声明式 DAG 工作流执行包。节点接收解析后的 input，返回 JSON output；引擎根据有向边调度节点，保存节点结果，并从未完成节点恢复执行。

支持拓扑校验、祖先节点输出引用、有界并发、分支跳过与汇合、节点状态持久化、挂起与唤醒、执行租约、版本 CAS、自定义节点，以及默认 JavaScript 节点。框架只管理节点状态，工作流的业务状态由外部判断。

## 快速使用

```go
definition := dflow.Definition{
    Version: "1",
    Nodes: []dflow.NodeSpec{
        {ID: "start", Type: "start"},
        {
            ID: "end", Type: "end",
            Inputs: dflow.Parameters{
                "name": dflow.Ref("start", "name"),
            },
        },
    },
    Edges: []dflow.Edge{
        {SourceNodeID: "start", TargetNodeID: "end"},
    },
}
graph, err := dflow.Compile(definition, dflow.DefaultRegistry())
if err != nil {
    return err
}
engine, err := dflow.NewEngine(graph, memory.New(), dflow.Options{
    MaxConcurrency: 4,
})
if err != nil {
    return err
}
state, err := engine.Run(ctx, map[string]any{"name": "DAG"})
if err != nil {
    return err
}
fmt.Println(state.Nodes["end"].Output)
```

导入 `github.com/k0923/go/dflow` 和 `github.com/k0923/go/dflow/memory`。完整示例见 [examples](examples/README.md)。

## 定义和输入引用

`Definition` 包含 `Version`、`Nodes`、`Edges`。每个 `NodeSpec` 统一定义 `ID`、`Type`、`Inputs` 和节点专用的 `Config`。定义支持标准 `encoding/json` 编解码；`Inputs` 的 JSON 字段名为 `inputsValues`。

`Compile` 在发布阶段拒绝空图、空/重复节点 ID、重复边、不存在的边端点、环、未知节点类型和非法参数引用。图支持多个根节点与末端节点，不要求必须存在 start/end。Graph 保存已校验定义，不持有运行时节点实例；注册表在执行时按需创建节点。

## 发布与节点级加载

`graph.Publication()` 导出可序列化的 Manifest 和 PublishedNode 记录。Manifest 只包含定义 hash 和根节点 ID；每条 PublishedNode 包含 NodeSpec、唯一前驱集合 Incoming 和出边 Outgoing。业务数据库可以分别保存这些记录。

运行时实现 `DefinitionProvider`：Manifest 读取小型版本头，LoadNode 只加载一个节点定义，Incoming 只读取目标的前驱索引。执行通过 `NewEngineWithProvider(provider, registry, infrastructure, options)` 创建，不需要反序列化整张图。Provider 必须绑定发布时已校验的不可变版本；入出边和节点记录必须来自同一次发布。

SQLite 提供原子发布和按节点加载：

```go
// 发布阶段：整图校验后按节点保存。
graph, err := dflow.Compile(definition, registry)
if err != nil {
    return err
}
if _, err := store.Publish(ctx, "snapshot-1", graph); err != nil {
    return err
}

// 执行或进程重启：只需要固定的发布 ID。
engine, err := dflow.NewEngineWithProvider(
    store.Provider("snapshot-1"), registry, store, dflow.Options{},
)
```

SQLite 发布 ID 在同一文件内唯一，业务可组合项目和版本作为 ID。相同内容可重复发布，修改内容必须使用新 ID。现有 NewEngine(graph, ...) 作为本地内存定义的便捷入口保留。

调度使用持久化就绪任务：根节点初始化为 ready；前驱成功或跳过后，Commit 在同一事务中保存结果并向所有直接后继记录 incoming[sourceID]=active。到达记录按来源去重，全部预期前驱到达时原子生成就绪状态，任一 active 就执行，否则跳过。同一个来源多个端口指向同一目标时合并为一个到达，任一端口激活即 active。

ClaimReady 原子领取节点，并以执行租约 generation 和节点 Version 防止重复领取、过期写入。结果与后继 readiness 原子提交，进程在提交后退出不会丢失推进任务。正常运行在内存中持续领取并发执行，恢复从持久化 ready/中断记录继续，无需扫描定义或已完成节点的实现。

| 参数 | 用途 |
| --- | --- |
| `Constant(value)` | JSON 常量，可为对象、数组或 null |
| `Input("user", "name")` | 工作流输入中的路径 |
| `Ref("nodeID", "items", "0", "name")` | 祖先节点 output 中的对象键或数组下标 |
| `Ref("nodeID")` | 整个祖先节点 output |
| `Template("{{nodeID.name}}")` | 用 JSON 字面量替换引用，字符串带引号 |
| `PlainTemplate("hello {{nodeID.name}}")` | 用纯文本替换引用，适合 URL、消息 |

引用使用 `{"type":"ref","content":["nodeID","path"]}`，常量使用 `{"type":"constant","content":42}`。`$input` 是工作流输入的保留源名称。模板使用点分路径；包含点号的对象键应使用 `Ref` 的路径分段。

节点只能引用沿有向边可达的祖先节点，允许跨多条边引用。输入引用不会自动创建边。模板引用接受相同校验。路径不存在、数组越界、引用节点被跳过都会返回 `ErrMissingValue`；显式 null 是合法值。

数据在执行与持久化边界按 JSON 规范化，数字通常为 `float64`，大整数应使用字符串。每个节点接收独立输入副本，修改 input 不会污染其他节点或已保存的输入。自定义节点可在 `Run` 中使用业务 schema 校验 input/output。

## 节点和分支

```go
type Node interface {
    Type() string
    Run(context.Context, map[string]any) (dflow.Result, error)
}
```

`Result.Output` 是节点输出；`Result.Ports` 选择被激活的出边 `SourcePortID`：

- `nil`：激活全部出边。
- 非 nil 空切片：不激活任何出边。
- 非空切片：仅激活列出的端口；不存在的端口会返回节点错误。

汇合节点等待全部前驱成功或跳过，然后在至少一条入边被激活时执行；全部入边失活时跳过，并继续传播跳过状态。末端节点不会提前终止其他分支。多个末端结果可从 `State.Nodes` 读取；框架不根据它们推断工作流整体状态。

`DefaultRegistry()` 提供：

| 类型 | 行为 |
| --- | --- |
| `start` | 未配置 Inputs 时透传工作流输入，否则透传解析后的输入 |
| `end` / `passthrough` | 将解析后的输入作为输出 |
| `json-parse` | 将输入字段 `json` 的字符串解析为 JSON 值 |
| `condition` | 根据布尔输入 `condition` 选择 `true`/`false` 端口，输出 `{matchedKey: ...}` |
| `code` | 执行 JavaScript，返回 JSON 值 |
| `http` | 发出 HTTP 请求，输出 body、headers、statusCode |
| `subflow` | 挂起并请求固定版本子流程，Resume 返回子流程出口结果 |
| `loop` | 复用子流程协议逐项循环或有界条件循环 |

`condition` 被选中的端口需要连接出边。复杂条件可由前置 JS 或业务节点计算。

例如 `Inputs: {"condition": Ref("check", "allowed")}`：引擎先读取 check.output.allowed，解析成布尔值再调用 condition.Run。true/false 端口的到达状态分别传播，汇合节点等待所有前驱到达。当前 condition 不提供内置的左右值比较表达式；两个引用的比较可由前置 code 节点计算，引用依旧放在 Inputs 中接受 DAG 校验。

注册方式与 `xfield` 一致，节点通过 `Type()` 声明类型，工厂只负责创建实例：

```go
registry := dflow.DefaultRegistry()
err := registry.Register(func() dflow.Node {
    return &MyNode{service: service}
})
```

`Register` 调用一次工厂获取类型名，每次解码再创建新实例。工厂应轻量、可并发调用，返回稳定非空 Type，不执行业务副作用。默认将 `NodeSpec.Config` JSON 解码进节点；服务依赖可通过工厂闭包注入，配置字段使用 JSON tag。

复杂配置可实现 `NodeDecoder.DecodeNodeJSON(json.RawMessage) error` 自行解码和编译，内置 JS 节点使用此接口。`Registry.DecodeNode(spec)` 提供独立解码入口，`Compile` 使用同一入口。

`NewRegistry()` 创建空注册表，零值也可用；`DefaultRegistry()` 创建独立的默认注册表。重复类型注册会报错；`RegisterBuiltins()` 原子加入所有默认节点，冲突时整批不添加。需要自定义默认类型时，使用空注册表并注册所需节点。

`FuncNode{Kind, Handler}` 将 `NodeFunc` 适配为带类型的节点。节点实现应可并发复用，遵守 context 取消，不在返回后继续修改输出。`NodeID(ctx)` 返回当前节点 ID，业务方可结合自己的 RunID 构造幂等键。服务注入与配置解码示例见 [custom_node](examples/custom_node/main.go)。

## HTTP

`http` 默认注册，节点配置与动态输入分开：

```json
{
  "id": "request",
  "type": "http",
  "config": {
    "method": "POST",
    "bodyType": "JSON",
    "timeoutMs": 30000,
    "retryTimes": 0
  },
  "inputsValues": {
    "url": {"type":"constant","content":"https://example.com/api"},
    "body": {"type":"ref","content":["prepare","payload"]},
    "headers": {"type":"constant","content":{"X-Custom":"value"}},
    "params": {"type":"constant","content":{"page":1}},
    "auth": {"type":"constant","content":{"type":"bearer","bearerToken":"example-token"}}
  }
}
```

上例的 prepare 节点必须是 request 的祖先。所有动态数据使用统一 Inputs 引用机制；凭证由业务输入或自定义节点注入，不应写入真实定义文件。节点输入会被框架持久化，业务存储需自行管理凭证访问权限。

输入字段为 `url`、`headers`、`params`、`body`、`auth`。url 必须为绝对 HTTP(S) 地址；headers/params 为对象，值支持字符串、数字、布尔和相应数组。params 覆盖 URL 中的同名参数，保留其他参数。auth 支持 `none`、`bearer`（bearerToken）、`basic`（basicUsername/basicPassword）；显式 auth 覆盖 Authorization 请求头。

method 默认 GET。bodyType 支持 none（默认）、JSON 和 raw-text。JSON 接受对象、数组、null，或模板生成的合法 JSON 字符串，字符串不会被二次编码；上游引用的 JSON 数据直接映射到 body，不需要单独 JSON_REF 模式。raw-text 接受字符串。自动设置 Content-Type，可通过 headers 覆盖。

输出固定为 `{body: string, headers: object, statusCode: number}`，多个同名响应头输出为数组。非 2xx 也返回该结构，由下游自行判断；需要解析 JSON 响应时接 json-parse 节点。

timeoutMs 默认 30000，最大 600000，覆盖本节点所有请求和重试等待；引擎超时或调用方取消仍优先生效。retryTimes 默认 0，最大 5；网络请求错误或非 2xx 会重试，等待依次为 3/5/7/9/11 秒，可被取消。重试要求幂等方法、Idempotency-Key 请求头，或显式配置 retryNonIdempotent=true。节点恢复也可能重发请求，业务副作用仍需幂等。

maxResponseBytes 默认 8 MiB，超过时返回节点错误，避免无界读取响应。`HTTPNode.Client` 可通过空注册表的工厂注入自定义 `*http.Client`（例如 transport、代理、重定向策略）；默认使用 http.DefaultClient。

本地示例见 [http](examples/http/main.go)，无需外部网络服务。

## JavaScript

`code` 配置示例：

```json
{
  "script": {
    "language": "javascript",
    "content": "async function main({ params }) { await sleep(5); return { total: params.price * 2 }; }"
  },
  "timeoutMs": 10000
}
```

支持 `main({ params })`，也支持 `run(params)`。编译阶段只检查语法，入口是否存在在运行时检查。每次执行创建独立 Goja VM，默认限制 10 秒和 1000 层调用栈；引擎超时、调用方取消和脚本超时中最早发生者生效。`sleep(ms)` 可被取消。

返回已完成的 Promise 可正常读取，拒绝的 Promise 返回错误，pending Promise 返回错误。输出按 `JSON.stringify` 语义转换；undefined 顶层输出与循环对象报错。未提供事件循环、网络、文件系统、模块加载和 TypeScript 转译。Goja 没有进程内硬内存限额；需要严格资源隔离时，应将 JS 执行封装为外部进程或服务节点。

## 节点级持久化与恢复

普通节点在当前进程中连续执行，框架自动保存节点 input、output、状态和挂起信息。业务节点无需调用 checkpoint API。恢复的最小单元是一个节点：已保存成功结果的节点跳过，执行中断且尚未保存结果的节点重试。

需要异步等待的节点实现 `ResumableNode`：

```go
type TranscodeNode struct{}

func (TranscodeNode) Type() string { return "transcode" }

func (TranscodeNode) Run(ctx context.Context, input map[string]any) (dflow.Result, error) {
    return dflow.Result{Suspend: &dflow.JSONSuspension{
        Data: map[string]any{"format": "mp4", "source": input["url"]},
    }}, nil
}

func (TranscodeNode) Resume(
    ctx context.Context, input map[string]any, resumed dflow.ResumeInput,
) (dflow.Result, error) {
    // input 是已保存的原始节点输入。
    // resumed.Suspension 是恢复后的具体挂起类型，Response 是外部回调结果。
    return dflow.Result{Output: resumed.Response}, nil
}
```

`Result` 只有两种有效结果：通过 `Output` / `Ports` 完成节点，或者通过 `Suspend` 挂起节点。两者不能混用。返回挂起结果的节点必须实现 `Resume`。

`Suspension` 是统一接口，提供 `Type() string` 和 `MarshalJSON() ([]byte,error)`。具体类型同时包含发起操作和恢复所需的数据，不再分成通用 State/Request。框架为每次挂起生成 token，将节点的 suspended 状态、`SuspensionEnvelope{type,data}` 和 Request 通知**原子保存**，再释放执行槽。通知中的 Request JSON 也是同一个信封，恢复时通过注册表还原具体 Suspension，再传给 ResumeInput.Suspension。

默认类型为 JSONSuspension（dflow.json）、ChildSuspension（dflow.child）、LoopSuspension（dflow.loop）。JSONSuspension 提供通用 JSON 数据载体；业务可以定义审批、上传等具体类型，并通过 `registry.RegisterSuspension(func() dflow.Suspension { return &ApprovalSuspension{} })` 注册。MarshalJSON 返回类型的数据部分，框架负责附加 type 信封。具体类型可使用 alias 避免 MarshalJSON 递归。Registry.DecodeSuspension 还原类型，SuspensionTypes 列出已注册类型。未知类型、typed nil 和非法 JSON 会拒绝执行，不会静默降级成 map。

外部处理器先解码通知信封，根据 Type 路由到对应处理器，并用 Registry.DecodeSuspension 恢复具体对象。回调调用 `engine.Wakeup(ctx,nodeID,token,response)`；首次结果持久化并生成 Wake，重复或过期回调返回 false，未知 token 返回 ErrWaitNotFound。业务仍负责请求消费和处理器路由，接口本身不会自动发起外部任务。

随后业务调度器调用 `Engine.ResumeNode(ctx, nodeID, token)`。框架使用保存的 input、挂起信息和回调结果调用**节点的 Resume**，不会重新调用该节点的 Run。`Resume` 可以返回成功，也可以返回下一次挂起；每次挂起使用新 token，旧回调无法误唤醒新轮次。`Resume` 执行失败或中断时保留原挂起信息和响应，恢复时仍重试 Resume。

请求通过 `Outbox.Pending` / `Ack` 暴露：

- **Request**：使用通知内的 `Request` 提交外部任务，传递 `NodeID` 与 `Token` 供回调使用，服务接受后 Ack。业务 RunID + NodeID + Token 可用作幂等键。
- **Wake**：调用 Engine.ResumeNode(ctx, notification.NodeID, notification.Token)，无错误返回后 Ack；遇到租约冲突或存储错误保留通知并重试。外部业务自行更新整体工作流状态。

Request 和 Wake 分别确认。响应独立于节点快照保存，后续快照写入不能将其覆盖。正在执行的驱动器在后续调度时检查响应；空闲期间由外部消费 Wake 通知恢复。无需逐节点投递 MQ。

跨进程示例见 [suspend](examples/suspend/main.go)。需要拆分同步计算的恢复边界时，拆成多个 DAG 节点即可。

## 子流程调用与 Loop

subflow 配置 `{"publication":"child-v1","outputNode":"out"}`，其解析后的 Inputs 整体作为子流程 input。Run 返回 ChildSuspension，统一信封 type 为 dflow.child，data 包含 publication、outputNode 和 input。它随挂起状态原子落库，不直接占用父租约运行子引擎。

业务通过 `ChildDispatcher.Handle(ctx, parentEngine, parentScope, requestNotification)` 消费 Request，并提供 ChildBinder。Binder 根据 ChildExecution.ScopeKey() 创建或重新打开子 Infrastructure，通过固定 publication 获取 DefinitionProvider 并创建子 Engine；必须与父 Infrastructure 隔离，同一调用的重复投递绑定到同一子 scope。ScopeKey 使用父业务 scope、父 NodeID 和挂起 token 派生；不要将尝试次数或随机 ID 用作重投递的子 scope。

子流程没有单独的 Infrastructure 类型，使用同一个接口的独立实例。实例在 ChildBinder 中创建，而非 SubflowNode 内部：

```go
dispatcher := dflow.ChildDispatcher{
    Bind: func(ctx context.Context, call dflow.ChildExecution) (
        *dflow.Engine, dflow.Infrastructure, error,
    ) {
        childStore, err := sqlite.Open(ctx, databasePath, call.ScopeKey())
        if err != nil {
            return nil, nil, err
        }
        // 调用方登记并负责关闭 childStore。
        child, err := dflow.NewEngineWithProvider(
            childStore.Provider(call.Request.Publication),
            registry, childStore, dflow.Options{},
        )
        return child, childStore, err
    },
}
```

父和子可以共用同一个数据库文件或连接池，但运行态和租约通过不同 scope 隔离。父节点的挂起信息留在父 Infrastructure，子节点的 input/output/incoming 与租约写入 childStore。Dispatcher 读取 childStore 的出口结果，再通过父 Engine.Wakeup 写回父存储。完整句柄管理见 subflow 示例。

Handle 驱动子流程，并检查 outputNode 作为显式返回契约：

- 出口 succeeded：持久化 `ChildResponse{output:...}` 并唤醒父节点。
- 出口尚未就绪或子流程 suspended：返回 false，保留父 Request，等待子进度后重试。
- 已持久化的业务节点失败或出口 skipped：向父节点返回错误响应，父 Resume 返回节点错误。
- 租约冲突、取消、超时、存储失败：返回 error，保留 Request 并重试，不误判成子业务完成。

Handle 返回 true 后可 Ack 原 Request；父 Wake 仍单独消费，调用父 Engine.ResumeNode 后 Ack。节点的 SubflowNode.Resume 将响应 output 作为自身 output。这里判断的是配置出口的调用返回契约，不维护整个工作流状态；出口应由业务设计为真正的返回屏障，不能用提前完成的普通节点代表仍有必要分支在执行的子流程。

异步子流程及嵌套子流程由外部 dispatcher 持续驱动其 scope 的 outbox：子 Request 可递归使用同一协议，子 Wake 调用子 Engine.ResumeNode，然后重试父 Request。Request 在父响应落盘前一直保留，所以进程重启不丢失父子等待关联。Binder 返回句柄的关闭、扫描哪些 scope、重试/退避和资源限额由业务调度器负责；本协议不内置后台轮询器。

LoopNode 返回 LoopSuspension，type 为 dflow.loop，包含轮次、累计值、结果和 ChildRequest。ChildSuspension 与 LoopSuspension 都实现 ChildInvocation.ChildCall()，ChildDispatcher 解码统一信封后通过这个接口发起子调用。每轮对应独立子 scope，外层图仍是 DAG。配置包含 publication、outputNode、mode 和 maxIterations（默认 1000）：

| mode | 父节点输入 | 子流程输入 | 子出口返回 | Loop 输出 |
| --- | --- | --- | --- | --- |
| foreach（默认） | items 数组 | index、item、value、params | 任意 JSON | 按顺序收集的结果数组 |
| until | value 初始值 | index、item=null、value、params | `{done:bool,value:any}` | done=true 时的 value |

params 是 Loop 节点的原始解析输入，可用于传递其他业务参数。foreach 空数组直接输出空数组；超过 maxIterations 拒绝执行。until 每轮至少执行一次，done=false 时保存下一轮 value 再挂起；达到上限仍未 done 时返回节点错误。

轮次、累计结果和 value 保存在 LoopSuspension；ResumeInput.Suspension 恢复为该具体类型。Resume 重试不会跳过或重复累计一轮。每轮新 token 防止旧回调推进新轮次。foreach 累计结果会逐轮增大，大规模数据建议子出口返回对象存储引用。业务应限制嵌套深度与循环次数。

SQLite 发布、子流程驱动与 foreach Loop 的可运行示例见 [subflow](examples/subflow/main.go)。

## 调度结果和业务状态

`State` 没有工作流整体 Status。Engine.Run/ResumeNode/Drain 返回的 Nodes **只包含本次驱动触及的节点**，历史成功或未就绪节点不加载。没有可执行任务时释放租约，err==nil 表示本次驱动正常停止。需要业务观察时调用 Infrastructure.GetNode，或可选 SnapshotReader.Load 获取全量运行记录；引擎不会调用全量观察接口。

调用方可自行选择业务完成条件，例如检查指定出口：

```go
_, err := engine.Drain(ctx)
if err != nil {
    return err
}
end, err := infrastructure.GetNode(ctx, "end")
if err != nil {
    return err
}
if end != nil && end.Status == dflow.Succeeded {
    // 由业务层处理输出、更新自己的工作流状态。
}
```

框架负责保存节点的 `pending/running/succeeded/skipped/failed/suspended` 状态。首次节点错误停止当前调度并取消已启动节点，退出前等待它们返回；是否重试和如何定义工作流失败由业务决定。再次驱动会重试 failed / 中断的 running 节点，复用 succeeded / skipped 节点，未收到响应的 suspended 节点保持不动。

## Infrastructure

`Infrastructure` 由业务方绑定到单次执行，负责租约、节点快照和回调结果。RunID、TenantID 由实现方管理。`memory.New()` 仅供进程内使用；`sqlite.Open(ctx, path, scope)` 支持进程退出后恢复，同一文件通过 scope 隔离不同执行，使用后关闭存储句柄。

Initialize 原子绑定定义 hash/input 并插入 ready 根节点，重复初始化不重置进度。GetInput(ctx, path...) 按需读取原始输入，空路径返回整体输入，缺失返回 ErrMissingValue，显式 null 是合法值；GetNode 单独读取运行态。不再提供 Execution 或整体 Resume 入口。

ClaimReady 在当前租约下原子领取 ready 节点；旧 generation 的 running/failed 节点可重试，同一 generation 不重复执行。Commit 原子校验租约与节点 Version/Generation，保存节点结果、后继 incoming/readiness 和挂起 Request 通知；任何错误回滚全部变化。NodeState.Version 是节点 CAS，State.Version 仅为存储观察元数据，不参与节点写入 CAS。

GetResponse 读取已登记 token 的响应，尚未到达返回 nil，JSON null 为非 nil Response。Resolve 不获取执行租约，原子保存首个响应、唤醒节点 readiness 和 Wake 通知；结果提交不得清除响应。所有返回值应深拷贝。

租约键为 `dflow/run`；一次执行只有一个驱动器，内部节点有界并发。SQLite 使用事务、WAL 和 synchronous FULL；多机部署应使用业务提供的共享存储。旧版节点内 checkpoint 的 SQLite 文件不兼容此状态模型，Open 会明确拒绝；使用新文件或显式迁移，避免自动误读旧状态。

相同 Infrastructure 只能使用同一份定义和输入，否则返回 ErrRunMismatch。修改节点实现或挂起信息结构时更新 Definition.Version，为新版本创建新执行。节点运行态独立保存，大数据建议保存对象存储引用。尚未收到任何前驱到达的非根节点可能没有运行记录，业务观察时将不存在记录视为尚未就绪。

节点默认并发数为 4，单节点每次 Run/Resume 默认超时为 5 分钟，通过 `Options` 调整。业务节点必须响应 context 取消。外部副作用成功但节点结果尚未落盘时仍可能重试，因此 Run/Resume 都需要业务幂等或事务；异步提交优先通过挂起的 Request 通知发出。

`NodeError` 标明节点和失败阶段，`InfrastructureError` 标明存储操作；支持 `errors.Is/As`。持久化失败不会被改写成节点业务失败，也不会允许下游读取尚未确认的结果。

业务方负责初次驱动、故障接管及 outbox 消费。持久化定时器和内联 loop blocks 格式尚未提供；Loop 通过固定版本子流程实现。JS 的 sleep 是当前 VM 内的短暂等待。定义使用上述 NodeSpec 格式，不直接解码 flowgram 编辑器的 data、schema、loop blocks 字段。

## 验证

```sh
go test ./dflow/...
go test -race ./dflow/...
go vet ./dflow/...
```

单点恢复使用 `ResumeNode(ctx,nodeID,token,response)`，会先持久化回调，再只领取该节点并推进它的后继；不执行无关 ready 分支。消费已持久化 Wake 时省略 response。传入单个 nil 表示 JSON null 回调。旧 token 或已完成节点为空操作。挂起节点恢复使用保存的 NodeState.Input，不预先读取原始输入；后继引用 $input 时才调用 GetInput。进程故障接管使用 Drain(ctx) 领取持久化 ready/中断任务，不重新初始化执行。
