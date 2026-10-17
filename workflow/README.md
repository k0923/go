# Workflow

一个以 Go 代码表达编排、以持久化快照回放执行结果的工作流内核。需要本仓库的 Go 1.27.1 工具链（使用泛型方法）。

## 核心 API

| 概念 | 职责 | API |
| --- | --- | --- |
| Workflow | 名称、版本及由引擎执行的顶层编排定义 | `NewWorkflow(name, version, handler)`；单 Flow 用 `NewWorkflowFromFlow(name, version, flow)` |
| Flow | 确定性编排、显式传递输入输出 | `Run(*FlowContext, I) (O, error)` |
| Action | 一次计算或外部操作，不持有编排能力 | `Run(context.Context, I) (O, error)` 接口；函数用 `ActionFn` / `AsAction` 适配 |
| Engine | 驱动、回放、租约、错误归类、观测 | `NewEngine`、`Drive`、`Signal`、`ResolveSignal`、`Subscribe` |
| WorkflowContext | 仅顶层跨流程调用能力，由 Engine 创建 | `RunFlow`、`GoFlow`、`Context` |
| FlowContext | Action、信号与检查点能力 | `Run`、`Go`、`WaitFor`、`WaitAny`、`Sleep`、`SleepUntil`、`Now`、`UUID`、`Loop` |
| Infrastructure | 一个实例的原子存储与租约 | `Get`、`Put`、`Commit`、`Resolve`、`Claim`、`Renew`、`Release`；可选 `WaitAnyInfrastructure`、`RecordLister`、`Clock` |

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
    if err != nil { // 只可能是已提交的业务失败 *ActionError，可以处理或返回
        return OrderOutput{}, err
    }
    approved := ctx.WaitFor[Approval]("approval", submitted) // 未回调时直接挂起，无需检查错误
    return buildOutput(submitted, approved), nil
}

