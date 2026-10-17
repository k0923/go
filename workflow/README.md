# Workflow

用 Go 代码描述编排、通过持久化快照回放执行结果的工作流**执行内核**。需要 Go 1.27.1+（使用泛型方法）。

Engine 每次 `Drive` 从入口回放一遍定义，并返回本次结果。run 级状态（创建、终态、输出）、调度、重试上限和版本路由都由调用方负责，内核不介入。

## 包结构

| 包 | 用途 |
| --- | --- |
| `workflow` | 引擎、编排原语、错误与事件模型、存储契约 |
| `workflow/memory` | 进程内参考存储 `memory.New()` 和参考投递契约 `memory.Outbox`，用于测试、示例和本地调试 |
| `workflow/debugger` | HTTP + SQLite 调试服务与 Web UI，`debugger.Run` 一行接入 |
| `workflow/tests` | 主包的黑盒行为测试 |
| `workflow/examples` | 可运行示例：`action_context_design`、`wait_any`、`debugger` |
| `github.com/k0923/go/lease` | 通用租约 Runner，workflow 复用它；lease 不依赖 workflow |

`workflow/legacy` 是旧实现，只保留做历史对照，与新版的数据格式不互通。

## 快速开始

```go
type SubmitOrder struct{ Client OrderClient }

func (a SubmitOrder) Run(ctx context.Context, in SubmitRequest) (SubmitResponse, error) {
    return a.Client.Submit(ctx, in) // 外部依赖由 Action 持有
}

type OrderFlow struct {
    Submit workflow.Action[SubmitRequest, SubmitResponse]
}

func (f OrderFlow) Run(ctx *workflow.FlowContext, in OrderInput) (OrderOutput, error) {
    submitted, err := ctx.Run("submit", f.Submit, prepare(in))
    if err != nil { // 只可能是已提交的业务失败 *ActionError
        return OrderOutput{}, err
    }
    approval := ctx.WaitFor[Approval]("approval", submitted) // 未回调时直接挂起，不返回
    return build(submitted, approval), nil
}

engine, err := workflow.NewEngine(store, workflow.Options{})
definition := workflow.NewWorkflowFromFlow("order", "1", OrderFlow{Submit: SubmitOrder{client}})
result, err := engine.Drive(ctx, definition, input)
// result.Status: COMPLETED / SUSPENDED / RUNNING / FAILED
```

完整的可运行例子（提交 → 挂起 → 回调 → 恢复）见 [example_test.go](example_test.go)。

## 三层模型

框架只允许 **顶层编排 → Flow → Action** 这一种调用关系，由类型在编译期保证：

| 层 | 定义 | 上下文 | 可用原语 |
| --- | --- | --- | --- |
| 顶层编排 | `NewWorkflow(name, version, func(*WorkflowContext, I) (O, error))` | `WorkflowContext` | `RunFlow`、`GoFlow` |
| Flow | `Flow[I, O]` 接口或 `FlowFunc` | `FlowContext` | `Run`、`Go`、`Loop`、`WaitFor`、`WaitAny`、`Sleep`、`SleepUntil`、`Now`、`UUID` |
| Action | `Action[I, O]` 接口或 `ActionFn` / `AsAction` / `Pure` | 标准 `context.Context` | 无编排能力 |

- 两种上下文是相互独立的类型，没有嵌入关系，也不能互相转换。Flow 拿不到 `RunFlow`/`GoFlow`，因此 Flow 嵌套 Flow 会直接编译失败；顶层也不能直接调用 Action 或等待原语。
- 整个工作流只有一个 Flow 时，用 `NewWorkflowFromFlow(name, version, flow)`，Flow 直接作为根，key 不带前缀。
- `Workflow` 不实现 Flow，也没有公开的 Run 方法，只能通过 `Engine.Drive` 执行。
- Go 不是沙箱：直接调用另一个 Flow 的 `Run` 方法，或在闭包里捕获 Engine/上下文，这些绕过手段类型系统拦不住，要靠代码审查。

## 控制流与错误语义

挂起和框架故障用**私有 panic** 展开编排代码，业务失败用 **error** 返回：

| 情况 | 编排代码看到什么 |
| --- | --- |
| 等待未就绪（WaitFor / Sleep / WaitAny / 等待中的 Future） | 不返回，当前 goroutine 被展开 |
| 框架故障：存储、租约、可重试 Action 失败、误用、定义漂移、WaitAny 全部候选失败 | 同上，本次 Drive 中止 |
| 已提交的业务失败 | `Run`/`Go`/`Loop` 返回 `*ActionError`；`Loop` 次数耗尽返回包装过的 `ErrLoopExhausted` |

