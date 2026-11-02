# dflow

dflow 是节点级持久化的 DAG 执行框架。每次 `Drive` 只执行一个节点，原子保存输入、输出、错误及下游到达信息，返回本次新就绪的节点 ID。业务负责执行初始化、取值、调度和异步任务进度；框架没有工作流整体状态。

完整最小示例见 [examples/minimal](examples/minimal/main.go)，包含业务发布对象、从边表生成拓扑、初始化根节点、内存存储事务、读取依赖和调度后继。从模块根目录运行：

```sh
go run ./dflow/examples/minimal
```

## 构建与运行

```go
type Definition[V any, R any] interface {
    GetNodes() []NodeSpec[V, R]
    GetEdges() []Edge
}

type Edge interface {
    GetSourceNodeID() string
    GetTargetNodeID() string
}

type NodeSpec[V any, R any] interface {
    GetID() string
    Validate(context.Context, V) error
    Build(context.Context, V) (PublishedNode[R], error)
}

type RuntimeNode[R any] interface {
    Run(context.Context, R) NodeResult
}
```

`V` 是业务校验上下文。`R` 是业务运行 API，节点直接通过它解析参数、执行 HTTP 操作或查询异步进度。每次 Drive 由调用方提供 R，不保存客户端和执行能力。

NodeSpec 描述完整配置，Build 返回业务实现的 PublishedNode，其中包含轻量运行节点、入出边和依赖列表，不执行业务操作。返回对象必须不可变且可并发复用。Loop 的子图可在构建时单独校验发布，运行节点只保存子图标识和必要参数。

```go
type ExecutionAPI interface {
    parameter.ValueResolver
    HTTPClient() *http.Client
}

type HTTPNode struct {
    URL parameter.Parameter[string]
}

func (n *HTTPNode) Run(ctx context.Context, runtime ExecutionAPI) dflow.NodeResult {
    address, err := n.URL.Get(ctx, runtime)
    if err != nil {
        return dflow.ResultWithError(err)
    }
    // 使用 runtime.HTTPClient() 执行业务请求。
    return dflow.Result{
        Input: map[string]any{"url": address},
        Output: map[string]any{"url": address},
    }
}
```

Compile 校验 ID、边唯一性、无环、唯一根节点和业务配置，以及业务发布对象与 `Definition.GetEdges()` 的一致性。`PublishedNode.GetDependencies()` 是唯一的依赖声明，包含该节点所有可能读取的节点 ID，不能有空值或重复项；编译校验这些节点都是当前节点的祖先，数据依赖不自动创建控制流边。业务 Build 从参数配置生成依赖列表，从边表统一生成入出边索引，无需在运行节点上重复声明。

`PublishedNode[R]` 是只读接口，提供 `GetID`、`GetNode`、`GetIncoming`、`GetOutgoing`、`GetDependencies`。业务用自己的数据库记录或 DTO 实现它，`DefinitionProvider.LoadNode` 直接返回接口。Compile 返回按拓扑排序的业务原始对象，不提供发布对象或 Provider 的内置实现，也不规定序列化格式。

```go
records, err := dflow.Compile(ctx, definition, validationAPI)
if err != nil {
    return err
}
// 业务持久化 records、构建绑定该发布的 provider、保存输入并初始化根节点：
// NodeState{Status: Pending, Ready: true, Enabled: true, Version: 1}
// 之后按节点准备运行 API：
engine, err := dflow.NewEngine(provider, infrastructure, dflow.Options{})
if err != nil {
    return err
}
executed, err := engine.Drive(ctx, records[0].GetID(), executionAPI)
// executed.Result 是业务 Run 返回的原始 NodeResult；executed.Ready 交给业务调度。
```

`DefinitionProvider[R]` 由业务实现并绑定已校验的不可变发布，LoadNode 只读取当前节点。下游前驱索引由存储适配器在 Commit 时读取。业务负责发布标识和执行绑定，节点配置、代码或拓扑变化时创建新发布，已有执行继续绑定原发布。

