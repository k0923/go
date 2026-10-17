# 任意完成的多路等待

`FlowContext.WaitAny(groupKey, cases...)` 将多个独立候选同时登记为一个等待组，写法类似 `select`。WaitFor 和 Sleep 是两类候选：分别由外部响应和到期时间触发。第一个完成的候选确定赢家，并在同一个原子操作中取消其他候选。它不等待其他响应，也不为候选创建常驻 goroutine。

## 外部响应或超时

```go
reply := workflow.OnSignal[Reply]("reply", request) // Response 显式写，Request 自动推断
timeout := workflow.OnSleep("timeout", 30*time.Second)
switch flowCtx.WaitAny("reply_or_timeout", reply, timeout) {
case reply:
    return reply.Value().Text, nil
case timeout:
    return "timeout", nil
}
```

- 每个候选有**自己的响应类型**：`OnSignal[Approval]`、`OnSignal[int]`、`OnSignal[json.RawMessage]` 可以放在同一组里，WaitAny 本身不是泛型。
- 与 WaitFor/Sleep 一样，WaitAny 只返回值、不返回 error：候选只是外部信号，挂起和故障都由引擎处理。
- WaitAny 返回赢家 `Case` 本身（即传入的那个指针），直接用 `==` 或 `switch` 判断；`Value()` 返回已解码的响应，只有赢家非零值。`Key()` 是解析后的存储 key，`Kind()` 是信号种类，`Failure()` 是该候选被隔离的错误。
- 构造函数：`OnSignal[Response](key, request)`（WaitFor）、`OnSleep(key, d)`、`OnSleepUntil(key, t)`、`OnKind[Response](kind, key, request)`（自定义 Kind）。它们只创建描述，不立即等待。
- Case 只能用一次：每次调用 WaitAny 都在 Flow 内部重新构造。重复使用同一个 Case、传入 nil 或 Kind 与构造方式不匹配，会在注册前以 `Failed` + `ErrInvalid` 拒绝。
- `f.WaitFor`、`f.Sleep`、`f.SleepUntil` 仍是单信号等待。混合等待的可执行例子见 `example_test.go`。

相对 Sleep 的截止时间只在首次登记时由存储计算，重驱不会推迟它；绝对 Sleep 保存指定 Until。存储推进时优先结算已到期的定时候选；多个定时候选同时到期时选最早的 Until，Until 相同则按声明顺序选。截止后才到达的外部回调不能抢走超时结果。定时候选会登记一个在 Until 时可见的 WAKE，由外部调度触发 Drive，不需要常驻计时 goroutine。

`OnKind` 可以描述自定义 Kind 的候选，其登记、触发、解析和取消规则由自定义存储实现。`memory.Infrastructure` 目前只实现 SignalKind 和 SleepKind，遇到未知种类会明确返回 ErrSignalUnsupported，不会悄悄按 WaitFor 处理。

## 两个外部信号

```go
primary := workflow.OnSignal[string]("primary", "order_123")
secondary := workflow.OnSignal[string]("secondary", "order_123")
winner := flowCtx.WaitAny("approvals", primary, secondary)
// 只需一个响应即可继续；winner.Key() 是展开后的赢家 key。
if winner == primary {
    return primary.Value(), nil
}
return secondary.Value(), nil
```

完整的双等待及外部监听清理示例：

```sh
go run ./workflow/examples/wait_any
```

输出：

```text
active waits: 2
continued: secondary approved
primary: CANCELLED
provider listeners stopped
```

另有 `example_test.go`，可以这样运行：

```sh
go test -race ./workflow ./workflow/tests -run '^(TestFlowContextWaitAny|ExampleFlowContext_WaitAny|ExampleOnSignal)$' -v
```

## 执行机制

1. Flow 调用一次 WaitAny，先校验所有局部 key 和 Case，再编码请求。
2. 存储原子登记全部候选及等待组归属：WaitFor 登记 REQUEST，Sleep 登记在 Until 时可见的 WAKE。尚未到期的有效候选都处于 SUSPENDED，Drive 返回所有 Pending。已到期的 Sleep 会直接完成并取消其他候选。
3. 外部业务方各自处理 REQUEST，对自己的 key 调用 `Signal(ctx, key, response)` 或 `ResolveSignal(ctx, key, version, response)`。
4. 第一笔成功的 Resolve 在同一原子操作中保存赢家、取消其余 SUSPENDED 成员、删除过时的请求通知，并登记赢家的 WAKE 与败者的 CANCEL 通知。
5. 外部调度方消费 WAKE 后立即 Drive；WaitAny 返回已持久化的赢家，后续逻辑继续执行，无需等败者结束。
6. CANCEL 消费方按 key 取消外部 context、注销监听句柄并确认消息；取消前后到达的迟到回调都返回 false，不会改变赢家。

这里沿用当前库的“持久化挂起 → 回调 → 重新 Drive”模型，不会把同一个 Go 调用一直阻塞在内存里。WAKE 消费者应及时驱动流程，内核没有自动后台调度线程（参考调度契约见 `memory.Outbox`）。并发完成时，赢家由存储的原子提交顺序决定，不依赖客户端时间戳。

外部 IO 的取消是协作式的：内核在 Resolve 时立即结束等待状态，已投递出去的外部任务由 CANCEL 消费方释放资源。示例用 context.CancelFunc 和 WaitGroup 演示清理。内核无法跨进程强制终止业务方的线程。

## 异常与资源

