# Cflow

用 Go 代码描述编排、通过持久化快照回放执行结果的工作流**执行内核**。需要 Go 1.27.1+（使用泛型方法）。

Engine 用 `Start` 启动一次运行，之后每条唤醒用 `Resume` 从入口回放对应的执行单元（一次回放称为一次 drive），并返回本次结果。run 级状态（创建、终态、输出）、调度、重试上限和新旧流程（注册名）的路由都由调用方负责，内核不介入。

## 包结构

| 包 | 用途 |
| --- | --- |
| `cflow` | 引擎、编排原语、错误与事件模型、存储契约 |
| `cflow/memory` | 进程内参考存储 `memory.New()` 和参考投递契约 `memory.Outbox`，用于测试、示例和本地调试 |
| `cflow/debugger` | HTTP + SQLite 调试服务与 Web UI，`debugger.Run` 一行接入 |
| `cflow/tests` | 主包的黑盒行为测试 |
| `cflow/examples` | [可运行示例](examples/README.md)：依赖注入、竞争等待、独立子流程、多商品订单和调试器 |
| `github.com/k0923/go/lease` | 通用租约 Runner，cflow 复用它；lease 不依赖 cflow |

`cflow/legacy` 是旧实现，只保留做历史对照，与新版的数据格式不互通。

## 快速开始

```go
type SubmitOrder struct{ Client OrderClient }

func (a SubmitOrder) Run(ctx context.Context, in SubmitRequest) (SubmitResponse, error) {
    return a.Client.Submit(ctx, in) // 外部依赖由 Action 持有
}

type OrderFlow struct {
    cflow.FlowBase[OrderInput, OrderOutput]
    Submit cflow.Action[SubmitRequest, SubmitResponse]
}

func (f OrderFlow) Run(ctx *cflow.FlowContext, in OrderInput) (OrderOutput, error) {
    submitted, err := ctx.Run("submit", f.Submit, prepare(in))
    if err != nil { // 只可能是已提交的业务失败 *ActionError
        return OrderOutput{}, err
    }
    approval := ctx.WaitFor[Approval]("approval", submitted) // 未回调时直接挂起，不返回
    return build(submitted, approval), nil
}

definition := OrderFlow{Submit: SubmitOrder{client}}
engine, err := cflow.NewEngine(store, cflow.Options{
    Flows: map[string]cflow.FlowDefinition{"order": definition},
})
// 处理 err 后执行：
result, err := engine.Start(ctx, definition, input)
// result.Status: COMPLETED / SUSPENDED / RUNNING / FAILED
```

完整的可运行例子（提交 → 挂起 → 回调 → 恢复）见 [example_test.go](example_test.go)。

## 分层模型

只有两层，一种编排上下文：

| 层 | 定义 | 上下文 | 可用原语 |
| --- | --- | --- | --- |
| Flow | `Flow[I, O]` 接口或 `FlowFunc` | `FlowContext` | `Run`、`Go`、`Poll`、`ForEach`、`ForItems`、`WaitFor`、`WaitAny`、`Sleep`、`SleepUntil`、`Now`、`UUID`、`Scope`、`RunFlow`、`GoFlow` |
| Action | `Action[I, O]` 接口或 `actionFn` / `AsAction` / `Pure` | 标准 `context.Context` | 无编排能力 |

- **定义是类型，调用是实例**：所有入口统一注册到 `map[string]FlowDefinition`，名称只在 map 中声明一次。结构体嵌入 `FlowBase[I,O]` 后仍使用 `Run(*FlowContext,I)(O,error)`；`FlowFunc` 适配函数，保留输入输出的泛型检查。

  ```go
  order := cflow.FlowFunc[Order, Result](
      func(f *cflow.FlowContext, in Order) (Result, error) {
          pay := f.GoFlow("pay", payFlow, in)
          ship, err := f.RunFlow("ship", shipFlow, in)
          if err != nil { return Result{}, err }
          payment, err := pay.Wait()
          return Result{Shipment: ship, Payment: payment}, err
      })
  options := cflow.Options{Flows: map[string]cflow.FlowDefinition{
      "order": order, "payment": payFlow, "shipment": shipFlow,
  }}
  ```