engine, err := workflow.NewEngine(scopedInfrastructure, workflow.Options{
    LeaseTTL: 30 * time.Second,
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

## 定位与返回语义

Engine 是**执行内核**：一次 `Drive` 回放一次定义并返回本次结果。run 级状态（创建、终态、输出）、调度、重试上限与版本路由都由业务方实现。

**版本策略：执行与定义版本绑定。** `$definition` 记录启动 run 的名称、版本与输入，换版本后在旧 run 上 Drive 会返回 `Failed` + `ErrChanged`。热修复以新版本发布，只用于新 run；在途 run 继续路由到原版本代码直至结束。内核不提供版本迁移原语。

### 控制流：挂起用 panic，业务失败用 error

编排代码只会看到两类值：

| 情况 | 编排代码看到的 | 说明 |
| --- | --- | --- |
| 等待未就绪（WaitFor / Sleep / WaitAny / 等待中的 Future） | 不返回，当前 goroutine 被私有 panic 展开 | 无需 `if err != nil`；吞不掉，也无法用零值继续 |
| 框架故障（存储、租约、可重试 Action 失败、误用、漂移、WaitAny 全部候选失败） | 同上，Drive 中止 | 由 `Drive` 的 `Result.Status` 与 err 报告 |
| 已提交的业务失败 | `Run`/`Go`/`Loop` 返回 `*ActionError`；`Loop` 耗尽返回 `ErrLoopExhausted` | 可以处理（补偿、降级）或直接返回 |

挂起时 defer 仍会执行，因此编排代码中的 defer 不得有副作用。不要在编排中盲目 `recover()`：即使恢复了框架 panic，同一 goroutine 上的后续原语都会再次停止，不会持久化任何新进度；若吞掉后正常返回，Drive 报告 `Failed`。

### Drive 结果

| 返回 | 含义 | 调度处理 |
| --- | --- | --- |
| `Completed`，nil | 本次编排完成 | 使用 Output，业务方记录终态 |
| `Suspended`，nil | 存在挂起点（`Pending` 列出全部） | 等待持久化 REQUEST/WAKE |
| `Running`，非 nil | 可重试：IO、存储、租约冲突、可重试 Action 失败、取消 | 退避后重新 Drive；上限由业务方决定 |
| `Failed`，非 nil | 终态：handler **返回**的 error、定义/类型漂移、误用、编排代码 panic、WaitAny 全部候选失败（`ErrWaitFailed`）、无人等待的 Go/GoFlow 业务错误 | 终止或人工处理；修复走新版本 |

只有 handler 最终返回的 error 决定失败：被编排代码处理掉的 `*ActionError` 不会让 run 失败。框架故障优先于挂起：任一分支中止时，即使兄弟分支挂起也报告 `Running`/`Failed`。

已完成 Action、检查点和信号持久化回放。Record 是 CAS 快照，不是不可变事件日志。

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

Action 失败分两类：

- **可重试**：普通 error、Action panic（转为带堆栈的 `PanicError`）、单次 Timeout、包装了 Permanent 但被中断的尝试。默认（`Retry.MaxAttempts == 0`）中止本次 Drive，Record 保持 RUNNING，由调度方重新驱动。设置 `ActionOptions{Retry: RetryPolicy{MaxAttempts, InitialInterval, MaxInterval, Multiplier}}` 后在持有租约期间指数退避重试；用尽后把最后一次失败**提交为业务失败**，编排可以据此补偿。尝试次数按 Drive 计数。
- **业务失败**：Action 返回 `Permanent(err)` 或重试耗尽。提交 FAILED 后 `Run` 返回 `*ActionError`；执行时 `Cause` 保留原始错误，回放时只有 `Message`（Go 错误身份无法跨序列化保留，稳定错误码请放进业务负载）。

编排（Flow / Workflow handler）本身直接返回普通 error 即为终态，不需要 `Permanent`。

## 挂起与唤醒

`WaitFor` 和 `Sleep` 是 Flow 原语，不在 Action 租约内部：

1. `Put` 原子插入等待快照和投递登记，insert-if-absent 返回已有快照。
2. 普通信号产生 REQUEST 消息；休眠产生按 Until 可见的 WAKE 消息。
3. 外部请求完成后调用 `Signal(ctx, key, response)`（幂等，无需先读 version）或 `ResolveSignal(ctx, key, version, response)`（带过期请求校验），原子提交响应及独立 WAKE 消息。
4. 旧 REQUEST 的延迟确认不会消耗新 WAKE。重复回调保留第一个结果，陈旧 version 返回 `ErrConflict`，注册前到达返回 `ErrNotFound`，调用方负责重试或 inbox 缓存。
5. WAKE 消费方重新 Drive，成功处理或再次挂起后确认；遇到租约冲突或暂时错误必须重试消息，不得静默确认。

驱动的 Pending 是观察快照，可能落后于已经提交的回调。调度必须以持久化投递为准。这个契约避免“持旧快照挂起时，唯一唤醒已经被消耗”的窗口。

## 并发与 Loop

`Go` 异步执行 Action，`GoFlow` 异步运行一个 Flow，各自运行在独立的编排路径（strand）上：某个分支挂起只停止该分支，兄弟分支继续执行，因此同一次 Drive 能登记全部独立等待。Future 支持重复、并发 Wait；`Wait` 返回 nil 或业务错误，若任务处于等待中或 Drive 已中止，`Wait` 会让调用方同样挂起。同一 strand 上的多个等待是顺序的，第一个未就绪的等待之后的代码不会执行。

Drive 在返回前汇合所有通过框架注册的分支，即使调用方未 Wait；框架故障会取消兄弟 IO，所有分支挂起点都被收集。无人 Wait 的 Go/GoFlow 业务错误不会被静默丢弃，Drive 报告 `Failed`。挂起本身不取消兄弟分支，已启动的 Action 可继续完成；因此挂起返回可能等待长耗时 Action。为长 IO 设置 ActionOptions.Timeout。

RunFlow 和 GoFlow 的完成边界包含被调用 Flow 的异步 Action，即使它忘记 Wait。Flow 中的等待会直接停止调用路径，顶层不可能越过汇合点用零值执行后续 Action。Flow 不可再创建其他 Flow。

并行子 Flow 应先全部启动，再等待结果：

```go
left := ctx.GoFlow("left", leftFlow, input)
right := ctx.GoFlow("right", rightFlow, input)
leftResult, err := left.Wait() // 左侧仍在等待时，这里直接挂起
if err != nil {                 // 只可能是左侧 Flow 返回的业务错误
    return Output{}, err
}
rightResult, err := right.Wait()
if err != nil {
    return Output{}, err
}
// 此处两侧 Flow 及其 Action 均已完成，可以组合结果并调用后续 Flow。
// 两侧都在等待时，两者的挂起点都会出现在 Result.Pending 中。
```

顶层可并行启动多个 Flow；每个 Flow 可独立等待 WaitFor、Sleep 或 WaitAny，并使用 Go 并行执行 Action。FlowContext 不提供 RunFlow/GoFlow，非法嵌套框架调用在编译期拒绝。WaitFor/Sleep 挂起时不保留阻塞监听 goroutine，而是登记持久记录并退出本次执行；下一次 Drive 重放并复用完成结果。Future.Wait 等待的是本次 Flow 执行结束，不是等待真实回调到达。

多个独立 Workflow 实例必须各自绑定 Infrastructure 作用域（参考实现每个实例一个 `memory.New()`）；同一作用域上的同名定义和相同输入表示同一实例的冗余驱动，不代表两个独立实例。业务回调和 WAKE 消费也必须路由到相同作用域，冲突或暂时错误保留消息重试。作用域内子 Flow 的 `left|reply`、`right|reply` 等完整 key 自动隔离。

结构化并发只跟踪框架 Go/GoFlow 创建的分支。不要用原生 goroutine 异步调用或保存 FlowContext；跨 Future 建立循环等待会造成业务死锁。Action 与父 context 应设置合理的 timeout；大规模扇出由调用方/调度器控制并发，当前框架不提供自动并发上限。

不允许使用原生 goroutine 在注册分支之外持续访问 FlowContext。不响应 context 的 Action 无法被 Go 安全强制终止，会阻塞汇合。这是明确的使用约束。

`Loop` 对每个 attempt 独立持久化 Action 结果，间隔通过独立 Sleep 持久化。未到期重驱只回放旧结果，不推进 attempt。完成或次数耗尽后不会重复执行已提交的 attempt。`MaxAttempts > 0`、`Interval > 0`，生产中避免过短间隔。

`f.WaitAny(groupKey, cases...)` 类似 `select`：每个 Case 各自声明响应类型，同一组可以混合不同响应类型的信号与定时器，首个成功回调或到期的定时器原子确定赢家并取消其余成员，后续 Drive 立即继续。返回值是赢家 Case 本身，用 `==` 或 `switch` 判断来源，再从对应 Case 的 `Value()` 取类型化响应：

```go
approval := workflow.OnSignal[Approval]("approval", request)
code := workflow.OnSignal[int]("code", request)
timeout := workflow.OnSleep("timeout", time.Hour)
switch f.WaitAny("decision", approval, code, timeout) {
case approval: // approval.Value() 是 Approval
case code:     // code.Value() 是 int
case timeout:  // 定时器获胜，不携带业务响应
}
```

`OnSleepUntil` 使用绝对截止时间，`OnKind` 用于自定义存储扩展的信号种类。Case 只能使用一次，nil、重复传入或 Kind 与请求不匹配都以 `Failed` + `ErrInvalid` 拒绝且不登记任何等待。与 WaitFor/Sleep 一样，WaitAny 不返回 error：候选只是外部信号。`FailWait` 只剔除单个候选（失败原因通过 `Failure()` 读取），全部失败时 Drive 以 `Failed` + `ErrWaitFailed` 终止；需要降级时让提供方回答类型化的失败值，或加一个 Sleep 候选；外部消费者通过 CANCEL 通知清理监听资源。该功能需要可选 `WaitAnyInfrastructure`，`memory.Infrastructure` 已支持。完整场景、原子存储契约与运行命令见 [WAIT_ANY.md](WAIT_ANY.md)。

## Key、版本、隔离

- 局部 key 匹配 `^[A-Za-z0-9_]+$`，不能使用 `|` 或 `$`。
- 顶层挂载形成 `flow|action`；Loop/WaitAny 内部命名空间可形成 `flow|loop|attempt_0` 等 key，并不表示嵌套 Flow。`NewWorkflowFromFlow` 直接以 Flow 为根，不加挂载前缀，key 即 `action`。同一次驱动的 resolved key 不得重复。
- key 需要手写，不按调用顺序自动编号：Go/GoFlow 的分支调度顺序不确定，自动序号会让回放错位。写错 key（重复、非法字符）会在登记前以 `Failed` + `ErrInvalid` 快速失败。
- key 稳定且不能更名。回放身份是 **resolved key + Kind + 类型路径**（`Record.Type`），输入不参与身份比较：
  - 类型路径与 key 路径逐层对齐，用 `|` 连接。挂载的 Flow（RunFlow/GoFlow）贡献 `flow[I,O]`，Loop 贡献 `loop[I,O]`，WaitAny 组贡献 `any`；叶子为 `action[I,O]`、`signal[Request,Response]`、`sleep`、`now[time.Time]`、`uuid[string]`。例如 `pay|charge` 的类型路径为 `flow[int,int]|action[int,int]`。`NewWorkflowFromFlow` 的根 Flow 与不加 key 前缀一样，不贡献类型段。
  - 因此 Flow 挂载层和 Action 层的 key 都不变，但**任意一层**的 I/O 类型改变（例如挂载 Flow 的输入从 `int` 改为 `string`），都是确定性漂移：Drive 返回 `Failed` + `ErrChanged`。类型串取自 I/O 签名而非实现类型，把 `AsAction` 换成同 I/O 的具名 Action 不算漂移；但类型串是 `reflect` 的类型名（如 `model.Order`），因此给 I/O 类型改名或换包即使 JSON 不变也算漂移。
  - 同类型但输入 JSON 变化：沿用持久化结果（WaitFor 不重发请求，Sleep 不改截止时间），并发出 `input_changed` 观测事件。被中断（RUNNING）的 Action 重新执行时，记录的 Input 更新为本次实际输入。
  - 引入 `Record.Type` 之前写入的记录没有类型路径，回退为原来的输入严格比较，在途实例行为不变。
- 根名称、版本、输入绑定在 `$definition`，**仍严格比较输入**：它标识的是实例本身，用不同输入 Drive 同一实例是调用方路由错误而不是代码演进。在同一实例上更换它们会以 `Failed` + `ErrChanged` 拒绝执行；该机制不是自动历史代码迁移。部署保留旧版本路由，在途实例继续使用原版定义，修复只作用于新实例。
- Infrastructure 必须绑定一个实例隔离范围。内核接口不增加 RunID、TenantID；跨实例隔离由实现或装饰器承担。`memory.Infrastructure` 每个实例使用一个对象。
- Lease 提交原子检查当前存储的到期时间及 Generation，并校验 Record.Version。客户端 context 取消只负责尽快中止，存储 fencing 才是拒绝旧 owner 写入的最终保证。

## 扩展与观测

通用租约生命周期位于独立的 `github.com/k0923/go/lease` 包，workflow 复用 `lease.Runner`。`workflow.Lease` 是 `lease.Token` 的类型别名，Infrastructure 嵌入 `lease.Infrastructure`。其他框架可直接使用 Runner，lease 不依赖 workflow。租约错误返回 `*lease.Error`，支持 `errors.Is/As`；`workflow.ErrConflict` 复用 `lease.ErrConflict`。续租持续到结果提交完成，再停止续租并释放。

Options 支持 LeaseTTL、CleanupTimeout、时钟注入；单个 Action 的超时与重试由 ActionOptions 配置。默认租约 30 秒、清理超时 5 秒。续租按 TTL/3 调度；存储层决定实际到期。`Options.Now` 为空时，若存储实现可选 `Clock` 接口（`memory.Infrastructure` 已实现）则使用存储时钟，使 Sleep 截止时间与租约到期由同一时间源判断；否则使用 `time.Now`。显式注入时钟时必须与存储时钟一致。

观测统一使用 `Engine.Subscribe(listener, types...)`（旧 `Options.Observer` 已移除）：支持多个外部监听者、类型过滤和幂等退订；结构化 Event 包含执行关联 ID、状态、版本、耗时、挂起列表及错误信息。监听者可能被并发调用，应线程安全、快速返回；panic 被隔离。Action 重试时每次尝试都会发出 `action_started`。完整触发规则、事件结构、兼容性和交付边界见 [EVENTS.md](EVENTS.md)，可执行示例见 `example_test.go`。

Infrastructure 的 `Put`、`Commit`、`Resolve` 都有原子契约；数据库适配器应使用事务、原子 CAS 或等价机制。投递（谁、何时再次 Drive）不属于内核契约：内核只要求 Put/Resolve 原子地登记“需要投递/唤醒”的意图，如何暴露（outbox、MQ……）由适配器决定，载体需携带或绑定实例路由。

可选接口（运行时类型断言，未实现不影响基本功能）：

- `RecordLister`：`Records(ctx)` 一次性返回实例的全部记录（深拷贝）。Drive 开始时预加载其中的终态结果（已结束 Action、已解析等待、检查点），回放时不再逐步 Get/Put；类型漂移校验照常执行。
- `Clock`：见上文时钟说明。
- `WaitAnyInfrastructure`：见 [WAIT_ANY.md](WAIT_ANY.md)。

### 调试与测试用的子包

主包只包含引擎与存储契约。以下子包只服务于测试、示例和本地调试，不是生产实现：

| 子包 | 内容 |
| --- | --- |
| [`workflow/memory`](memory) | `memory.New()` 进程内参考 Infrastructure（实现全部可选接口）；参考投递契约 `memory.Outbox` 与 `memory.Notification`（REQUEST / WAKE / CANCEL）；测试辅助 `SetClock`、`Seed`、`Stats` |
| [`workflow/tests`](tests) | 主包的黑盒行为测试（只用公开 API + `memory`）。主包目录只保留可运行示例 `example_test.go` 和私有退避公式的白盒测试 `retry_internal_test.go` |
| [`workflow/debugger`](debugger) | HTTP + SQLite 调试服务与 Web UI，`debugger.Run` 一行接入；投递复用 `memory.Outbox` 语义 |

`memory.Outbox` 的投递契约：至少一次。REQUEST 投递给提供方后 Ack，提供方用 `Signal` / `ResolveSignal` / `FailWait` 回答；WAKE 触发 Drive，结果不是 `Running` 才 Ack，可重试错误保留消息退避重试；CANCEL 通知提供方停止失败的等待后 Ack。重复 WAKE 只会重放，迟到回答返回 false。可运行示例见 `memory.ExampleOutbox`。生产实现可以用自己的 MQ/outbox，遵守同样的至少一次语义即可。

没有生产数据库/MQ 适配器、自动后台调度、取消实例、补偿事务或热迁移功能。

## 从旧版迁移

旧版源码及测试完整保留在 `workflow/legacy`，仅用于兼容和历史对照；旧版已知问题没有被新版 API 修复。

| 旧版 | 新版 |
| --- | --- |
| `WithInfrastructure`，未注入时内存降级 | 显式 `NewEngine(infrastructure, options)` |
| 根 `RunFlow(ctx, handler, input)`，input 被忽略 | `engine.Drive(ctx, NewWorkflow(name, version, handler), input)` |
| `Action.Run(*ActionContext, input) output` | `Action.Run(context.Context, input) (output, error)`，函数用 AsAction |
| Action 内 `WaitFor` / `Sleep` | Flow 中独立调用，先完成 `Run` 再等待 |
| `panic(error)` | Action 返回 error，永久失败显式 `Permanent`；编排直接返回 error 即终态 |
| panic Signal / Conflict | 挂起仍由框架私有 panic 表达，但只在内核内部；调用方看 `Result.Status` / `Pending` / `errors.Is(err, ErrConflict)` |
| 直接 UpdateEvent 完成信号 | `ResolveSignal`，原子响应与唤醒登记 |
| Loop 单个快照滚动 Attempt | 每个 attempt 独立 Action，独立间隔 Sleep |

旧、新 Record schema 和 key 命名不同，不支持直接读取旧事件继续执行。生产迁移需使用新实例存储范围，或编写并验证显式转换工具；不能直接切换旧在途实例的包引用。

从本次重构中先前的函数式 Action 迁移：`ctx.Run(key, fn, input)` 改为 `ctx.Run(key, AsAction(fn), input)`，Go/Loop 同样适配；具名 Action 可直接传实例。这是源码接口变更，调用方需要更新。key、输入输出和 Record 格式保持不变，不能因为适配而更名执行点。nil 接口、nil ActionFn、typed-nil 实例和 Pure(nil) 均在执行前拒绝，不申请租约或写步骤记录。

从先前共用 FlowContext 的顶层定义迁移：删除 `Workflow{Flow: root}`，改用 `NewWorkflow(name, version, rootHandler)`；rootHandler 接收 WorkflowContext，普通 Flow 继续接收 FlowContext。根原有的 Action/信号操作放入独立 Flow，由根调用。RunFlow/GoFlow 已从 FlowContext 移除，topLevel 和 ErrFlowCallForbidden 不再使用。若新增挂载前缀或拆平流程导致 key 变化，应提升定义版本并使用新实例范围，在途实例继续路由到旧代码。

从上一版 error 挂起模型迁移（源码不兼容，Record 格式不变）：

| 上一版 | 当前 |
| --- | --- |
| `v, err := f.WaitFor[T](k, req)` | `v := f.WaitFor[T](k, req)` |
| `if err := f.Sleep(k, d); err != nil` / `SleepUntil` | `f.Sleep(k, d)` / `f.SleepUntil(k, t)` |
| `id, err := f.UUID(k)` / `f.Now(k)` | `id := f.UUID(k)` / `t := f.Now(k)` |
| `Run`/`Go`/`Loop` 返回任意 error | 只返回 nil 或 `*ActionError`（Loop 另有 `ErrLoopExhausted`） |
| `*ReplayError` | `*ActionError`（回放时 `Cause == nil`） |
| `*SuspendedError` | 移除；挂起不再作为 error 暴露 |
| WaitAny 全部失败返回 `*PermanentError` | WaitAny 只返回赢家；全部失败时 Drive 以 `Failed` + `ErrWaitFailed` 终止 |
| `f.WaitAny[R](g, WaitForSignal(k, req), SleepSignal(t, d))`，所有成员共用响应类型 R | `f.WaitAny(g, OnSignal[R](k, req), OnSleep(t, d))`，返回赢家 Case，各 Case 独立类型（详见 [WAIT_ANY.md](WAIT_ANY.md)） |
| `workflow.NewMemoryInfrastructure()`、`workflow.NotificationInfrastructure`、`workflow.Notification` / `*Notification` 常量 | `memory.New()`、`memory.Outbox`、`memory.Notification` / `memory.*Notification`（`workflow/memory` 子包） |
| `Future.WaitContext` | 移除；Drive 取消会直接中止等待中的编排 |
| `Options.Observer` / `Observation` | 移除；使用 `Engine.Subscribe` |
| handler 普通 error → `Running` 无限重试 | handler 返回的 error 一律 `Failed` |
| `ErrChanged`、误用、编排 panic → `Running` | `Failed` |
| 原语出错即取消整个执行，补偿不可达 | 业务失败交给编排处理，补偿可达 |

验证命令：

```sh
go test -race ./workflow/...
go vet ./workflow/...
```