## 参数与数据加载

参数工具位于可选子包 `github.com/k0923/go/dflow/parameter`，核心引擎不依赖它。业务也可以自行实现参数解析和依赖声明。

```go
type Parameter[T any] interface {
    Get(context.Context, ValueResolver) (T, error)
}

constant := parameter.Const("https://example.com")
reference := parameter.Ref[string]("prepare", "request", "url")
rootInput := parameter.Ref[string]("root", "callbackURL")
address, err := reference.Get(ctx, executionAPI)
```

路径首段是节点 ID，其余支持字符串 key、数组索引和导出 Go 字段名。Root 输入应作为 Root 节点的输出暴露给 resolver。取值保留具体 Go 类型，不隐式转换。缺失返回 `parameter.ErrMissingValue`，类型不符返回 `parameter.ParameterTypeError`，支持 `errors.Is(err, parameter.ErrParameterType)`。nil 适用于可为空类型，不能解析为 string 等标量。

`Template` 插入 JSON 字面量；`PlainTemplate` 插入普通文本，二者返回 Parameter[string]。参数包不推导节点依赖；业务在 Build 时将依赖节点 ID 放入发布对象。

取值由业务 R 实现，引擎不读取依赖输出，也不包装 R。建议业务按当前节点的 `PublishedNode.GetDependencies()` 预加载输出，把存储错误留在 Run 之前。仅暴露声明的依赖和原始输入；跳过的源视为缺失。

`Values` 提供简单内存解析器，可嵌入业务运行对象：

```go
type Execution struct {
    parameter.Values
    Client *http.Client
}

func (r *Execution) HTTPClient() *http.Client { return r.Client }

runtime := &Execution{
    Values: parameter.Values{
        "root": map[string]any{"orderID": "order-1"},
        "prepare": map[string]any{
            "request": map[string]any{"url": "https://example.com"},
        },
    },
    Client: client,
}
```

Values 没有数据库访问、锁或缓存。创建后只读；缺失源或路径返回 `parameter.ErrMissingValue`。业务负责快照隔离以及 resolver 的并发安全。若 R 在运行期间读取存储，必须通过 `NodeResult.GetError` 返回错误；引擎无法观察业务主动忽略的错误。

## 节点结果与控制流

```go
type NodeResult interface {
    GetInput() map[string]any
    GetOutput() map[string]any
    GetError() error
}

type AsyncNodeResult interface {
    NodeResult
    IsDone() bool
}

type BranchNodeResult interface {
    NodeResult
    GetOutgoing() []string
}
```

默认实现为 Result、AsyncResult、BranchResult。输入和输出始终是 map[string]any，包含失败或未完成时的部分数据。nil Input/Output 表示保留已有持久化值，非 nil map 表示全量替换；空 map 可用于清空字段。

自定义结果可以增加业务方法；框架只保存标准字段，提交成功后 Executed.Result 保留本次结果对象，重启不会恢复该对象。结果对象必须只读，getter 必须可重复调用。Run 或结果 getter 的 panic 被转换为节点失败，getter panic 后不会返回原始结果对象。已成功捕获的标准字段仍可保存；框架的 map 拷贝是浅拷贝，嵌套对象的快照隔离由存储适配器负责。

- 普通节点成功后激活所有出边。
- 分支节点返回实际下游节点 ID；nil 或空数组表示全部不选，重复或未知 ID 是错误。未选择的边仍传播 inactive 到达。
- 异步节点只有无错误且 IsDone 为 true 才激活下游。done=false 保存 Input/Output，状态为 Suspended。
- 失败保存 Input/Output/Error 后返回 NodeError，不传播下游。

| Drive 结果 | 是否已提交 | 调用方处理 |
| --- | --- | --- |
| 零值 Executed、nil error | 否，本次未获取工作 | 不重复调度后继 |
| Status=Succeeded / Skipped | 是 | 调度 Ready 中的节点 |
| Status=Suspended | 是 | 由业务安排下一次 Drive，不自动轮询 |
| Status=Failed、NodeError | 是 | 根据错误决定停止或重试当前节点 |
| Status 为空、基础设施或租约错误 | 结果不确定 | 重试当前节点；恢复扫描修复可能丢失的后继投递 |