- **根与子都是 Flow**：根通过 `Engine.Start(ctx, flow, input)` 启动，不追加命名空间，深度从 0 开始；子级通过 `f.RunFlow(name, flow, input)` / `f.GoFlow(name, flow, input)` 调用，name 是当前父级下的实例名，空字符串默认使用注册名。同一类型可用不同实例名调用多次。
- **每个子 Flow 是独立恢复边界**：首次调用原子保存输入并登记子级 WAKE，父级等待结果。调度器用 `Resume(ctx,key)` 恢复子级；回调只唤醒所属子级，子级终态提交后才唤醒父级。父级恢复时读取子级结果，不进入其内部回放。Action 随所属 Flow 回放。
- **边界有调度成本**：即使子级没有任何等待，一次调用也要经过「父级登记并挂起 → 子级 WAKE → `Resume(子级)` → 提交终态 → 父级 WAKE → `Resume(父级)` 回放」，每层至少两轮调度和若干次存储读写；N 层嵌套约 2N 轮。`GoFlow` 只是并发登记，实际并发度取决于调度器。需要独立恢复、独立等待或单独观察的逻辑才拆成子 Flow；纯计算或一两个 Action 的小步骤用普通函数或 `Scope` 组织即可。
- **已结束的子级随父级预加载**：父级回放时，存储随预加载一并返回它启动的已结束子级（含输入、输出与失败原因），父级直接使用并按当前调用校验身份，不再调用 `StartFlow`，也不额外写记录。
- **子级内的 Action 共用子级租约**：恢复子级时引擎持有并续租子级租约，子级内的 Action 直接以它提交，不再逐个加锁；根运行不持有租约，Action 仍各自加锁。
- **错误跨边界只保留可序列化部分**：子级返回的业务错误以 `*FlowFailure` 持久化，父级拿到的 error 保留消息，以及 `errors.As(..., *ActionError)`（含 `Key`）；不保留任意 Go 错误的 `errors.Is` 身份和 `Cause`，稳定的错误码请放进业务输出或错误消息。子级的框架故障（定义漂移、误用、编排 panic 等）不交给父级业务代码：父级回放到该调用时整个运行以 `FAILED` 终止，即使父级写了 `if err != nil` 兜底也不会吞掉。
- 结构体定义按 Go 类型识别（指针和值共享类型身份），同一类型只注册一次；执行使用注册对象持有的依赖。函数定义按 `FlowFunc` 返回的对象识别，同签名的不同函数可分别注册；不要在编排体内重新构造函数定义。注册表在创建引擎时复制，定义应保持不可变。
- 嵌套深度由 `Options.MaxFlowDepth` 限制（默认 8），超过时以 `FAILED` + `ErrInvalid` 终止。nil、缺少匹配的 Run 或 FlowBase 泛型签名不一致的定义在注册时拒绝。
- 根 Flow 与子 Flow 使用同一注册表；调用的 Flow 必须已注册。

## 控制流与错误语义

挂起和框架故障用**私有 panic** 展开编排代码，业务失败用 **error** 返回：

| 情况 | 编排代码看到什么 |
| --- | --- |
| 等待未就绪（WaitFor / Sleep / WaitAny / 等待中的 Future） | 不返回，当前 goroutine 被展开 |
| 框架故障：存储、租约、可重试 Action 失败、误用、定义漂移（Kind/Path/注册名）、超过 MaxFlowDepth、WaitAny 全部候选失败 | 同上，本次 Drive 中止 |
| 已提交的业务失败 | `Run`/`Go`/`Poll` 返回 `*ActionError`；`Poll` 次数耗尽返回包装过的 `ErrPollExhausted` |

- 等待原语（`WaitFor`、`Sleep`、`WaitAny`、`Now`、`UUID`）只返回值，不返回 error。
- 展开时 defer 照常执行，所以编排代码里的 defer 不能有副作用。
- 即使编排代码 `recover()` 了框架 panic，同一条执行路径（strand）上后续的原语也会再次停止，不会持久化新进度；如果吞掉后正常返回，Drive 报告 `Failed`。

`Start` / `Resume` 的返回：