- 等待原语（`WaitFor`、`Sleep`、`WaitAny`、`Now`、`UUID`）只返回值，不返回 error。
- 展开时 defer 照常执行，所以编排代码里的 defer 不能有副作用。
- 即使编排代码 `recover()` 了框架 panic，同一条执行路径（strand）上后续的原语也会再次停止，不会持久化新进度；如果吞掉后正常返回，Drive 报告 `Failed`。

`Drive` 的返回：

| Status | err | 含义 | 调用方的处理 |
| --- | --- | --- | --- |
| `COMPLETED` | nil | 编排完成，`Output` 有效 | 记录终态 |
| `SUSPENDED` | nil | 存在挂起点，`Pending` 列出全部 | 等待投递的唤醒后再 Drive |
| `RUNNING` | 非 nil | 可重试：IO、存储、租约冲突、可重试 Action 失败、取消 | 退避后重新 Drive，上限自定 |
| `FAILED` | 非 nil | 终态：handler 返回的 error、定义/类型漂移（`ErrChanged`）、误用（`ErrInvalid`）、编排 panic、WaitAny 全部失败（`ErrWaitFailed`）、没人 Wait 的 Go/GoFlow 业务错误 | 终止或人工处理；修复通过发布新版本 |

- 只有 handler **最终返回**的 error 才决定失败。编排代码处理掉的 `*ActionError` 不会让 run 失败，因此可以写补偿逻辑。
- 框架故障优先于挂起：任一分支中止时，即使其他分支在挂起，也报告 `RUNNING` 或 `FAILED`。

## Action

```go
type Action[I, O any] interface {
    Run(context.Context, I) (O, error)
}
```

- **适配**：普通函数和方法值用 `AsAction(fn)`；纯计算用 `Pure(fn)`。轻量计算直接在 Flow 里写，不必生成记录。
- **依赖**：客户端、Repository 等依赖放在 Action 里。Action 不持有 FlowContext、Engine 或可变的编排状态。
- **确定性**：时间和随机 ID 在 Flow 中用 `Now(key)` / `UUID(key)` 取得（会持久化为检查点），再作为输入传给 Action。
- **序列化**：输入、输出和信号负载必须能 JSON 编解码。成功的输出会先编码、再解码成新实例返回，首次执行和回放拿到的值完全一致。`any` 里的数字会变成 `float64`，需要精确数值时用明确的类型或 `json.Number`。
- **幂等**：结果提交前崩溃时，IO 类 Action 可能被重复执行。外部幂等键应组合“实例标识 + `ActionKey(ctx)`”，不要包含租约 generation。引擎不承诺外部副作用 exactly-once。

失败分类：

| 类型 | 来源 | 行为 |
| --- | --- | --- |
| 可重试 | 普通 error、Action panic（转为带堆栈的 `*PanicError`）、单次 `Timeout`、被中断的尝试（即使包装了 Permanent） | 默认中止本次 Drive，记录保持 `RUNNING`，由调用方重新 Drive；配置了 `Retry` 时在持有租约期间指数退避重试 |
| 业务失败 | `Permanent(err)`，或 `Retry.MaxAttempts` 用尽 | 提交为 `FAILED`，`Run` 返回 `*ActionError`。执行时 `Cause` 保留原始错误；回放时只有 `Message`，稳定的错误码请放进业务负载 |

```go
ctx.Run("charge", charge, order, workflow.ActionOptions{
    Timeout: 5 * time.Second, // 单次尝试的超时
    Retry:   workflow.RetryPolicy{MaxAttempts: 3, InitialInterval: 100 * time.Millisecond, MaxInterval: 10 * time.Second, Multiplier: 2},
})
```

重试次数按单次 Drive 计数，进程崩溃后重新计数。

## 等待与唤醒

`WaitFor`、`Sleep`、`SleepUntil` 是 Flow 原语，不在 Action 的租约内执行：

1. 首次执行时，`Put` 原子写入等待记录和投递意图：WaitFor 产生 REQUEST，Sleep 产生到期才可见的 WAKE。之后的 Drive 读到已有记录。
2. 提供方回答时调用 `engine.Signal(ctx, key, resp)`（幂等，无需版本号）或 `engine.ResolveSignal(ctx, key, version, resp)`（会校验请求是否过期）。响应和一个独立的 WAKE 会被原子提交。
3. 调度方收到 WAKE 后重新 Drive，等待点返回已持久化的响应。

