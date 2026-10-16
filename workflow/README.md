# Workflow

一个以 Go 代码表达编排、以持久化快照回放执行结果的工作流内核。需要本仓库的 Go 1.27.1 工具链（使用泛型方法）。

## 核心 API

| 概念 | 职责 | API |
| --- | --- | --- |
| Workflow | 名称、版本及由引擎执行的顶层编排定义 | `NewWorkflow(name, version, handler)` |
| Flow | 确定性编排、显式传递输入输出 | `Run(*FlowContext, I) (O, error)` |
| Action | 一次计算或外部操作，不持有编排能力 | `Run(context.Context, I) (O, error)` 接口；函数用 `ActionFn` / `AsAction` 适配 |
| Engine | 驱动、回放、租约、错误归类、观测 | `NewEngine`、`Drive`、`ResolveSignal` |
| WorkflowContext | 仅顶层跨流程调用能力，由 Engine 创建 | `RunFlow`、`GoFlow`、`Context` |
| FlowContext | Action、信号与检查点能力 | `Run`、`Go`、`WaitFor`、`WaitAny`、`Sleep`、`SleepUntil`、`Now`、`UUID`、`Loop` |
| Infrastructure | 一个实例的原子存储与租约 | `Get`、`Put`、`Commit`、`Resolve`、`Claim`、`Renew`、`Release` |

`FlowContext` 不实现 `context.Context`，也不嵌入 ActionContext。客户端和 Repository 由 Action 持有，只在 Action.Run 内使用标准 context。FlowContext 不提供业务客户端、Repository 或依赖容器泛型。Action 内没有等待、休眠、子流程或调度权限。

根输入传入 NewWorkflow 定义的处理函数，它接收独立 WorkflowContext，只能通过 `RunFlow` / `GoFlow` 调用 Flow。FlowContext 不含这两个方法，无法编译嵌套框架调用；顶层也不提供直接 Action 或等待原语。Workflow 不实现 Flow，没有公开 Run，执行入口由 Engine.Drive 管理。每次挂载创建一个 key 命名空间；Flow 本身不申请租约。完整权限矩阵和代码边界见 [CALL_HIERARCHY.md](CALL_HIERARCHY.md)。

## 使用

```go
type SubmitOrderAction struct {
    Client OrderClient
}

func (a SubmitOrderAction) Run(ctx context.Context, input SubmitRequest) (SubmitResponse, error) {
    return a.Client.Submit(ctx, input)
}

type OrderFlow struct {
    Submit workflow.Action[SubmitRequest, SubmitResponse]
}

// Flow 持有可执行步骤；外部依赖注入 Action。
func (f OrderFlow) Run(ctx *workflow.FlowContext, input OrderInput) (OrderOutput, error) {
    prepared := prepareRequest(input) // 轻量纯计算直接执行
    submitted, err := ctx.Run("submit", f.Submit, prepared)
    if err != nil {
        return OrderOutput{}, err
    }
    approved, err := ctx.WaitFor[Approval]("approval", submitted)
    if err != nil {
        return OrderOutput{}, err
    }
    return buildOutput(submitted, approved), nil
}

engine, err := workflow.NewEngine(scopedInfrastructure, workflow.Options{
    LeaseTTL: 30 * time.Second,
    Observer: observer,
})
if err != nil {
    return err
}
orderFlow := OrderFlow{Submit: SubmitOrderAction{Client: client}}
definition := workflow.NewWorkflow("order", "1",
    func(ctx *workflow.WorkflowContext, input OrderInput) (OrderOutput, error) {
        return ctx.RunFlow("order", orderFlow, input)
    },
)
result, err := engine.Drive(ctx, definition, input)
```

完整可执行例子见 `example_test.go`：提交 → 挂起 → 完成回调 → 恢复。

## 返回语义

`Drive` 不使用 panic 表达控制流。

| 返回 | 含义 | 调度处理 |
| --- | --- | --- |
| `Completed`，nil error | 本次编排完成 | 使用 Output |
| `Suspended`，nil error | 存在一个或多个挂起点 | 等待持久化请求/唤醒消息 |
| `Running`，非 nil error | 当前执行未完成，可能是 IO、存储、取消、冲突或代码故障 | 根据 error 重试、退避或告警 |
| `Failed`，非 nil error | 显式 Permanent 错误或持久化失败回放 | 按业务策略终止或人工处理 |

所有调用都遵循 Go 的 error 返回惯例。编排函数应在错误或挂起时立即返回。引擎另外登记框架 API 返回的错误和挂起点，避免忽略错误后误报成功；这个兜底不阻止错误后的业务代码继续执行。