| Status | err | 含义 | 调用方的处理 |
| --- | --- | --- | --- |
| `COMPLETED` | nil | 编排完成，`Output` 有效 | 记录终态 |
| `SUSPENDED` | nil | 存在挂起点，`Pending` 列出全部 | 等待投递的唤醒后再 `Resume` |
| `RUNNING` | 非 nil | 可重试：IO、存储、租约冲突、可重试 Action 失败、取消 | 退避后重新 `Resume`，上限自定 |
| `FAILED` | 非 nil | 终态：handler 返回的 error、定义漂移（`ErrChanged`）、误用或超深嵌套（`ErrInvalid`）、结果解码失败、编排 panic、WaitAny 全部失败（`ErrWaitFailed`）、没人 Wait 的 Go/GoFlow 业务错误 | 终止或人工处理；修复以新注册名发布新流程 |

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
ctx.Run("charge", charge, order, cflow.ActionOptions{
    Timeout: 5 * time.Second, // 单次尝试的超时
    Retry:   cflow.RetryPolicy{MaxAttempts: 3, InitialInterval: 100 * time.Millisecond, MaxInterval: 10 * time.Second, Multiplier: 2},
})
```

重试次数按单次 Drive 计数，进程崩溃后重新计数。

## 等待与唤醒

`WaitFor`、`Sleep`、`SleepUntil` 是 Flow 原语，不在 Action 的租约内执行：

1. 首次执行时，`Put` 原子写入等待记录和投递意图：WaitFor 产生 REQUEST，Sleep 产生到期才可见的 WAKE。之后的 Drive 读到已有记录。
2. 提供方回答时调用 `engine.Signal(ctx, key, resp)`（幂等，无需版本号）或 `engine.ResolveSignal(ctx, key, version, resp)`（会校验请求是否过期）。响应和一个独立的 WAKE 会被原子提交。
3. 调度方收到 WAKE 后调用 `engine.Resume(ctx, notification.ExecutionKey)`（空串是根运行），等待点返回已持久化的响应。

几点语义：
- 重复回调保留第一次的结果，返回 false；版本过期返回 `ErrConflict`；还没登记就回调返回 `ErrNotFound`。
- 相对 Sleep 的截止时间在首次登记时固定，重新 Drive 不会推迟。
- `Result.Pending` 只是观察快照，可能落后于已经提交的回调，调度要以持久化的投递为准。

投递（谁、何时再次 Drive）不属于内核契约：内核只要求 Put/Resolve 原子地登记投递意图，怎么暴露（outbox、MQ……）由存储适配器决定。参考契约是 `memory.Outbox`，投递语义是至少一次：

| 消息 | 处理 |
| --- | --- |
| REQUEST | 把记录的 Input 交给提供方后 Ack；提供方用 `Signal` / `ResolveSignal` / `FailWait` 回答 |
| WAKE | `engine.Resume(ctx, n.ExecutionKey)`，根运行与子级同一种调用；结果不是 `RUNNING` 时才 Ack，可重试错误保留消息并退避 |
| CANCEL | 通知提供方停止已落败的等待（仅 WaitAny 产生）后 Ack |

重复的 WAKE 只会触发重放，迟到的回答返回 false，两者都是安全的。

## WaitAny

`f.WaitAny(group, cases...) Case` 类似 `select`：所有候选同时登记，第一个完成的候选成为赢家，其余候选在同一个原子操作中被取消。

```go
reply := cflow.OnSignal[Reply]("reply", request) // 显式写 Response 类型，Request 类型自动推断
code := cflow.OnSignal[int]("code", request)     // 同一组里的候选可以有不同响应类型
timeout := cflow.OnSleep("timeout", time.Hour)
switch f.WaitAny("decision", reply, code, timeout) {
case reply:
    return reply.Value().Text, nil
case code:
    return fmt.Sprint(code.Value()), nil
case timeout:
    return "timeout", nil
}
```

- 构造函数：`OnSignal[R](name, req)`、`OnSleep(name, d)`、`OnSleepUntil(name, t)`。
- 返回值是传入的那个 Case 指针。`Value()` 只有赢家才是非零值；`Key()` 是展开后的存储 key；`Failure()` 是该候选被隔离时的失败原因。
- Case 只能使用一次，每次调用都要在 Flow 内重新构造。nil、重复使用或 Kind 不匹配都会在登记前以 `FAILED` + `ErrInvalid` 拒绝。
- 已到期的定时候选优先结算；多个同时到期时选最早的截止时间，相同则按声明顺序。截止之后才到的回调抢不走超时结果。
- 单个候选失败（请求无法编码，或提供方调用 `engine.FailWait(ctx, key, version, cause)`）只剔除该候选。**全部候选都失败**时 Drive 以 `FAILED` + `ErrWaitFailed` 终止。需要降级时，让提供方回答一个带类型的失败值，或者在组里加一个 Sleep 兜底。
- 需要存储实现可选的 `WaitAnyInfrastructure`；不支持时以 `FAILED` + `ErrWaitAnyUnsupported` 终止。

运行示例：`go run ./cflow/examples/wait_any`。

## 并发与 Poll

```go
left := f.GoFlow("left", leftFlow, in) // 先全部启动
right := f.GoFlow("right", rightFlow, in)
a, err := left.Wait() // 左侧还在等待时，这里直接挂起
if err != nil {
    return Out{}, err
}
b, err := right.Wait()
```

- `Go`（Action）在当前执行中并发运行；`GoFlow` 并发登记独立子执行，实际执行的并发度由外部调度器控制。一个分支挂起只停止它自己，所以一次 Drive 能登记所有独立子调用。同一 strand 上的调用是顺序的。
- `Future.Wait` 可以重复、并发调用，返回 nil 或业务错误。任务在等待或 Drive 已中止时，`Wait` 会让调用方也挂起。
- Drive 返回前会汇合所有框架注册的分支，即使调用方忘了 Wait；没人 Wait 的业务错误会让 Drive 报告 `FAILED`。`RunFlow`/`GoFlow` 的完成也包含该 Flow 内的异步 Action。
- 同一次 Drive 内的框架故障会取消兄弟 Action 的 IO；单纯挂起不会取消兄弟分支。独立子 Flow 的重试不终止其他执行。已启动的长 IO 会拖慢挂起返回，所以要给 Action 设置 `Timeout`。
- 不要在原生 goroutine 里使用或保存 FlowContext；不响应 context 的 Action 会阻塞汇合。框架不限制扇出并发数。

`Poll(name, action, input, PollOptions{Interval, MaxAttempts, Action})` 轮询一个 Action 直到输出的 `Done()` 为真：每次尝试记录为独立的 Action（`name|attempt_N`），间隔是独立的持久化 Sleep（`name|sleep_N`）。输出实现 `Done() bool`。已提交的尝试不会重复执行。

## ForEach / ForItems（Experimental / 实验性、不稳定）

`ForEach(name, body, items, key, opts...)` 为集合中的每个元素登记独立、可恢复的 Flow，全部成功后返回 `[]O`：一个输入对应一个输出，按输入顺序排列，与完成顺序无关。`ForItems` 参数相同，但丢弃每个元素的输出，只返回 `error`。它可以复用任意已注册的 Flow，元素输出既不保存，父级也不加载。API、存储契约及持久化格式尚不稳定，不保证实验版本之间旧运行数据的兼容性。

循环体与根 Flow 都在每个 worker 的 `Options.Flows` 注册。恢复时只需要元素 key、注册定义及持久化输入；变化的业务数据必须放在输入里，不能捕获父流程的可变局部状态。不需要处理的元素，先在代码里过滤掉再传入。

```go
orderFlow := cflow.FlowFunc[string, string](func(f *cflow.FlowContext, id string) (string, error) {
        return f.WaitFor[string]("approval", id), nil
    })
