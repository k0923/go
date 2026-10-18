# Cflow 流程开发规范

本规范基于 `github.com/k0923/go/cflow` 包当前代码编写，适用于使用该包开发可重放、可恢复、可并发调度的业务流程。

## 核心模型

`cflow` 是一个基于事件溯源和租约的轻量流程运行时：

- 流程代码每次执行都会从入口重新跑一遍。
- 已完成的 Action、Loop、Checkpoint 会从事件中回放结果，不再重复执行副作用。
- 挂起点通过 `WaitForSignalEvent` 或 `SleepSignalEvent` 持久化，外部系统完成信号后再次驱动 `RunFlow`。
- 并发执行通过事件版本 CAS 和租约 Generation fencing 收敛。
- 根流程和子流程本身不申请流程级租约；真正持有租约的是 `Run`、`Go` 和 `Loop` 这些执行单元。

运行时不维护流程级 `State`。流程是否完成、失败、挂起，主要由事件状态决定。

## 上下文与能力边界

运行时有三类上下文：

| 上下文 | 角色 | 能力 |
| --- | --- | --- |
| `WorkflowContext` | 根流程上下文 | 继承 `FlowContext`，可运行 Action、Go、Loop、子 Flow |
| `FlowContext` | 流程/子流程上下文 | `Run`、`Go`、`Loop`，并继承 `ActionContext` 的等待与检查点能力 |
| `ActionContext` | Action 内上下文 | `WaitFor`、`Sleep`、`SleepUntil`、`Now`、`UUID`、`GetKey` |

约束：

- `ActionContext` 不持有租约；租约管理在 `FlowContext.Run`、`Go`、`Loop` 内完成。
- `Go` 是异步版 `Run`，会复用 `Run` 的事件执行逻辑。
- 子流程通过 `WorkflowContext.RunFlow` 启动，只做 key 命名空间隔离，不申请父/子流程级租约。
- Action 内不要再直接编排子 Flow；Action 只表达一个原子业务步骤。

## 快速示例

```go
package order

import (
	"context"
	"encoding/json"
	"time"

	"github.com/k0923/go/cflow"
)

type ActionFunc[Input any, Output any] func(*cflow.ActionContext, Input) Output

func (f ActionFunc[Input, Output]) Run(ctx *cflow.ActionContext, input Input) Output {
	return f(ctx, input)
}

type ApprovalOutput struct {
	Value string
}

func RunOrder(ctx context.Context, infra cflow.Infrastructure, orderID string) string {
	ctx = cflow.WithInfrastructure(ctx, infra)

	return cflow.RunFlow(ctx, func(ctx *cflow.WorkflowContext) string {
		prepared := ctx.Run(
			"prepare_order",
			ActionFunc[string, string](func(actionCtx *cflow.ActionContext, input string) string {
				createdAt := actionCtx.Now("created_at")
				return input + "|" + createdAt.Format(time.RFC3339Nano)
			}),
			orderID,
			cflow.ActionRunOption{},
		)

		approval := ctx.Run(
			"approval",
			ActionFunc[string, ApprovalOutput](func(actionCtx *cflow.ActionContext, input string) ApprovalOutput {
				resp := actionCtx.WaitFor[string, string](
					"callback",
					cflow.WaitForOption[string]{Request: input},
				)
				return ApprovalOutput{Value: resp}
			}),
			prepared,
			cflow.ActionRunOption{},
		)

		return approval.Value
	}, struct{}{})
}

func CompleteApproval(ctx context.Context, infra cflow.Infrastructure, key string, response string) error {
	event, err := infra.GetEvent(ctx, key)
	if err != nil {
		return err
	}
	wait := event.(*cflow.WaitForSignalEvent)
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	wait.Response = raw
	wait.SetStatus(cflow.FlowStatusCompleted)
	_, err = infra.UpdateEvent(ctx, wait)
	return err
}
```

说明：

- `RunFlow` 的第三个参数当前不会注入到 `WorkflowContext`，业务入参应显式传给 `Run`、`Go`、`Loop`。不要依赖闭包捕获可变业务状态传递流程数据。
- 外部信号完成后没有单独的 `ResumeRun` API；应更新对应信号事件，然后再次调用同一个 `RunFlow`。
- 不注入 `Infrastructure` 时会默认使用 `DebugInfrastructure`，仅适合调试和测试。

## Flow / Action 实现规范