合并错误中的所有挂起点会保留在 Pending 中；同时存在真实错误时仍返回该错误，
只有纯挂起结果才是 `Suspended` / nil。Action 的 FAILED 提交失败、提交结果未知或
执行 context 取消时保持可重试；不能仅凭 `errors.As(err, *PermanentError)` 停止调度，
应以 `Result.Status` 判断是否终止。panic 的原因即使含 Permanent，也仍作为代码故障处理。

根 Workflow 的完成状态由本次驱动结果表达；内核不保存根输出或维护调度器的实例状态。外部系统负责状态和终止策略。已完成 Action、检查点和信号持久化回放。Record 是 CAS 快照，不是不可变事件日志。

## Action 接口与纯函数边界

Action 是标准 Run 接口；ActionFn 保留函数接入能力，但接口或函数签名都不能证明数学意义上的纯度。

```go
type Action[I, O any] interface {
    Run(context.Context, I) (O, error)
}

// 普通函数和方法值通过 AsAction 推断类型。
output, err := ctx.Run("step", workflow.AsAction(fn), input)
```

- 纯计算：相同输入产生相同输出，不修改输入、共享状态或外部资源。轻量计算直接调用；需要独立持久化结果的昂贵计算可用 `Pure(fn)` 适配。
- IO Action：可以调用 HTTP、数据库、消息系统；必须响应标准 context 取消，并使用外部幂等机制。不得依赖 context 中的时间、随机数或 attempt 身份改变业务结果。
- Action 不持有工作流状态，不捕获可变编排状态，不调用 Flow API。无状态客户端依赖可以通过具名方法接收器绑定。
- 输出、输入及信号负载必须可 JSON 编解码。指针与可变对象不能在并行分支之间不加保护地共享。

Action 成功输出在提交前先按 JSON 编码，再解码到一个全新的输出类型实例；
首次执行和回放都向 Flow 返回该表示。`any` 中的数值按标准 JSON 解码为 `float64`，
不会保留原始 `int` 类型，也不能保证大整数精度。需要精确数值时使用明确的整数 DTO、
字符串或 DTO 中声明为 `json.Number` 的字段。自定义 MarshalJSON/UnmarshalJSON
应保持确定性并能够往返；解码失败不会提交 COMPLETED。
- 流程时间和 UUID 在 Flow 中通过 `Now(key)` / `UUID(key)` 检查点获取，再作为 Action input 传入。

结果持久化之前发生故障，IO Action 可能重复执行。`ActionKey(ctx)` 在一个绑定实例内稳定，外部幂等键应组合为“实例标识 + ActionKey”，不要包含租约 Generation。引擎不承诺外部副作用 exactly-once。

普通 Action error 只结束本次驱动，Record 保持 RUNNING，下次驱动可重试。业务确定性失败用 `Permanent(err)`；成功提交 FAILED 后回放返回 `ReplayError`。序列化后的错误不能保留 Go sentinel 身份；需要稳定错误码时把它定义在业务负载中。panic 会转为带堆栈的 `PanicError`，作为可诊断故障返回，不直接持久化成业务失败。

## 挂起与唤醒

`WaitFor` 和 `Sleep` 是 Flow 原语，不在 Action 租约内部：

1. `Put` 原子插入等待快照和投递登记，insert-if-absent 返回已有快照。
2. 普通信号产生 REQUEST 消息；休眠产生按 Until 可见的 WAKE 消息。
3. 外部请求完成后调用 `ResolveSignal(ctx, key, version, response)`，原子提交响应及独立 WAKE 消息。
4. 旧 REQUEST 的延迟确认不会消耗新 WAKE。重复回调保留第一个结果，陈旧 version 返回 `ErrConflict`，注册前到达返回 `ErrNotFound`，调用方负责重试或 inbox 缓存。
5. WAKE 消费方重新 Drive，成功处理或再次挂起后确认；遇到租约冲突或暂时错误必须重试消息，不得静默确认。

驱动的 Pending 是观察快照，可能落后于已经提交的回调。调度必须以持久化投递为准。这个契约避免“持旧快照挂起时，唯一唤醒已经被消耗”的窗口。

## 并发与 Loop

`Go` 异步执行 Action，`GoFlow` 异步运行一个 Flow。Future 支持重复、并发 Wait，并支持 `WaitContext` 取消等待。取消等待不等于取消任务。

Drive 在返回前汇合所有通过框架注册的分支，即使调用方未 Wait；错误会取消兄弟 IO，所有分支挂起点都被收集。挂起本身不取消兄弟分支，已启动的 Action 可继续完成；因此挂起返回可能等待长耗时 Action。为长 IO 设置 ActionOptions.Timeout。

