# 任意完成的多路等待

`FlowContext.WaitAny[Response](groupKey, signals...)` 将多个独立 Signal 同时登记为一个等待组。WaitFor 和 Sleep 是两类信号：分别由外部响应和到期时间触发。第一个完成信号确定赢家，在同一个原子操作中取消其他信号。它不等待其他响应，不为候选创建常驻 goroutine。

## 外部响应或超时

```go
winner, err := flowCtx.WaitAny[string]("reply_or_timeout",
    workflow.WaitForSignal("reply", request),
    workflow.SleepSignal("timeout", 30*time.Second),
)
if err != nil {
    return "", err
}
if winner.Kind == workflow.SleepKind {
    return "timeout", nil
}
return winner.Response, nil
```

`WaitForSignal`、`SleepSignal`、`SleepUntilSignal` 创建信号描述，不立即执行等待。`ctx.WaitFor`、`ctx.Sleep`、`ctx.SleepUntil` 执行单信号等待，`ctx.WaitAny` 执行组合等待。混合等待的完整可执行例子见 `signals_example_test.go`。

`Signal` 是 `WaitCase[any]` 的统一描述类型，包含 Key、Kind、Request、Duration、Until。旧 `WaitCase[T]{Key, Request}` 仍然表示 WaitFor；零 Kind 保持这个兼容含义。WaitRequest 在存储边界携带同样的种类和时间配置，Pending 与赢家结果保留真实 Kind。

相对 Sleep 的截止时间只在首次登记时由存储计算，重驱不会推迟它；绝对 Sleep 保存指定 Until。存储推进时优先结算已到期定时信号，多个定时信号到期时按最早 Until 选择，同截止时间按声明顺序选择。外部回调在定时截止后到达不能抢占超时结果。计时信号登记一个按 Until 可见的 WAKE，由外部调度触发 Drive，不需要常驻计时 goroutine。

可以定义自定义 Kind 的 Signal 描述，由自定义存储实现其登记、触发、解析和取消规则。MemoryInfrastructure 当前仅实现 WaitFor/SignalKind 与 SleepKind，未知种类明确返回 ErrSignalUnsupported，不会悄悄按 WaitFor 处理。

## 两个外部信号

```go
winner, err := flowCtx.WaitAny[string]("approvals",
    workflow.WaitCase[string]{Key: "primary", Request: "order_123"},
    workflow.WaitCase[string]{Key: "secondary", Request: "order_123"},
)
if err != nil {
    return "", err
}
// 只需一个响应即可继续；winner.Key 是展开后的赢家 key。
return winner.Response, nil
```

完整双等待及外部监听清理示例：

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

还提供 `wait_any_example_test.go`，可运行：

```sh
go test -race ./workflow -run '^(TestFlowContextWaitAny|ExampleFlowContext_WaitAny)$' -v
```

## 执行机制

1. Flow 一次调用 WaitAny，校验所有局部 key 并编码请求。
2. 存储原子登记全部候选和等待组归属：WaitFor 登记 REQUEST，Sleep 登记按 Until 可见的 WAKE。未到期的有效候选同时处于 SUSPENDED；Drive 返回所有 Pending。已经到期的 Sleep 可直接完成并取消其他信号。
3. 外部业务方独立处理 REQUEST，对自己的 key 调用 `ResolveSignal(ctx, key, version, response)`。
4. 第一笔成功 Resolve 原子保存赢家、取消剩余 SUSPENDED 成员、删除过时请求通知，并登记赢家 WAKE 与败者 CANCEL 通知。
5. 外部调度方消费 WAKE 后立即 Drive；WaitAny 返回已持久化的赢家，后续逻辑继续。无需等败者完成。
6. CANCEL 消费方按 key 取消外部 context、注销监听句柄并确认消息；取消前后的迟到回调返回 false，不改变赢家。