几点语义：
- 重复回调保留第一次的结果，返回 false；版本过期返回 `ErrConflict`；还没登记就回调返回 `ErrNotFound`。
- 相对 Sleep 的截止时间在首次登记时固定，重新 Drive 不会推迟。
- `Result.Pending` 只是观察快照，可能落后于已经提交的回调，调度要以持久化的投递为准。

投递（谁、何时再次 Drive）不属于内核契约：内核只要求 Put/Resolve 原子地登记投递意图，怎么暴露（outbox、MQ……）由存储适配器决定。参考契约是 `memory.Outbox`，投递语义是至少一次：

| 消息 | 处理 |
| --- | --- |
| REQUEST | 把记录的 Input 交给提供方后 Ack；提供方用 `Signal` / `ResolveSignal` / `FailWait` 回答 |
| WAKE | Drive 该 run；结果不是 `RUNNING` 时才 Ack，可重试错误保留消息并退避 |
| CANCEL | 通知提供方停止已落败的等待（仅 WaitAny 产生）后 Ack |

重复的 WAKE 只会触发重放，迟到的回答返回 false，两者都是安全的。

## WaitAny

`f.WaitAny(group, cases...) Case` 类似 `select`：所有候选同时登记，第一个完成的候选成为赢家，其余候选在同一个原子操作中被取消。

```go
reply := workflow.OnSignal[Reply]("reply", request) // 显式写 Response 类型，Request 类型自动推断
code := workflow.OnSignal[int]("code", request)     // 同一组里的候选可以有不同响应类型
timeout := workflow.OnSleep("timeout", time.Hour)
switch f.WaitAny("decision", reply, code, timeout) {
case reply:
    return reply.Value().Text, nil
case code:
    return fmt.Sprint(code.Value()), nil
case timeout:
    return "timeout", nil
}
```

- 构造函数：`OnSignal[R](key, req)`、`OnSleep(key, d)`、`OnSleepUntil(key, t)`、`OnKind[R](kind, key, req)`（自定义 Kind，需要存储支持）。
- 返回值是传入的那个 Case 指针。`Value()` 只有赢家才是非零值；`Key()` 是展开后的存储 key；`Failure()` 是该候选被隔离时的失败原因。
- Case 只能使用一次，每次调用都要在 Flow 内重新构造。nil、重复使用或 Kind 不匹配都会在登记前以 `FAILED` + `ErrInvalid` 拒绝。
- 已到期的定时候选优先结算；多个同时到期时选最早的截止时间，相同则按声明顺序。截止之后才到的回调抢不走超时结果。
- 单个候选失败（请求无法编码，或提供方调用 `engine.FailWait(ctx, key, version, cause)`）只剔除该候选。**全部候选都失败**时 Drive 以 `FAILED` + `ErrWaitFailed` 终止。需要降级时，让提供方回答一个带类型的失败值，或者在组里加一个 Sleep 兜底。
- 需要存储实现可选的 `WaitAnyInfrastructure`；不支持时以 `FAILED` + `ErrWaitAnyUnsupported` 终止。

运行示例：`go run ./workflow/examples/wait_any`。

## 并发与 Loop

```go
left := w.GoFlow("left", leftFlow, in) // 先全部启动
right := w.GoFlow("right", rightFlow, in)
a, err := left.Wait() // 左侧还在等待时，这里直接挂起
if err != nil {
    return Out{}, err
}
b, err := right.Wait()
```

- `Go`（Action）和 `GoFlow`（Flow）各自运行在独立的 strand 上：一个分支挂起只停止它自己，兄弟分支继续执行，所以一次 Drive 能登记所有相互独立的等待。同一 strand 上的等待是顺序的。
- `Future.Wait` 可以重复、并发调用，返回 nil 或业务错误。任务在等待或 Drive 已中止时，`Wait` 会让调用方也挂起。
- Drive 返回前会汇合所有框架注册的分支，即使调用方忘了 Wait；没人 Wait 的业务错误会让 Drive 报告 `FAILED`。`RunFlow`/`GoFlow` 的完成也包含该 Flow 内的异步 Action。
- 框架故障会取消兄弟分支的 IO；单纯挂起不会取消兄弟分支。已启动的长 IO 会拖慢挂起返回，所以要给 Action 设置 `Timeout`。
- 不要在原生 goroutine 里使用或保存 FlowContext；不响应 context 的 Action 会阻塞汇合。框架不限制扇出并发数。