`Flow` 和 `Action` 接口用于定义稳定的执行边界，不只是为了适配 Go 语法。生产流程应优先使用具名类型实现接口，避免把业务步骤写成大量匿名闭包。

推荐：

- 每个业务步骤使用具名 `Action` 类型表达，例如 `SubmitTaskAction`、`PollTaskAction`。
- `Action` 可以持有无状态依赖，例如 client、logger、静态配置和编解码器。
- `Action` 不应持有本次执行的可变业务状态；跨步骤传值只能通过 input/output、事件和信号完成。
- `Flow` 负责声明编排关系，`Action` 负责一个原子副作用或纯计算步骤。
- AI 生成生产流程代码时，应优先生成具名 `Flow` / `Action` 类型，而不是匿名闭包。

不推荐：

- 在 `RunFlow` handler 或 `ActionFunc` 闭包中捕获并修改外部变量。
- 用包级变量、内存 map、闭包局部变量保存流程进度。
- 让多个 `Action` 通过共享对象字段传递本次流程状态。
- 在闭包里隐藏外部依赖，导致测试、回放和审计时无法看清输入输出边界。

闭包适合测试、文档示例或一次性调试 helper。即使使用闭包，也只能捕获只读依赖，不能捕获会影响流程分支或副作用结果的可变状态。

具名 `Action` 示例：

```go
type SubmitTaskAction struct {
	Client MediaKitClient
}

func (action SubmitTaskAction) Run(ctx *cflow.ActionContext, input SubmitTaskInput) SubmitTaskOutput {
	output, err := action.Client.Submit(ctx, input)
	if err != nil {
		panic(err)
	}
	return output
}
```

## Run 使用边界

`Run` 会写入事件、申请租约，并在恢复时读取已完成结果。它适合保护外部副作用和需要持久化的昂贵步骤，但不应被当成普通函数调用的替代品。

应该使用 `Run` / `Go`：

- HTTP、DB 写入、发消息、提交任务等外部副作用。
- 昂贵且希望完成后直接回放的计算。
- 需要失败持久化、租约互斥或跨 worker 防重复执行的步骤。

不应该使用 `Run`：

- 构造请求体、拼接 URL、字段映射、状态归一化等轻量纯函数。
- 只依赖当前 input 和已完成 output，且每次重算成本很低的确定性逻辑。
- 不产生外部副作用，也不需要独立事件状态的中间变量。

这类轻量纯函数应直接在 `Flow.Run` 中调用。流程恢复时重复计算即可，成本通常远低于额外事件读写和租约开销。

```go
request := buildSubmitRequest(input)

submit := flowCtx.Run(
	"submit_task",
	SubmitTaskAction{Client: client},
	SubmitTaskInput{Request: request},
	cflow.ActionRunOption{},
)

taskURL := buildTaskURL(submit.TaskID)
```

## Key 规范

Key 是事件的持久化身份，必须稳定。

规则：

- 同一个执行点必须使用固定 key。
- 同一个 Workflow 实例内，所有最终展开后的 key 必须全局唯一。这里的执行点包括 Workflow/Flow 挂载点、`Run`、`Go`、`Loop`、`WaitFor`、`Sleep`、`Now`、`UUID`。
- 用户自定义 key 只能使用英文字母、数字和下划线，建议匹配 `^[A-Za-z0-9_]+$`。不要使用短横线、空格、点号、斜杠、冒号、中文或其他特殊字符。
- 用户自定义 key 绝对不能包含 `|`。`|` 是运行时内部命名空间分隔符，只能由框架在最终 key 中生成。
- key 上线后不要改名。改名等同于创建新事件，旧副作用可能无法回放。
- 不同事件类型不能复用同一个最终 key。否则可能错误回放，或在事件类型转换时触发系统错误。
- 推荐使用简短业务语义，如 `prepare_order`、`approval`、`callback`。
- `Flow` / `Action` 可以定义局部 key；复用时由挂载 key 形成实例命名空间：
  - 根上 `ctx.Run("approval", ...)` 的事件 key 是 `approval`。
  - 在 `approval` Action 内 `actionCtx.WaitFor("callback", ...)` 的事件 key 是 `approval|callback`。
  - 子流程 `ctx.RunFlow("child", ...)` 内的 `flowCtx.Run("step", ...)` 是 `child|step`。
  - Loop 第 N 次尝试的 ActionContext key 是 `{loopKey}|{attempt}`，从 0 开始。
  - Loop sleep key 是 `{loopKey}|sleep|{attempt}`，从 1 开始。

正确复用示例：