沿用当前库的“持久化挂起 → 回调 → 重新 Drive”模型，不是把同一个 Go 调用一直阻塞在内存中。WAKE 消费者应及时驱动流程；内核没有自动后台调度线程。并发完成时由存储原子提交顺序决定赢家，不依赖客户端时间戳。

外部 IO 的取消是协作式操作：内核在 Resolve 时立即终止等待状态，已经投递出去的外部任务由 CANCEL 消费方释放资源。示例用 context.CancelFunc 和 WaitGroup 示范清理。无法跨进程直接强制杀死业务方的线程。

## 异常与资源

- 单个请求无法 JSON 编码或其 MarshalJSON panic：记录到返回值 Failures，继续登记其他有效候选。
- 外部候选执行失败：调用 `Engine.FailWait(ctx, key, version, cause)`，只失败该成员，健康成员继续等待。已失败/取消/完成成员的重复报告返回 false。
- 所有候选失败：等待组进入 FAILED，保存失败列表并登记 WAKE；恢复时返回 PermanentError。
- key 非法、重复、空组、存储事务失败或父 context 取消：作为整个操作的错误返回。配置和基础设施故障不能伪装成某个候选的业务失败。
- 有数据响应的信号共享 Response 类型，成功回调必须符合这个 JSON 类型；Sleep 获胜时 Response 为零值，应通过 Kind 区分，不伪造 JSON 业务响应。已选择赢家后的响应解码错误属于回调协议错误，不能假装是另一个任务完成。业务失败应上报 FailWait，不应作为成功响应提交。
- 同组 key、顺序与请求内容必须稳定；变更返回 ErrChanged。重驱/重启回放同一个赢家，不重新登记败者。
- 等待完成后无库内监听句柄或 goroutine。记录、等待组和 CANCEL/WAKE 消息保留用于回放与可靠投递；消息由消费者 ACK，历史记录按实例保留策略清理。

## 存储扩展与兼容

基础 Infrastructure 未增加必选方法。新增可选 `WaitAnyInfrastructure`：

```go
WaitAny(context.Context, groupKey string, requests []WaitRequest) (*WaitAnyState, error)
FailWait(context.Context, memberKey string, version int64, message string) (bool, error)
```

自定义存储必须持久化组归属、请求、失败列表和赢家，并升级原有 Resolve：对组成员的完成要原子选择赢家、取消败者和登记消息。仅实现两个新方法而不升级 Resolve 不满足契约。MemoryInfrastructure 提供完整参考实现。

`WaitRequest.Error` 非空表示候选编码失败，仍须按原顺序持久化该候选的
Key、Kind、Duration、Until 和失败状态；不为它登记 REQUEST/定时 WAKE，
并将它计入 Failures。全部候选失败时保存 FAILED 组并登记组 WAKE。
存储必须先验证完整候选集合，包括失败候选的种类、唯一 key 与归属。
回放时比较完整有序定义以及 Error 是否为空，错误消息文本不参与身份比较。
自定义 WaitAny 适配器必须同步支持该字段，不能继续过滤失败候选。

旧实现已登记的组若曾省略编码失败候选，升级后完整定义会返回 ErrChanged；
部署前需处理这些在途实例或保留对应历史执行版本，不能静默改变等待组。

旧适配器和普通 WaitFor/Action 接入不受影响；在旧适配器上调用 WaitAny 返回 ErrWaitAnyUnsupported。CancelNotification 只由新增 WaitAny 产生；接入这项能力的投递消费方需处理 CANCEL。普通 WaitFor 的既有 REQUEST/WAKE 规则保持不变。

WaitAnyResult 包含赢家 Key、Kind、Response 和可选 Failures。WaitAnyState 是适配器使用的快照，包含 Status、WinnerKey、WinnerKind、Response、Pending 和 Failures。所有返回快照必须深拷贝。等待历史只能由同一实例作用域下的基础设施访问。支持混合信号的适配器需保存 Kind、Duration、Until，并实现定时触发语义；既有接口签名保持不变。