notifyFlow := cflow.FlowFunc[string, string](func(f *cflow.FlowContext, id string) (string, error) {
        return f.Run("send", sendNotice, id) // 返回回执，ForItems 会丢弃它
    })
workflow := cflow.FlowFunc[[]string, []string](func(f *cflow.FlowContext, ids []string) ([]string, error) {
        byID := func(id string) string { return id }
        approvals, err := f.ForEach("orders", orderFlow, ids, byID, cflow.Concurrency(5))
        if err != nil {
            return nil, err
        }
        return approvals, f.ForItems("notify", notifyFlow, ids, byID, cflow.Concurrency(5))
    })
engine, err := cflow.NewEngine(store, cflow.Options{
    Flows: map[string]cflow.FlowDefinition{"batch": workflow, "order": orderFlow, "notify": notifyFlow},
})
// 处理 err 后：Start 登记整批并挂起，调度器对每条 WAKE 调用 Resume。
```

**并发是滑动窗口。** `Concurrency(n)` 最多同时激活 n 个元素，默认 1，即顺序执行。以 8 个元素、`Concurrency(5)` 为例：登记时激活前 5 个，之后每有一个元素终结（成功或失败），就在同一事务里激活下一个排队元素，直到 8 个全部激活。框架只控制激活：每个被激活的元素产生一条 WAKE。这些元素用 goroutine、消息队列还是 worker pool 执行，由调度器决定。窗口在批次首次登记时固定，之后修改 `Concurrency` 不报错，也不影响已登记的批次。

- 每项地址为 `orders|item[<key>]`，内部步骤如审批为 `orders|item[<key>]|approval`，均沿用结构化 `Path` 和 `KeyBuilder`。元素身份只使用显式 `key func(I) string`，不读取元素的 `StepKey()`。
- key 必须是纯函数、稳定、非空、有效 UTF-8、不超过 256 字节且批内唯一。登记前校验整批，非法项不会留下部分批次；空集合下 `ForEach` 返回非 nil 的空切片。首次登记固定有序 key、输入、循环体注册名以及是否丢弃输出（`ForEach` 和 `ForItems` 不能互换），后续漂移返回 `ErrChanged`。
- `StartLoop` 原子保存批次和各元素输入，并激活窗口内的元素。父流程挂起于 `LoopKind`，不进入元素体。
- 调度器对每条 WAKE 调用 `engine.Resume(ctx, notification.ExecutionKey)`。元素内等待回调及定时器的唤醒指向所属元素，因此只回放该元素。结果为 `RUNNING` 时保留消息并退避重试；其他状态可 ACK，子项 `FAILED` 仍须处理随后投递的父级唤醒。
- 元素完成时，`CommitFlow` 校验该元素的租约及版本，并在同一事务里完成：保存结果、只更新一次计数（`AdmittedCount`、`RunningCount`、`CompletedCount`）、激活下一个排队元素。没有运行中的元素时，循环终结并唤醒父级，父级通过 `LoopResults` 批量读取结果。重复投递、完成提交后响应丢失均不会重复计数。嵌套循环的唤醒指向所属父元素。
- 终态循环随父级预加载一并返回（实现 `ExecutionLoader` 时）。`ForEach` 附带全部输出，`ForItems` 不带输出，父级回放都不再调用 `GetLoop`/`LoopResults`；输入指纹不一致报 `ErrChanged`。
- 元素挂起或可重试故障不计数、不腾出窗口。终态失败后停止激活新元素，已在运行的元素继续执行到终态，之后父级恢复：`ForEach` 返回 `nil` 和 `*ForEachError`，`ForItems` 返回 `*ForEachError`。并发下已完成的元素不一定连续，所以不返回部分结果。多个元素失败时报告最先提交的那个（`Index`、`Key`）。错误从持久化数据重建：保留消息及 `errors.As(..., *ActionError)`，不保留任意 Go 错误的 `errors.Is` 身份。父级可处理该错误，根运行是否失败以根运行的结果为准。元素的框架故障不返回 `*ForEachError`，而是令父级以 `FAILED` 终止；它不会被先前的业务失败掩盖。
- 元素内支持 Action、WaitFor、WaitAny、Sleep、RunFlow/GoFlow 和嵌套 ForEach/ForItems；完成前汇合异步分支，并计入 `MaxFlowDepth`。

此功能需要存储实现可选的 `FlowInfrastructure`；内存参考实现和 debugger SQLite/HTTP 已支持，`debugger.Run` 已处理路由。自定义适配器须保证登记、租约校验、结果、计数、后继激活与 outbox 的事务原子性，返回深拷贝快照，终态重复提交返回首次结果。

性能边界：首次登记和 `ForEach` 的最终汇总仍处理 O(n) 数据，适合保存稳定 ID 数组；`ForItems` 汇总时不读取元素输出。元素恢复只读取自身步骤，不遍历其他元素；根运行也只读取自身步骤。单个元素仍从自身入口回放，当前不提供分页。

完整调度、审批与重建引擎示例：`go run ./cflow/examples/for_each`。

## Key、Path 与回放身份

### 名字固定，身份来自数据

代码里写的是**固定的语义名**（`llm`、`tool_call`、`approve`），重复步骤的唯一性由**数据**提供，框架不做任何隐式计数：

| 机制 | 用法 | 地址 |
| --- | --- | --- |
| `StepKeyer` | 输入实现 `StepKey() string`，适用于 `Run`、`Go`、`Poll`、`RunFlow`、`GoFlow`、`WaitFor` 和 `OnSignal` 的请求 | `tool_call[call_7]` |
| `Scope(name, id)` | 为循环或分支显式开一层命名空间，不产生记录 | `step[0]\|llm` |
| 外层命名空间 | 没有输入的原语（`Sleep`、`Now`、`UUID`、WaitAny 组名）只写固定名，唯一性继承自外层 | `approve[call_7]\|timeout` |

```go
func (c ToolCall) StepKey() string { return c.ID }