```go
ctx.RunFlow("front_video", enhanceFlow, frontInput, cflow.FlowRunOption{})
ctx.RunFlow("back_video", enhanceFlow, backInput, cflow.FlowRunOption{})
```

如果 `enhanceFlow` 内部都使用 `submit`、`poll` 这些局部 key，最终 key 会展开为：

```text
front_video|submit
front_video|poll
back_video|submit
back_video|poll
```

错误示例：

```go
ctx.RunFlow("video", enhanceFlow, frontInput, cflow.FlowRunOption{})
ctx.RunFlow("video", enhanceFlow, backInput, cflow.FlowRunOption{})
```

这两个调用会落到同一个实例命名空间，第二个调用会复用第一个调用的事件，不会被视为两个独立流程实例。

同一 Workflow 实例内也不要写出会展开成同一个最终 key 的路径：

```go
ctx.Run("approval", actionA, input, cflow.ActionRunOption{})
ctx.Loop("approval", actionB, input, cflow.LoopRunOption{})
```

上面两个执行点最终 key 都是 `approval`，但事件类型不同，会造成冲突。

不要手写和运行时内部格式冲突的 key，尤其是包含 `|` 或 `|sleep|` 的 key。`|` 当前是运行时命名空间分隔符，不应出现在用户自定义 key 中。

## 重放确定性

流程代码会被重复执行，因此开发时必须保证重放确定性。

必须遵守：

- 所有外部副作用只能放在 `Action.Run` 内，并通过 `FlowContext.Run` 或 `Go` 调用。
- 需要当前时间时使用 `actionCtx.Now(key)`。
- 需要 UUID 时使用 `actionCtx.UUID(key)`。
- Action 输入、输出、WaitFor Request/Response、Loop 输出必须能被 `encoding/json` 序列化。
- 长耗时 IO 必须监听 `actionCtx.Done()` 或使用 `actionCtx` 作为 context，以便租约续租失败时及时退出。
- Action 业务失败可以 `panic(error)`；运行时会将非 `ContextError` 包装为 `BizError` 并持久化失败事件。

禁止：

- 在 `RunFlow` handler 顶层直接执行 HTTP、DB 写入、发消息等副作用。
- 在 Action 外直接调用 `time.Now()`、随机数、UUID 并影响流程分支。
- 依赖包级变量、闭包可变变量、内存缓存保存流程进度。
- 在业务代码中 `recover` 运行时 panic。运行时使用 panic 表达挂起信号、冲突和错误传播。
- 启动 `Go` 后不调用 `Wait()`。`Go` 是异步执行，`Wait()` 是汇合点。

## 编排原语

### Run

`Run` 表示一个原子 Action：

```go
output := flowCtx.Run("step", action, input, cflow.ActionRunOption{})
```

首次执行时，运行时会：

1. 申请 key 对应的租约。
2. 写入 `StepResultEvent`，状态为 `RUNNING`。
3. 执行 Action。
4. 成功则写入 Result 并置为 `COMPLETED`。
5. 失败则写入 Error 并置为 `FAILED`。
6. 遇到 WaitFor/Sleep 信号则置为 `SUSPENDED`，释放租约并向上抛出信号。

重放时：

- `COMPLETED`：直接返回持久化 Result。
- `FAILED`：抛出 `LegacyBizError`，Action 不再执行。
- `RUNNING` / `SUSPENDED`：尝试重新申请租约继续执行。

### Go

`Go` 是异步版 `Run`：

```go
result := flowCtx.Go("async", action, input)
// 可以继续做其他编排
value := result.Wait()
```

规则：

- `Go` 使用 `StepResultEvent`，不引入额外的 AsyncEvent。
- 已完成结果会直接回放，Action 不会重复执行。
- 多个 `Go` 分支可以并行，但每个分支 key 必须唯一。
- 分支内如果挂起，`Wait()` 会重新抛出信号，当前驱动结束；外部完成信号后再次运行 `RunFlow`。

### RunFlow

根入口：

```go
output := cflow.RunFlow(ctx, handler, input)
```

子流程：

```go
result := ctx.RunFlow("child", childFlow, input, cflow.FlowRunOption{})
value := result.Wait()
```

规则：

- 根 `RunFlow` 创建 key 为空的 `WorkflowContext`。
- 子流程以 `ctx.apiKey(key)` 作为命名空间前缀。
- 流程本身不申请租约，避免父流程租约阻塞子流程；租约只在子流程内部的 Action/Loop 上发生。