- 单个请求无法 JSON 编码，或其 MarshalJSON 发生 panic：该候选被隔离（`Failure()` 非空），其他有效候选继续登记。
- 外部候选执行失败：调用 `Engine.FailWait(ctx, key, version, cause)`，只让该成员失败，健康成员继续等待。对已失败、已取消或已完成的成员重复上报，返回 false。
- 所有候选都失败：等待组进入 FAILED，保存失败列表并登记 WAKE；恢复时 Drive 以 `Failed` + `ErrWaitFailed` 终止，WaitAny 之后的编排代码不会执行。需要降级时，让提供方用 Signal 回答一个类型化的失败值（例如 `Reply{Err: "down"}`）使其成为赢家，或在组里加一个 Sleep 候选兜底。
- key 非法或重复、空组、Case 误用、存储事务失败或父 context 取消：作为整个操作的框架错误，中止本次 Drive，不会伪装成某个候选的业务失败。
- 赢家的响应按该 Case 自己的 Response 类型解码；解码失败属于回调协议错误，是终态（`Failed`），不会当作其他候选完成。Sleep 获胜没有响应，用 `winner == timeout` 区分即可，不需要伪造 JSON。业务失败应通过 FailWait 上报，不要当作成功响应提交。
- 同组的 key、顺序、Kind 和每个成员的类型（`WaitRequest.Type`，如 `any|signal[int,string]`）必须保持稳定，变更会返回 ErrChanged。请求内容、Sleep 时长/截止时间以首次登记为准，后续变化不影响回放。重驱或重启后回放的是同一个赢家，不会重新登记败者。
- 等待完成后，库内不会残留监听句柄或 goroutine。记录、等待组以及 CANCEL/WAKE 消息会保留，用于回放和可靠投递；消息由消费者 ACK，历史记录按实例的保留策略清理。

## 存储扩展与兼容

基础 Infrastructure 没有新增必选方法，新增的是可选接口 `WaitAnyInfrastructure`：

```go
WaitAny(context.Context, groupKey string, requests []WaitRequest) (*WaitAnyState, error)
FailWait(context.Context, memberKey string, version int64, message string) (bool, error)
```

自定义存储必须持久化组归属、请求、失败列表和赢家，同时升级原有的 Resolve：完成组成员时，要在同一原子操作中选出赢家、取消败者并登记消息。只实现这两个新方法而不升级 Resolve，不满足契约。`workflow/memory` 提供了完整的参考实现。

WaitAny 是可选能力，因此由运行时类型断言检测；在不支持的存储上调用会以 `Failed` + `ErrWaitAnyUnsupported` 终止（确定性的配置错误，重试无效）。之所以不放进 Infrastructure 必选接口，是为了让只用 WaitFor/Action 的适配器无需实现组语义。

`WaitRequest.Error` 非空表示该候选编码失败。存储仍须按原顺序持久化它的 Key、Kind、Duration、Until 和失败状态，但不为它登记 REQUEST 或定时 WAKE，并把它计入 Failures。所有候选都失败时，保存 FAILED 组并登记组 WAKE。存储必须先校验完整的候选集合，包括失败候选的种类、key 唯一性和归属。回放时，存储适配器必须对每个有序成员调用 `workflow.SameWaitMember(stored, request)`：已存储成员带 `Type` 时比较 Key + Kind + Type；没有 `Type` 的旧登记退回完整比较（请求、Duration、Until 以及 Error 是否为空），错误消息文本本身不参与身份比较。适配器须持久化 `WaitRequest.Type` 并写入成员 Record。自定义 WaitAny 适配器必须同步支持这些字段，不能把失败候选过滤掉。

如果旧实现登记的组省略过编码失败的候选，升级后完整定义会返回 ErrChanged。部署前需要处理这些在途实例，或保留对应的历史执行版本，不能悄悄改变等待组。

旧适配器以及普通的 WaitFor/Action 接入不受影响。CancelNotification 只由 WaitAny 产生，接入这项能力的投递消费方需要处理 CANCEL。普通 WaitFor 原有的 REQUEST/WAKE 规则不变。

WaitAnyState 是适配器使用的快照，包含 Status、WinnerKey、WinnerKind、Response、Pending 和 Failures，所有返回的快照都必须是深拷贝。等待历史只能由同一实例作用域下的基础设施访问。支持混合信号的适配器需要保存 Kind、Duration、Until，并实现定时触发语义；已有接口签名不变。

### 从上一版 WaitAny 迁移（源码不兼容，存储格式不变）

| 上一版 | 当前 |
| --- | --- |
| `f.WaitAny[string]("g", WaitForSignal("a", req), SleepSignal("t", d))` | `a := OnSignal[string]("a", req)`；`f.WaitAny("g", a, OnSleep("t", d))` |
| `WaitCase[T]{Key, Request}` / `Signal{Kind: ...}` | `OnSignal[Response](key, req)` / `OnKind[Response](kind, key, req)` |
| `SleepUntilSignal(key, t)` | `OnSleepUntil(key, t)` |
| 所有候选共用响应类型 `R` | 每个 Case 有自己的 Response 类型 |
| `result.Key` / `result.Kind` / `result.Response` | `winner.Key()` / `winner.Kind()` / `c.Value()`（`winner == c` 判定） |
| `result.Failures` | 各 Case 的 `Failure()` |

存储层的 WaitRequest、WaitAnyState、key 拓扑（`group|member`）完全不变，在途等待组可以继续回放。