for i := range maxSteps {
    s := f.Scope("step", strconv.Itoa(i))
    resp, _ := s.Run("llm", llm, req)                 // step[0]|llm
    for _, call := range resp.ToolCalls {
        c := s.Scope("call", call.ID)
        c.WaitAny("approve", decision, timeout)        // step[0]|call[call_7]|approve|timeout
        s.Go("tool_call", tools, call)                 // step[0]|tool_call[call_7]
    }
}
```

- 名字必须匹配 `^[A-Za-z0-9_]+$`；ID 是任意 UTF-8，最长 `MaxIDLength`（256）字节。
- `StepKey` 只能由输入数据推导，不能包含时间或随机数。它的输入本身来自已持久化的结果（例如 LLM 返回的 tool call ID），因此回放时保持一致。`StepKey` panic 按定义缺陷处理（`ErrInvalid`）。
- 同一次 Drive 中，相同地址出现两次会以 `FAILED` + `ErrInvalid` 拒绝。

### 地址规则：每层是下一层的前缀

整个 run 内地址唯一，规则只有一条：**每个原语在当前命名空间下追加一段 `name[id]`，Scope、子 Flow 与 ForEach 把自己的地址作为前缀传给内部。** 用户写的名字就是地址，框架不在其中插入隐藏段。

| 调用 | 地址 | 说明 |
| --- | --- | --- |
| `f.Run("charge", …)`、`f.WaitFor("approval", …)`、`f.Now("at")` | `charge`、`approval`、`at` | 输入实现 `StepKeyer` 时为 `name[id]` |
| `f.Scope("call", id)` 内的 `Run("x")` | `call[id]\|x` | Scope 只加前缀，本身不写记录 |
| `f.RunFlow("pay", …)` 内的 `Run("charge")` | 子级 `pay`，步骤 `pay\|charge` | 子级地址即其执行单元 key |
| `f.ForEach("orders", …)` | 循环 `orders`，元素 `orders\|item[<key>]`，元素内步骤 `orders\|item[<key>]\|…` | `item` 是唯一由框架命名的段，代表集合元素 |
| `f.Poll("poll", …)` | `poll\|attempt_N`、`poll\|sleep_N` | 每次尝试与间隔本身就是可观察的步骤 |
| `f.WaitAny("decide", …)` | 组 `decide`，候选 `decide\|<候选名>` | |

另外每个 run 有一条 `$definition` 记录，保存入口名称和输入，用于校验身份；它不是步骤，`$` 开头的地址不会与用户名字冲突。除以上地址外，框架不写入其他记录；预加载、子级结果复用都直接使用这些记录本身。

### Path 与 BuildKey

- 每个步骤的结构化地址是 `Path []Segment{Name, ID}`。`Record`、`WaitRequest`、`Pending`、`Event` 都会带上它，**存储层按自己的方式持久化**：拆成列、存成数组或建成树。
- 引擎、租约、`Get`/`Commit`/`Resolve`、`Signal` 仍然使用字符串 key，它由 `BuildKey(path)` 生成：
  - 存储层可以实现可选接口 `KeyBuilder` 自定义编码，否则使用 `DefaultKey`。
  - `DefaultKey` 编码为 `name[id]`，各段用 `|` 连接，ID 中的 `\`、`[`、`]`、`|` 用反斜杠转义，因此编码是单射的。
- `BuildKey` 必须是纯函数、跨版本稳定、单射。框架有两层兜底：
  - 同一次 Drive 内，两个不同 Path 映射到同一个 key 时，报 `ErrInvalid: BuildKey not injective`。
  - 跨 Drive 时，存储的 `Record.Path` 与期望路径不一致，报 `ErrChanged`。

### 回放身份 = key + Kind（+ Path）

- **类型不参与身份。** `Record.Type` 只记录叶子 I/O 签名（如 `action[main.Order,int]`）用于观测。类型改名、换包都照常回放；不一致时发出 `type_changed` 事件，事件里带 `StepType`（当前）和 `StoredType`（记录值）。真正不兼容的结构仍会在 JSON 解码时以 fatal 失败。
- **输入不参与身份。** 输入变化时继续使用已持久化的结果（WaitFor 不重发请求，Sleep 不改截止时间），并发出 `input_changed` 事件。被中断的 `RUNNING` Action 重新执行时，改用本次的输入和类型。
- **Kind 变化**（例如同一个地址从 WaitFor 改成 Run），或者 Now 与 UUID 互换，都报 `ErrChanged`。
- **名字即版本**：`$definition` 记录启动 run 时的注册名和输入，并严格比较；子级与循环同样记录注册名。框架没有单独的版本号，内核不提供迁移，也不鼓励在一份定义里按版本写分支。
- **改流程 = 写新流程**：版本号意味着编排体里出现 `if version == ...` 一类分支，且分支只增不减。推荐做法：
  - 已发布的 Flow 不改步骤名、顺序、Kind 和子调用结构；只做不影响回放的修改（Action 内部实现、日志等）。
  - 需要改变语义时，以新的注册名写一个新 Flow（如 `order_v2` 或更有意义的新名字），新 run 从新入口启动；子 Flow 同理，新父级调用新子级。
  - 旧定义继续留在 `Options.Flows` 中，直到其在途 run 全部结束再删除。恢复时找不到注册名按可重试处理，以兼容滚动发布。
  - 原地修改已有名字背后的结构（步骤 Kind、Path）会在回放时以 `ErrChanged` 失败；只改输入或类型则照常回放并发出事件。
- **部署参数漂移**：已登记的子级若与当前 `BuildKey` 或 `MaxFlowDepth` 不再匹配（例如调小了深度上限），恢复时以 `FAILED` + `ErrInvalid` 终止，父级随之失败，而不是无限重试。
- **实例隔离**：一个 Infrastructure 实例只绑定一个 run，key 只在这个实例内有意义。RunID/TenantID 由存储实现拼进实际的存储键。

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

- **Commit**：在一次原子写入中校验**当前**租约的 generation 和到期时间，以及记录版本；`expected=0` 表示只插入。存储层的 fencing 是拒绝旧 owner 写入的最终保证。租约可以是 Action 自己的（`Lease.Key == Record.Key`，根运行），也可以是所属子级的（`Lease.Key == Record.ExecutionKey`，恢复子级时）；其他组合返回 `ErrInvalid`，已有记录的 `ExecutionKey` 不得改变。
- **Put**：记录不存在时插入（检查点、定义、等待记录），否则返回已有记录，绝不覆盖已解析的信号。Signal/Sleep 的登记与投递意图必须原子写入。
- **Resolve**：校验 suspended 状态、Kind 和版本，原子提交响应和唤醒意图。已解析时返回 false，版本过期返回 `ErrConflict`。
- 所有返回的 Record 都必须是深拷贝，所有方法都要检查 ctx。超时可能意味着提交其实已成功，调用方要幂等重试。

可选接口（运行时做类型断言）：

| 接口 | 作用 |
| --- | --- |
| `RecordLister` | `Records(ctx)` 列出全部步骤，仅用于观测 |
| `ExecutionLoader` | `LoadExecution(ctx, executionKey)` 返回某个执行边界回放所需的 `ExecutionSnapshot`：它拥有的记录（`Record.ExecutionKey`，根为空串）、它启动的独立子级与循环（`ParentKey`），成功的 `ForEach` 循环附带全部输出。每次 `Start`/`Resume` 开头调用一次，终态项直接从快照回放；不实现时逐步读取，语义相同。不得返回其他边界或子级内部的数据。每个边界每次恢复都会调用，实现方需按归属（`ExecutionKey`/`ParentKey`）建索引，使成本只取决于该边界自身的数据，而不是整个运行（参考实现 memory 与 debugger 均如此） |
| `KeyBuilder` | `BuildKey(Path) string` 决定 Path 到字符串 key 的编码；不实现时使用 `DefaultKey` |
| `Clock` | `Options.Now` 为空时作为默认时钟，让 Sleep 截止与租约到期同源 |
| `WaitAnyInfrastructure` | `WaitAny(ctx, group, []WaitRequest)` / `FailWait(ctx, key, version, msg)` |
| `FlowInfrastructure` | 子级登记 `StartFlow`、快照 `GetFlow`、租约提交 `CommitFlow`，以及批次 `StartLoop` / `GetLoop` / `LoopResults` |

WaitAny 存储的要求：
- 原子登记全部候选和组归属，持久化请求、失败列表和赢家。
- `Resolve` 必须同时升级：完成组成员时，在同一事务里选出赢家、取消其余成员、清理过时通知并登记 CANCEL。
- 回放时对每个成员按顺序调用 `cflow.SameWaitMember(stored, request)`（比较 Key、Kind、Path），并持久化 `WaitRequest.Path` 和 `WaitRequest.Type`。
- 编码失败的候选（`WaitRequest.Error` 非空）也要按顺序保存，计入 Failures，不为它登记投递。

`cflow/memory` 是完整的参考实现。

### 存储适配辅助函数

以下导出函数集中在 [store_helpers.go](store_helpers.go)，**只给 Infrastructure 实现使用**，编排代码用不到。memory 和 debugger 两个参考实现都通过它们保持与引擎一致的行为，自定义存储也应复用：

| 函数 | 用途 |
| --- | --- |
| `SameFlow` / `SameLoop` / `SameWaitMember` | 重复登记时判断与已存数据是否一致，不一致返回 `ErrChanged` |
| `ValidateFlow` / `ValidateLoopBatch` | 首次登记前校验整份数据，非法时不写入任何内容 |
| `NewLoopState` | 批次登记后的初始进度：前 `Window()` 个元素已激活，适配器为这些元素投递唤醒 |
| `ApplyFlowCompletion` | 子级终态提交：校验版本、更新子级与循环计数，并返回下一个要激活的元素下标（无则为 -1）；适配器再原子落库、激活该元素，并在循环终结时唤醒父级 |
| `FlowWakeID` / `FlowCompletionWakeID` | 子级激活与完成唤醒的稳定投递 ID，与步骤唤醒互不冲突 |

## 观测

```go
unsubscribe := engine.Subscribe(func(ctx context.Context, e cflow.Event) {
    log.Info("workflow", "type", e.Type, "key", e.Key, "status", e.Status, "error", e.Error)
}, cflow.DriveFinished, cflow.ActionFinished) // 不传类型表示订阅全部
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
| `type_changed` | 回放校验通过但叶子类型签名与记录不同时（改名、换包或真实变更），仅作告警；带 `step_type` / `stored_type` |