`Loop(key, action, input, LoopOptions{Interval, MaxAttempts, Action})` 把每次尝试记录为独立的 Action（`key|attempt_N`），间隔是独立的持久化 Sleep（`key|sleep_N`）。输出实现 `Done() bool`。已提交的尝试不会重复执行。

## Key、回放身份与版本

- 局部 key 必须匹配 `^[A-Za-z0-9_]+$`，需要手写。不做自动编号，因为 Go/GoFlow 的调度顺序不确定，自动序号会让回放错位。
- key 按挂载层级拼接：`RunFlow("pay")` 里的 `Run("charge")` 存为 `pay|charge`；Loop 和 WaitAny 组也各贡献一层。同一次 Drive 中 resolved key 不能重复；非法或重复的 key 会在登记前以 `FAILED` + `ErrInvalid` 拒绝。
- **回放身份 = resolved key + Kind + 类型路径（`Record.Type`）**，输入不参与比较。
  - 类型路径与 key 路径逐层对应：挂载的 Flow 为 `flow[I,O]`，Loop 为 `loop[I,O]`，WaitAny 组为 `any`；叶子为 `action[I,O]`、`signal[Req,Resp]`、`sleep`、`now[time.Time]`、`uuid[string]`。例如 `pay|charge` 对应 `flow[main.Order,string]|action[main.Order,int]`。
  - 任意一层的 I/O 类型变化都算漂移，返回 `FAILED` + `ErrChanged`。类型串取自 `reflect` 的类型名，因此类型改名或换包也算漂移；换成 I/O 相同的另一个 Action 实现不算。
  - 类型相同但输入变了：继续使用已持久化的结果（WaitFor 不重发请求，Sleep 不改截止时间），并发出 `input_changed` 事件。被中断的 `RUNNING` Action 重新执行时，改用本次的输入。
  - 没有类型路径的记录按输入严格比较。
- **版本绑定**：`$definition` 记录启动 run 时的名称、版本和输入，并严格比较。换版本或换输入后再 Drive 同一实例，返回 `FAILED` + `ErrChanged`。热修复以新版本发布、只用于新 run，在途 run 继续路由到原来的代码。内核不提供迁移。
- **实例隔离**：一个 Infrastructure 实例只绑定一个 run。内核接口里没有 RunID 或 TenantID，隔离由存储实现或装饰器负责。

## 存储契约

```go
type Infrastructure interface {
    lease.Infrastructure                                  // Claim / Renew / Release
    Get(context.Context, string) (*Record, error)
    Commit(context.Context, Lease, *Record, int64) (*Record, error)
    Put(context.Context, *Record) (*Record, error)
    Resolve(context.Context, string, int64, []byte) (bool, error)
}
```

- **Commit**：在一次原子写入中校验**当前**租约的 generation 和到期时间，以及记录版本；`expected=0` 表示只插入。存储层的 fencing 是拒绝旧 owner 写入的最终保证。
- **Put**：记录不存在时插入（检查点、定义、等待记录），否则返回已有记录，绝不覆盖已解析的信号。Signal/Sleep 的登记与投递意图必须原子写入。
- **Resolve**：校验 suspended 状态、Kind 和版本，原子提交响应和唤醒意图。已解析时返回 false，版本过期返回 `ErrConflict`。
- 所有返回的 Record 都必须是深拷贝，所有方法都要检查 ctx。超时可能意味着提交其实已成功，调用方要幂等重试。

可选接口（运行时做类型断言）：

| 接口 | 作用 |
| --- | --- |
| `RecordLister` | `Records(ctx)` 一次性返回全部记录。Drive 开始时预加载终态结果（已结束的 Action、已解析的等待、检查点），回放时不再逐步读写，类型漂移校验照常进行 |
| `Clock` | `Options.Now` 为空时作为默认时钟，让 Sleep 截止与租约到期同源 |
| `WaitAnyInfrastructure` | `WaitAny(ctx, group, []WaitRequest)` / `FailWait(ctx, key, version, msg)` |

WaitAny 存储的要求：
- 原子登记全部候选和组归属，持久化请求、失败列表和赢家。
- `Resolve` 必须同时升级：完成组成员时，在同一事务里选出赢家、取消其余成员、清理过时通知并登记 CANCEL。
- 回放时对每个成员按顺序调用 `workflow.SameWaitMember(stored, request)`，并持久化 `WaitRequest.Type`。
- 编码失败的候选（`WaitRequest.Error` 非空）也要按顺序保存，计入 Failures，不为它登记投递。