`Executed.Status` 非空表示 Commit 已成功，即使退出清理时又发生取消或租约错误。NodeID 可能在提交前已设置，不能仅凭 NodeID 判断已落库。以 Drive 返回的 error 判断本次执行错误；原始 Result.GetError 只表示业务结果中的错误，不能替代 panic、超时或非法结果等框架错误。

节点类型由 `Run` 返回的结果接口决定：优先识别 `BranchNodeResult`，其次识别 `AsyncNodeResult`，否则按普通 `NodeResult` 处理。一个结果不能同时实现分支和异步接口。

节点收到所有预期前驱到达后才就绪：至少一条 active 则执行，全部 inactive 则 Skipped 并继续传播。完成和跳过都计入已到达前驱数，因此互斥分支可以汇合到普通节点。Skipped 不调用 Run。

汇合的控制流就绪与取值独立。需要选择某个实际完成的分支数据时，应在业务节点逻辑里按自身规则读取对应分支输出，而不是在配置层声明兜底参数。

## 异步调度

异步节点始终调用同一个 Run，通过业务 R 启动任务或查询进度：

```go
return dflow.AsyncResult{
    Result: dflow.Result{
        Input: map[string]any{"taskID": taskID},
        Output: map[string]any{"progress": progress},
    },
    Done: finished,
}
```

done=false 不生成下游 Ready，也不自动重复入队。业务通过队列延迟、外部任务回调或其他策略再次调用 `Drive(ctx, nodeID, runtime)`。Suspended 可以显式重新 claim，仍受同一节点的租约保护。任务 ID、查询进度、外部幂等、过期回调判断及取消语义由业务 API 负责；框架不提供回调 token 和响应存储协议。

## 基建与恢复

核心 Infrastructure 只有 ClaimNode、RenewNode、ReleaseNode、Commit 四个方法。执行初始化、原始输入、依赖读取及回调不属于这个接口。

ClaimNode 原子检查就绪/可重试性并获得节点租约，递增 generation/version、置 Running，返回独立状态快照。Commit 在事务内校验 lease generation、存储侧当前有效期及 ExpectedVersion，保存结果、释放租约并合并下游到达。适配器在事务内使用 ValidateCommit、ApplyCommitResult、ApplyArrival。

通用续租、取消和退出清理直接复用 `github.com/k0923/go/lease.Runner`；节点适配层仅协调 ClaimNode 状态快照与 Commit 的结束租约行为。不同节点可并发推进，同一节点只有一个有效 owner。外部副作用仍需幂等。

恢复扫描使用 `driver.RecoveryReader.ReadyNodes`，由业务显式提供 reader。它列出新就绪节点、失去租约的 Running 和 Failed 重试，排除未就绪节点、有效租约、终态以及等待业务调度的 Suspended，避免形成异步忙轮询。核心 Engine 只提供单节点 Drive。

Ready 必须与结果一起持久化，使业务能修复提交后队列投递丢失。队列通知、定时检查和 outbox 方式由业务定义。

调用方取消、租约丢失、InfrastructureError 表示结果不确定，不记为 Failed。节点超时、panic、参数错误、业务错误和非法结果保存为 Failed。Hooks 提供 claim、Run 开始和结果规范化后的观测；OnFinish 在提交前，不表示已落库。

`driver.Run(ctx, engine, nodeID, runtime)` 串行推进指定节点及新就绪后继；`driver.Drain(ctx, engine, reader, runtime)` 执行一次恢复扫描。driver 不初始化执行，不自动轮询 Suspended。使用同一个 R 推进整图时，其 resolver 必须能读取每一步最新提交的输出；按节点预加载时由业务直接调用 Engine.Drive。