RunFlow 和 GoFlow 的完成边界包含被调用 Flow 的异步 Action，即使它忘记 Wait。Flow 中的等待返回挂起错误，顶层在检查错误后不能越过汇合点执行后续 Action。Flow 不可再创建其他 Flow。普通用户代码仍必须检查返回错误；框架无法阻止用户忽略错误后自行执行代码。

并行子 Flow 应先全部启动，再等待结果：

```go
left := ctx.GoFlow("left", leftFlow, input)
right := ctx.GoFlow("right", rightFlow, input)
leftResult, err := left.Wait()
if err != nil {
    return Output{}, err
}
rightResult, err := right.Wait()
if err != nil {
    return Output{}, err
}
// 此处两侧 Flow 及其 Action 均已完成，可以组合结果并调用后续 Flow。
```

顶层可并行启动多个 Flow；每个 Flow 可独立等待 WaitFor、Sleep 或 WaitAny，并使用 Go 并行执行 Action。FlowContext 不提供 RunFlow/GoFlow，非法嵌套框架调用在编译期拒绝。WaitFor/Sleep 挂起时不保留阻塞监听 goroutine，而是登记持久记录并退出本次执行；下一次 Drive 重放并复用完成结果。Future.Wait 等待的是本次 Flow 执行结束，不是等待真实回调到达。

多个独立 Workflow 实例必须各自绑定 Infrastructure 作用域（参考实现每个实例一个 MemoryInfrastructure）；同一作用域上的同名定义和相同输入表示同一实例的冗余驱动，不代表两个独立实例。业务回调和 WAKE 消费也必须路由到相同作用域，冲突或暂时错误保留消息重试。作用域内子 Flow 的 `left|reply`、`right|reply` 等完整 key 自动隔离。

结构化并发只跟踪框架 Go/GoFlow 创建的分支。不要用原生 goroutine 异步调用或保存 FlowContext；跨 Future 建立循环等待会造成业务死锁。Action 与父 context 应设置合理的 timeout；大规模扇出由调用方/调度器控制并发，当前框架不提供自动并发上限。

不允许使用原生 goroutine 在注册分支之外持续访问 FlowContext。不响应 context 的 Action 无法被 Go 安全强制终止，会阻塞汇合。这是明确的使用约束。

`Loop` 对每个 attempt 独立持久化 Action 结果，间隔通过独立 Sleep 持久化。未到期重驱只回放旧结果，不推进 attempt。完成或次数耗尽后不会重复执行已提交的 attempt。`MaxAttempts > 0`、`Interval > 0`，生产中避免过短间隔。

`WaitAny[Response](groupKey, cases...)` 支持多个 WaitFor 同时等待，首个成功回调原子确定赢家并取消其余成员，后续 Drive 立即继续。`FailWait` 可隔离单个候选失败；外部消费者通过新增 CANCEL 通知清理监听资源。该功能需要可选 `WaitAnyInfrastructure`，MemoryInfrastructure 已支持。完整双等待场景、原子存储契约与运行命令见 [WAIT_ANY.md](WAIT_ANY.md)。

WaitAny 的组合单位是 Signal 描述，也支持 `WaitForSignal(...)` 与 `SleepSignal(...)` / `SleepUntilSignal(...)` 混合等待，实现“外部响应或超时先到”。赢家 Kind 区分触发来源；Sleep 获胜不要求业务响应。`ctx.Sleep` 等单独等待 API 保留，旧 WaitCase 用法继续兼容。

## Key、版本、隔离

- 局部 key 匹配 `^[A-Za-z0-9_]+$`，不能使用 `|` 或 `$`。
- 顶层挂载形成 `flow|action`；Loop/WaitAny 内部命名空间可形成 `flow|loop|attempt_0` 等 key，并不表示嵌套 Flow。同一次驱动的 resolved key 不得重复。
- key 稳定且不能更名。Action input 的 JSON 内容变化会返回 `ErrChanged`。
- 根名称、版本、输入绑定在 `$definition`。在同一实例上更换它们会拒绝执行；该机制不是自动历史代码迁移。部署保留旧版本路由，在途实例继续使用原版定义。
- Infrastructure 必须绑定一个实例隔离范围。内核接口不增加 RunID、TenantID；跨实例隔离由实现或装饰器承担。MemoryInfrastructure 每个实例使用一个对象。
- Lease 提交原子检查当前存储的到期时间及 Generation，并校验 Record.Version。客户端 context 取消只负责尽快中止，存储 fencing 才是拒绝旧 owner 写入的最终保证。