`workflow/memory` 是完整的参考实现。

## 观测

```go
unsubscribe := engine.Subscribe(func(ctx context.Context, e workflow.Event) {
    log.Info("workflow", "type", e.Type, "key", e.Key, "status", e.Status, "error", e.Error)
}, workflow.DriveFinished, workflow.ActionFinished) // 不传类型表示订阅全部
defer unsubscribe()
```

| 事件 | 触发时机 |
| --- | --- |
| `drive_started` / `drive_finished` | 每次 Drive 各一次，校验失败也会触发；`drive_finished.Pending` 包含全部挂起点 |
| `action_started` | 每次实际调用 Action 之前（重试时每次尝试都会触发）；回放不触发 |
| `action_finished` | 每个 Action 请求结束时一次，包括执行、回放和初始化失败 |
| `action_replayed` | 读到 `COMPLETED` / `FAILED` 记录并回放时 |
| `wait_suspended` | 每次经过一个未解决的等待点 |
| `wait_resolved` | 存储实际接受一次 Resolve 时（包括 Sleep 到期） |
| `input_changed` | 回放校验通过但输入与持久化输入不同时，仅作告警 |

- `Event` 可以 JSON 编解码（`SchemaVersion=1`），包含 `ID`、`EngineID`、`Sequence`、`ExecutionID`（每次 Drive 一个；Drive 之外的 ResolveSignal 为空）、名称和版本、`Kind`、`Status`、时间与耗时、`Replayed`、`RecordVersion`、`Pending`，以及 `Error{Category, Message, Operation}`。
- 错误类别：execution、panic、conflict、cancelled、timeout、infrastructure、lease、permanent、business、invalid、changed。
- 事件不包含业务输入输出，但错误文本由应用产生，注意不要写入凭证。
- 通知是同步的。监听者必须线程安全、快速返回，单个监听者的 panic 会被隔离。退订是幂等的，也可以在回调里调用。
- 这是进程内的观测接口，不是可靠的投递协议，可靠调度应依赖持久化的投递。

## 选项与租约

`Options{LeaseTTL, CleanupTimeout, Now}`：
- `LeaseTTL` 默认 30s，`CleanupTimeout` 默认 5s。
- 租约每 TTL/3 续一次，续租一直持续到结果提交完成。
- `Now` 为空时，若存储实现了 `Clock` 就用存储时钟，否则用 `time.Now`。显式注入的时钟必须与存储时钟一致。

租约生命周期由 `lease.Runner` 管理；`workflow.Lease` 是 `lease.Token` 的别名，`workflow.ErrConflict` 就是 `lease.ErrConflict`。

## 调试工具

```bash
go run ./workflow/cmd/wfdebug -addr 127.0.0.1:7788 -db wfdebug.db   # 启动调试服务
go run ./workflow/examples/debugger -run order-1                     # 运行示例 worker
# 打开 http://127.0.0.1:7788
```

在自己的工作流里接入：

```go
result, err := debugger.Run(ctx, definition, input, debugger.Options{RunID: "order-1"}) // 复用同一 ID 可在重启后恢复
```

`debugger.Run` 会注册 run、把引擎事件推送到时间线，并循环 Drive，直到 `COMPLETED` / `FAILED`；挂起时长轮询等待 WAKE。如果只要存储、自己控制 Drive，用 `debugger.NewClient(server, runID)`，它实现了 `Infrastructure`、`WaitAnyInfrastructure` 和 `memory.Outbox`。

UI 功能：
- 按 workflow 分组的 run 列表；
- 步骤的甘特时间线，以及记录的 Input / Result / Type；
- 引擎事件与存储事件时间线；
- 通知的投递状态；
- 对等待中的信号可以提交 JSON 推进、立即唤醒 Sleep、把 WaitAny 成员标记为失败；
- Re-drive 和删除 run。

语义说明：
- 存储语义与 `memory.Infrastructure` 一致：所有变更在单个 SQLite 事务中串行执行，时间以服务端时钟为准。
- 删除 run 会留下禁止复用 ID 的标记。
- UI 样式依赖 Tailwind CDN。

## 不提供的能力

生产数据库或 MQ 适配器、自动后台调度、实例取消、补偿事务、历史代码热迁移、工作流代码沙箱、扇出并发上限。

## 验证

```sh
go vet ./workflow/... ./lease/...
go test -race ./workflow/... ./lease/...
node --test workflow/debugger/ui_regression_test.cjs
```