### WaitFor

`WaitFor` 用于等待外部信号：

```go
resp := actionCtx.WaitFor[string, string](
	"callback",
	cflow.WaitForOption[string]{Request: "review"},
)
```

首次执行时会写入 `WaitForSignalEvent`：

- key 为当前 Action key 拼接子 key，例如 `approval|callback`。
- `Request` 会被 JSON 持久化。
- 状态为 `SUSPENDED`，随后以信号 panic 结束当前驱动。

外部系统完成后，应：

1. 读取该 `WaitForSignalEvent`。
2. 填充 `Response`。
3. 将状态改为 `COMPLETED`。
4. 使用 `UpdateEvent` 做版本 CAS 更新。
5. 再次调用同一个 `RunFlow`。

### Sleep / SleepUntil

`Sleep` 和 `SleepUntil` 用于定时挂起：

```go
actionCtx.Sleep("backoff", 30*time.Second)
actionCtx.SleepUntil("deadline", deadline)
```

规则：

- 首次执行写入 `SleepSignalEvent`，状态为 `SUSPENDED`。
- 未到期时会抛出信号，当前驱动结束。
- 到期后再次运行 `RunFlow`，运行时会将 sleep 事件置为 `COMPLETED` 并继续执行。
- `duration <= 0` 或 `until` 已经过期时会立即完成。

### Now / UUID

`Now` 和 `UUID` 都是 Checkpoint：

```go
createdAt := actionCtx.Now("created_at")
id := actionCtx.UUID("id")
```

首次调用写入 `CheckpointEvent`，重放时返回第一次生成的值。不要用 `time.Now()` 或 `uuid.NewString()` 直接参与流程决策。

### Loop

`Loop` 用于有限次数轮询：

```go
type PollOutput struct {
	Value string
	DoneValue bool
}

func (o PollOutput) Done() bool { return o.DoneValue }

out := flowCtx.Loop(
	"poll",
	pollAction,
	input,
	cflow.LoopOption{Interval: 5 * time.Second, MaxAttempts: 3},
)
```

规则：

- `LoopOutput` 必须实现 `Done() bool`。
- `Interval` 不能小于 5 秒。
- `MaxAttempts` 必须大于 0。
- attempt 从 0 开始，ActionContext key 为 `{loopKey}|{attempt}`。
- 当 `Done()` 为 false 时，Loop 事件进入 `SUSPENDED`，并创建 `{loopKey}|sleep|{attempt}` 的 sleep 事件。
- 超过最大次数会将 Loop 事件置为 `FAILED`，错误为 `loop max attempts exceeded: N`。

## 事件模型

当前支持的事件类型：

| 事件 | 来源 | 类型标识 | 负载 |
| --- | --- | --- | --- |
| `StepResultEvent` | `Run` / `Go` | `STEP_RESULT` | Result |
| `WaitForSignalEvent` | `WaitFor` | `WAIT_FOR_SIGNAL` | Request / Response |
| `CheckpointEvent` | `Now` / `UUID` | `CHECKPOINT` | Data |
| `SleepSignalEvent` | `Sleep` / `SleepUntil` / Loop 间隔 | `SLEEP` | Until |
| `LoopEvent` | `Loop` | `LOOP` | Result / Attempt |

版本规则：

- `AddEvent` 成功后版本置为 1。
- `UpdateEvent` 必须基于当前版本 CAS，成功后版本 +1。
- key 已存在、版本不匹配、租约冲突等并发冲突应返回 `(nil, nil)` 或 `false, nil`，不要返回 error。
- 存储故障、序列化错误、上下文取消等才返回 error。

## 租约规范

执行租约定义在 `lease.go`：

- TTL 固定为 30 秒。
- 后台续租间隔为剩余 TTL 的 1/3。
- `ClaimLease` 返回 `(nil, nil)` 表示被其他 worker 持有。
- `RenewLease` 返回 `(nil, nil)` 表示租约过期或 Generation 不匹配。
- `ReleaseLease` 返回 `false, nil` 表示租约不存在或 Generation 不匹配。

运行时行为：

- Claim 冲突会转成 `ConflictError`。
- Renew / Release 冲突会让本次执行结果作废，并以 `ConflictError` 收敛。
- Claim 过程中的基础设施错误会上报 `OnError`，并向上抛出 `InfrastructureError`。
- Renew / Release 的基础设施错误会上报 `OnError`，通常表现为当前执行被中断、等待下次驱动。