## 扩展与观测

通用租约生命周期位于独立的 `github.com/k0923/go/lease` 包，workflow 复用 `lease.Runner`。`workflow.Lease` 是 `lease.Token` 的类型别名，Infrastructure 嵌入 `lease.Infrastructure`。其他框架可直接使用 Runner，lease 不依赖 workflow。租约错误返回 `*lease.Error`，支持 `errors.Is/As`；`workflow.ErrConflict` 复用 `lease.ErrConflict`。续租持续到结果提交完成，再停止续租并释放。

Options 支持 LeaseTTL、Action timeout、CleanupTimeout、Observer、时钟注入。默认租约 30 秒、清理超时 5 秒。续租按 TTL/3 调度；存储层决定实际到期。测试时钟必须同时注入 Engine 与测试存储，生产应保证时钟一致。

Observer 可记录 drive_started、drive_finished、action_started、action_finished、action_replayed、suspended。它可能被并发调用，应线程安全、快速返回；观察器 panic 被隔离，防止已提交操作因为监控故障被重试。需要持久化审计轨迹时由 Observer 或存储实现单独维护。

新增的 `Engine.Subscribe(listener, types...)` 支持多个外部监听者、类型过滤和幂等退订；结构化 Event 包含执行关联 ID、状态、版本、耗时、挂起列表及错误信息。旧 Observer 的签名与阶段保持不变。完整触发规则、事件结构、兼容性和交付边界见 [EVENTS.md](EVENTS.md)，可执行示例见 `events_example_test.go`。

Infrastructure 的 `Put`、`Commit`、`Resolve` 都有原子契约；数据库适配器应使用事务、原子 CAS 或等价机制。`Delivery` 是可选接口，生产实现可以暴露自己的 MQ/outbox。通知载体需携带或绑定实例路由，内核 Notification 只保存实例内的 key。

当前提供 MemoryInfrastructure 和契约测试，没有生产数据库/MQ适配器、自动后台调度、取消实例、补偿事务或热迁移功能。

## 从旧版迁移

旧版源码及测试完整保留在 `workflow/legacy`，仅用于兼容和历史对照；旧版已知问题没有被新版 API 修复。

| 旧版 | 新版 |
| --- | --- |
| `WithInfrastructure`，未注入时内存降级 | 显式 `NewEngine(infrastructure, options)` |
| 根 `RunFlow(ctx, handler, input)`，input 被忽略 | `engine.Drive(ctx, NewWorkflow(name, version, handler), input)` |
| `Action.Run(*ActionContext, input) output` | `Action.Run(context.Context, input) (output, error)`，函数用 AsAction |
| Action 内 `WaitFor` / `Sleep` | Flow 中独立调用，先完成 `Run` 再等待 |
| `panic(error)` | 返回 error；需要永久失败显式 `Permanent` |
| panic Signal / Conflict | `Result.Status` / `Pending` / `errors.Is(err, ErrConflict)` |
| 直接 UpdateEvent 完成信号 | `ResolveSignal`，原子响应与唤醒登记 |
| Loop 单个快照滚动 Attempt | 每个 attempt 独立 Action，独立间隔 Sleep |

旧、新 Record schema 和 key 命名不同，不支持直接读取旧事件继续执行。生产迁移需使用新实例存储范围，或编写并验证显式转换工具；不能直接切换旧在途实例的包引用。

从本次重构中先前的函数式 Action 迁移：`ctx.Run(key, fn, input)` 改为 `ctx.Run(key, AsAction(fn), input)`，Go/Loop 同样适配；具名 Action 可直接传实例。这是源码接口变更，调用方需要更新。key、输入输出和 Record 格式保持不变，不能因为适配而更名执行点。nil 接口、nil ActionFn、typed-nil 实例和 Pure(nil) 均在执行前拒绝，不申请租约或写步骤记录。

从先前共用 FlowContext 的顶层定义迁移：删除 `Workflow{Flow: root}`，改用 `NewWorkflow(name, version, rootHandler)`；rootHandler 接收 WorkflowContext，普通 Flow 继续接收 FlowContext。根原有的 Action/信号操作放入独立 Flow，由根调用。RunFlow/GoFlow 已从 FlowContext 移除，topLevel 和 ErrFlowCallForbidden 不再使用。若新增挂载前缀或拆平流程导致 key 变化，应提升定义版本并使用新实例范围，在途实例继续路由到旧代码。

验证命令：

```sh
go test -race ./workflow/...
go vet ./workflow/...
```
