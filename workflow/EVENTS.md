# 对外可观测事件

通过 `Engine.Subscribe(listener, types...)` 订阅，不需要替换现有 Observer。省略 types 订阅全部类型，指定 types 只接收对应事件；返回一个可重复调用的退订函数。

```go
unsubscribe := engine.Subscribe(func(ctx context.Context, event workflow.Event) {
    // logger 应线程安全；回调应快速返回。
    logger.Info("workflow event",
        "event_id", event.ID,
        "execution_id", event.ExecutionID,
        "type", event.Type,
        "key", event.Key,
        "status", event.Status,
        "duration", event.Duration,
        "error", event.Error,
    )
}, workflow.DriveFinished, workflow.ActionFinished, workflow.WaitResolved)
defer unsubscribe()
```

完整可执行示例见 `events_example_test.go`。业务、监控和埋点系统可以分别注册独立监听者。

## 触发时机与条件

| 类型 | 时机、条件 | 数量保证 |
| --- | --- | --- |
| DriveStarted | 进入 Drive，参数及定义校验前 | 每次调用一次；非法定义、序列化失败、取消、存储失败也可观察 |
| DriveFinished | Drive 即将返回，正常流程已汇合所有分支；校验拒绝也触发 | 每次调用一次，与同 ExecutionID 的开始事件对应 |
| ActionStarted | 租约内读取/创建 RUNNING 成功，调用 Action 之前 | 实际 Action 调用一次；回放、租约冲突和初始化失败不触发 |
| ActionFinished | 一个已解析 key 的 Action 执行请求返回，含结果提交和租约释放之后 | 每个该请求一次；包含执行、回放、初始化错误；非法/重复 key 在 claim 阶段被拒绝时没有 Action 事件，由 DriveFinished.Error 表达 |
| ActionReplayed | 读取到 COMPLETED 或 FAILED 的持久化记录，使用该记录回放 | 每次回放一次；解码失败也带 Error；不同时触发 ActionStarted |
| WaitSuspended | WaitFor/Sleep 成功取得持久化等待记录，判断仍需等待 | 每次经过未解决等待点一次；同一 key 不同 Drive 属于不同观察事件 |
| WaitResolved | ResolveSignal 或 Sleep 到期时，存储 Resolve 返回 true 且无错误 | 每次实际接受的 Resolve 一次；重复/陈旧/失败回调和已完成 Sleep 回放不触发 |

ActionReplayed 与 ActionFinished 表达不同阶段，不是重复执行事件。Loop 每个 attempt 是独立 Action；Go/GoFlow 分支采用相同触发规则。Flow 挂载、检查点和租约续租不单独发事件，本次接口范围是驱动、Action 及等待生命周期。

每次 Drive 有独立 ExecutionID；重驱同一个实例会生成新的 ExecutionID。外部 ResolveSignal 不属于一个 Drive，ExecutionID/WorkflowName/WorkflowVersion 为空，监听方按引擎与实例范围内的 Key 关联。

## 完整数据结构

结构定义在 `events.go`，SchemaVersion 当前为 1。事件可直接 JSON 编解码。

| 字段 | 含义 |
| --- | --- |
| SchemaVersion | 事件结构版本 |
| ID | EngineID 与递增 Sequence 组成的唯一事件标识 |
| EngineID | 一个 Engine 对象的标识；重建 Engine 会变化 |
| Sequence | 同一个 Engine 的事件生成序号；跨并发分支回调的到达顺序可能不同 |
| ExecutionID | 一次 Drive 的关联标识，所有子流程/Action 继承；外部 Resolve 为空 |
| WorkflowName / WorkflowVersion | 当前 Drive 的定义名称和版本 |
| Type / Key | 事件类型与展开后的执行点 key；Drive 的 Key 是根定义名称 |
| Kind | Action、Signal 或 Sleep；Drive 和外部 ResolveSignal 为空。原 Resolve 接口不返回记录类型，避免额外读取或推断错误类型 |
| Status | Drive 的执行结果；ActionFinished 是已观察或成功提交的记录状态；等待为 SUSPENDED/COMPLETED |
| At / StartedAt | 事件发生时间与对应生命周期的开始时间 |
| Duration | `At - StartedAt`，JSON 单位纳秒，时钟回拨时下限为 0 |
| Replayed | 是否使用持久化 Action 结果 |
| RecordVersion | 已观察或成功提交的记录版本；尚未有记录时为 0 |
| Pending | 等待 key、kind 和 until；DriveFinished 含本次全部挂起点 |
| Error | 可传输的 Category、Message、Operation，不包含原始 Go error |

Drive Duration 包括参数校验、初始化和执行。Action Duration 包括该请求的回放检查、租约等待/操作、执行、提交和清理。等待事件的 StartedAt 来自等待记录创建时间；外部 ResolveSignal 不读取记录，StartedAt=At，Duration=0。

Action 成功提交后 Release 失败：ActionFinished.Status 仍为 COMPLETED，同时 Error 记录 Release 故障；Drive 返回可重试错误。记录状态与本次调用错误是两个不同维度。

Error.Category 包括 execution、panic、conflict、cancelled、timeout、infrastructure、lease、replay、permanent、invalid、changed。错误合并时按该实现中的优先级选择类别，Message 保留完整合并文本，Operation 尽可能保留存储/租约操作名。

事件不包含业务 input/output 或凭证字段。应用的 error 文本仍可能包含敏感信息，业务应避免将凭证写入错误；监控接入也应遵循自身脱敏规则。

## 订阅与隔离语义

- 同步通知：一次事件在 API 返回前通知当前快照中的全部匹配监听者，不使用可能丢消息的有界异步通道。
- 并发分支可能同时通知，监听者必须线程安全。Sequence 是生成顺序，不承诺不同 goroutine 的到达顺序。
- 每个监听者收到独立的 Pending/Error 副本，修改不会影响其他监听者或执行结果。
- 单个监听者 panic 被隔离，后续监听者继续通知；旧 Observer 的 panic 也独立隔离。
- 不持订阅锁调用监听者，可以在回调中订阅或退订。
- 退订幂等；已经取得订阅快照的通知可以在退订后完成。新注册者不接收已取得快照的事件，也不接收历史事件。
- 同一个函数注册两次视为两个订阅，会分别通知。监听者可以用 ID 去重自己的上报。
- context 保留业务 trace 等 values；失败场景中 context 可能已经取消，监听方不能仅因取消而跳过诊断记录。
- 慢监听者会增加执行耗时。需异步上报时，监听者自行持久化交接或写入自身队列；该队列的重试/去重由接入方负责。

这是进程内可观测接口，不是持久化事件交付协议。进程崩溃或存储已提交但客户端超时，仍可能导致完成事件未送达；外部 exactly-once 不在承诺范围内。业务可靠调度继续使用已有 REQUEST/WAKE outbox，而不依赖观察事件。

## 向后兼容

Options.Observer、Observer、Observation 的公开签名与原有 phase 保持不变；已有接入方不需要改代码。没有订阅时原功能照常执行。新事件通过新增 Subscribe 接口独立发布，不向旧 Observer 增加 Sleep/Resolve 等新阶段。

SchemaVersion=1 的字段与 EventType 常量是公开契约。未来新增字段或类型应保持已有字段语义，监听者应允许未知事件类型。旧版 workflow/legacy 的 API 不受本次改动影响。