生产实现必须使用存储层原子 CAS，不能用非原子的“先读后写”模拟。

## Infrastructure 实现契约

`Infrastructure` 必须实现：

```go
type Infrastructure interface {
	LeaseInfrastructure
	AddEvent(ctx context.Context, event Event) (Event, error)
	GetEvent(ctx context.Context, key string) (Event, error)
	UpdateEvent(ctx context.Context, event Event) (Event, error)
	OnError(err error)
}
```

要求：

- `GetEvent` 不存在时返回 `(nil, nil)`。
- `AddEvent` key 已存在时返回 `(nil, nil)`。
- `UpdateEvent` key 不存在或版本不匹配时返回 `(nil, nil)`。
- 返回给调用方的事件必须是深拷贝，尤其是 `json.RawMessage`。
- 必须支持所有内置事件类型的序列化与反序列化。
- 所有方法必须响应 `ctx.Err()`。
- `OnError(nil)` 应该是 no-op；`OnError` 不应 panic，也不应长时间阻塞。
- `DebugInfrastructure` 仅用于测试和调试，不作为生产持久化方案。

## 错误分类

| 错误 | 含义 | 处理 |
| --- | --- | --- |
| `ConflictError` | 租约冲突、版本冲突、等待中的信号 | 正常并发结果，通常等待下次驱动 |
| `ContextError` | context 已取消或超时 | 停止当前执行，必要时重驱 |
| `BizError` | 首次执行业务失败 | 事件置为 FAILED，修正业务数据或新建实例 |
| `LegacyBizError` | 重放命中已失败事件 | 说明失败已持久化，不会重复执行 Action |
| `SystemError` | API 使用错误、事件类型不符、未知状态 | 修代码或修数据 |
| `InfrastructureError` | 存储/租约基础设施错误 | 由 `OnError` 观测并告警 |
| `MarshalError` / `UnMarshalError` | JSON 编解码失败 | 修正负载类型或脏数据 |
| `UnknownError` | panic 了非 error 值 | 修业务代码 |

业务代码应优先返回或 panic `error`，不要 panic 字符串或任意对象。

## 推荐调度方式

一次完整恢复链路通常是：

1. 创建或找到流程实例对应的 `Infrastructure`。
2. 调用 `RunFlow`。
3. 如果抛出 `Signal`：
   - `WaitForSignalEvent`：把 Request 暴露给外部审批/回调系统。
   - `SleepSignalEvent`：按 Until 建定时任务或由扫描器定期检查。
4. 外部条件满足后更新对应事件。
5. 再次调用同一个 `RunFlow`。
6. 如果命中已完成事件，结果会回放；如果命中已失败事件，会返回持久化失败。

多 worker 可以同时驱动同一流程实例，未抢到租约的一方会以 `ConflictError` 退出。不要把这种冲突作为业务错误告警。

## 开发检查清单

- [ ] 同一个 Workflow 实例内，每个 Workflow/Flow 挂载点、`Run`、`Go`、`Loop`、`WaitFor`、`Sleep`、`Now`、`UUID` 的最终展开 key 稳定且唯一。
- [ ] 用户自定义 key 匹配 `^[A-Za-z0-9_]+$`，不包含 `|`、短横线、空格、斜杠、冒号、中文或其他特殊字符。
- [ ] 生产流程优先使用具名 `Flow` / `Action`，闭包 helper 仅用于测试、示例或调试。
- [ ] 轻量纯函数不包 `Run`；只对外部副作用、昂贵计算或需要事件状态的步骤使用 `Run` / `Go`。
- [ ] 所有外部副作用都在 Action 内。
- [ ] Action 不通过闭包、包级变量或共享字段保存本次流程状态。
- [ ] Action 外没有会影响流程分支的非确定性调用。
- [ ] 时间和 UUID 使用 `Now` / `UUID` 检查点。
- [ ] Action 输入输出和事件负载都可 JSON 序列化。
- [ ] WaitFor 的外部完成逻辑使用 `UpdateEvent` 做版本 CAS。
- [ ] Sleep/Loop 的恢复由到期扫描或定时任务再次驱动 `RunFlow`。
- [ ] `LoopOption.Interval >= 5s` 且 `MaxAttempts > 0`。
- [ ] `Go` 返回的 `IResult` 在明确汇合点调用了 `Wait()`。
- [ ] 生产 `Infrastructure` 区分“冲突返回 nil”和“故障返回 error”。
- [ ] `OnError` 已接入日志/监控，且不会 panic。