- `Event` 可以 JSON 编解码（`SchemaVersion=1`），包含 `ID`、`EngineID`、`Sequence`、`ExecutionID`（每次 Drive 一个；Drive 之外的 ResolveSignal 为空）、流程注册名 `WorkflowName`、`Key`、`Path`、`Kind`、`StepType`（当前代码的叶子类型）、`Status`、时间与耗时、`Replayed`、`RecordVersion`、`Pending`，以及 `Error{Category, Message, Operation}`。
- 类型信息只在事件中提供，用于排查。是否记录全部事件由业务方决定。
- 错误类别：execution、panic、conflict、cancelled、timeout、infrastructure、lease、permanent、business、invalid、changed。
- 事件不包含业务输入输出，但错误文本由应用产生，注意不要写入凭证。
- 通知是同步的。监听者必须线程安全、快速返回，单个监听者的 panic 会被隔离。退订是幂等的，也可以在回调里调用。
- 这是进程内的观测接口，不是可靠的投递协议，可靠调度应依赖持久化的投递。

## 选项与租约

`Options{Flows, LeaseTTL, CleanupTimeout, MaxFlowDepth, Now}`：
- `Flows` 是统一注册表，根与子 Flow 都在这里注册；`Resume` 根据记录的注册名找到根 Flow。注册名即 Flow 的身份，改流程请注册新名字（见「回放身份」）。
- `LeaseTTL` 默认 30s，`CleanupTimeout` 默认 5s，`MaxFlowDepth` 默认 8。
- 租约每 TTL/3 续一次，续租一直持续到结果提交完成。
- `Now` 为空时，若存储实现了 `Clock` 就用存储时钟，否则用 `time.Now`。显式注入的时钟必须与存储时钟一致。

租约生命周期由 `lease.Runner` 管理；`cflow.Lease` 是 `lease.Token` 的别名，`cflow.ErrConflict` 就是 `lease.ErrConflict`。

## 调试工具

```bash
go run ./cflow/cmd/wfdebug -addr 127.0.0.1:7788 -db wfdebug.db   # 启动调试服务
go run ./cflow/examples/debugger -run order-1                     # 运行示例 worker
# 打开 http://127.0.0.1:7788
```

在自己的工作流里接入：

```go
result, err := debugger.Run(ctx, definition, input, debugger.Options{
    RunID: "order-1", Engine: options,
}) // options 含 Flows；复用同一 ID 可在重启后恢复
```

`debugger.Run` 会注册 run、把引擎事件推送到时间线，并对每条 WAKE 调用 `Resume`，直到根运行 `COMPLETED` / `FAILED`；挂起时长轮询等待 WAKE。如果只要存储、自己调度，用 `debugger.NewClient(server, runID)`，它实现了 `Infrastructure`、`WaitAnyInfrastructure`、实验性的 `FlowInfrastructure` 和 `memory.Outbox`。

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
go vet ./cflow/... ./lease/...
go test -race ./cflow/... ./lease/...
node --test cflow/debugger/ui_regression_test.cjs
```
