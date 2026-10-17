# Workflow 架构评估报告（只评估，不修改）

- 评估日期：2026-10-16
- 范围：`workflow/` 新版内核（engine / flow / future / wait_any / memory / events / types / infrastructure）及 debugger 的接口依赖；**不含 legacy**
- 方法：通读源码 + 用 `go test -overlay` 注入临时探针测试（未改动仓库任何文件）+ 现有测试全量通过确认

---

## 0. 结论速览

| 问题 | 结论 |
| --- | --- |
| 是否需要重构 | **需要做“核心语义重构”，不需要推倒重写**。存储层（Record + CAS + lease fencing）、Action 接口、WaitAny 的存储语义都可以保留；需要重做的是**控制流模型、错误语义、Run 级状态**这三块 |
| 用 panic 做阻塞是否更好 | **是，但只把“挂起（suspend）”改成 panic，业务错误继续用 error**。把所有错误都改成 panic 不推荐 |
| 最严重的问题 | ① 挂起靠 error 传递，用户吞掉后会用零值执行并**持久化**后续 Action，run 永久卡死；② 任何原语返回的错误，即使用户已经处理（如走补偿分支），也会被引擎记下并取消整个执行，**Saga/补偿无法实现**；③ 普通 error 被默认当作可重试，业务校验错误会无限重试 |

---

## 1. 探针实测结果（证据）

以下行为均通过临时测试实测确认，测试文件放在 `/tmp/wfeval`，经 `-overlay` 注入，仓库无改动。

### 1.1 吞掉挂起错误 → 零值副作用被提交 → run 永久卡死

```go
approval, _ := f.WaitFor[string]("approval", in) // 用户忽略 err
return f.Run("charge", act, approval)
```

| 步骤 | 实测 |
| --- | --- |
| Drive #1 | `status=SUSPENDED`，但 **`charge` 已执行 1 次，输入是空串 `""`，并已提交为 COMPLETED** |
| ResolveSignal("approved") 后 Drive #2 | `status=RUNNING err=workflow definition or input changed`，`charge` 的输入从 `""` 变成 `"approved"`，触发 `ErrChanged` |
| 之后 | `ErrChanged` 不属于终态错误，调度方会**永远重试、永远失败**（毒丸 run） |

根因：[remember](file:///Users/bytedance/code/personal/go/workflow/engine.go#L142-L159) 只把 `SuspendedError` 记到 pending，并**不阻止**后续原语继续执行；[claim](file:///Users/bytedance/code/personal/go/workflow/engine.go) 也不检查“本次执行是否已挂起”。

### 1.2 已处理的业务错误仍会让 run 失败（补偿不可达）

```go
if _, err := f.Run("pay", pay, in); err != nil {   // pay 返回 Permanent("card declined")
    return f.Run("compensate", refund, in)         // 想走补偿
}
```

实测：`compensate out="" err=context canceled`，最终 `status=FAILED err=card declined`。

根因：每个原语 `defer f.remember(&err)`，错误**在产生点**就进入 `execution.errs` 并调用 `e.cancel()`（[engine.go:158](file:///Users/bytedance/code/personal/go/workflow/engine.go#L158)）。引擎把“原语返回了错误”等同于“工作流失败”，而不是看**错误是否从 handler 逃逸**。这直接违背了 Go 的 error 语义——返回 error 的意义正是让调用方决定处理方式。

### 1.3 普通 error 无限重试

handler 直接 `return "", errors.New("biz invalid")`：两次 Drive 都是 `status=RUNNING`。
[terminalExecutionError](file:///Users/bytedance/code/personal/go/workflow/engine.go#L190-L208) 只认 `PermanentError` / `ReplayError`，其余都算可重试。用户忘了 `workflow.Permanent(...)` 就是无限重试，且引擎没有重试次数、退避、死信。

### 1.4 并发 Drive 同一 run，编排代码并发执行

两个 Engine 共享同一 store 并发 Drive：编排入口进入 2 次。Action 层有 lease 保护所以副作用不会重复，但编排层没有 run 级互斥，事件、Observer、WaitFor 的 Put 都会重复发生。

### 1.5 已完成的 run 再次 Drive 会完整重放

COMPLETED 后再 Drive：handler 再次执行（入口计数 2→3），返回 COMPLETED。系统**没有持久化 run 终态和输出**，只有 `$definition` 记录。调度方必须自己记 run 状态，否则每次投递都要完整重放一遍。

### 1.6 挂起时用户 defer 会执行

挂起时 Flow 函数正常返回，`defer` 会执行 1 次。这一点无论用 error 还是 panic 都一样（panic 也会执行 defer），在“重放模型”下需要写进文档：**defer 中不能有副作用**。

---

## 2. 架构问题清单

按严重度排序。P0 = 会导致数据错误或 run 永久卡死；P1 = 语义缺陷，影响正确使用；P2 = 工程或体验问题。

### P0

| # | 问题 | 位置 | 说明 |
| --- | --- | --- | --- |
| 1 | 挂起靠 error 传递，可以被吞掉 | [flow.go WaitFor](file:///Users/bytedance/code/personal/go/workflow/flow.go#L52)、[wait_any.go:216](file:///Users/bytedance/code/personal/go/workflow/wait_any.go#L216) | 见 1.1。挂起是**控制流**而不是错误，用 error 表达意味着每一处调用都得正确 `return`，漏写一处就会把零值写进持久化记录 |
| 2 | 错误在产生点就判定失败 | [engine.go remember](file:///Users/bytedance/code/personal/go/workflow/engine.go#L142-L159) | 见 1.2。Saga、降级、fallback 等常见模式全部做不了 |
| 3 | `ErrChanged` / 非确定性没有终态 | [engine.go terminalExecutionError](file:///Users/bytedance/code/personal/go/workflow/engine.go#L190) | 代码变更或吞错导致的不一致会被当作可重试，形成毒丸 run。应当是 `NonDeterministic` 终态（或进入“需要人工介入”状态） |

### P1

| # | 问题 | 说明 |
| --- | --- | --- |
| 4 | 没有 run 级状态与 run 级租约 | 见 1.4 / 1.5。缺少 `RunRecord{Status, Output, Error, Attempt}`，调度方无法判断“这个 run 是否已结束”；并发 Drive 没有互斥 |
| 5 | 错误默认可重试，没有重试策略 | `ActionOptions` 只有 `Timeout`。缺少 `MaxAttempts / Backoff / NonRetryable(errors...)`。Action 返回普通 error 时**不落库**，整个 Drive 失败后由外部重新投递，重试粒度变成“整个 run”，而不是“这一个 Action” |
| 6 | 没有版本迁移手段 | `$definition` 用 `Name+Version+Input` 的整体字节做身份，升级 Version 后所有在途 run 都会 `ErrChanged`。缺少 Temporal `GetVersion` / patch 类原语，长流程无法灰度改代码 |
| 7 | 回放靠 key 加输入字节严格相等 | [runAction](file:///Users/bytedance/code/personal/go/workflow/engine.go#L308) 用 `bytes.Equal(r.Input, data)` 判断。只要 Action 输入里带了 `time.Now()`、随机数或其他非确定值，就会触发 `ErrChanged`（叠加问题 3 后变成毒丸）。可以考虑只校验 key 加类型，输入差异降级为告警，或提供 `IgnoreInputDrift` 选项 |
| 8 | 调度契约不完整 | `Pending/Ack` 只存在于 [MemoryInfrastructure](file:///Users/bytedance/code/personal/go/workflow/memory.go#L232-L261) 和 debugger Store 上，**不在** [Infrastructure](file:///Users/bytedance/code/personal/go/workflow/infrastructure.go#L20) 接口里。库只定义“怎么执行”，没有定义“谁、在什么时机调用 Drive”。生产用户必须自己设计 dispatcher，而这恰恰是最容易出错的部分 |
| 9 | `ResolveSignal` 要求调用方先读 store 拿 version | 示例里都是 `store.Get(...)` 之后再 `ResolveSignal(key, version, ...)`。外部回调方需要直接访问存储，还要知道 `main|signals|reply` 这种拼接后的内部 key，泄露了实现细节。应当提供不带 version 的幂等 Resolve，或返回一个 opaque 的 signal token |
| 10 | Flow 内 panic 被视为可重试 | `invoke` 把 panic 转成 `PanicError`，属于可重试错误。代码 bug 导致的 panic 会被无限重放 |

### P2

| # | 问题 | 说明 |
| --- | --- | --- |
| 11 | 每次 Drive 对每个步骤做一次 `store.Get` | 重放成本为 O(步骤数) 次存储往返；Loop 这种长流程整个生命周期是 O(n²)。可以在 Drive 开始时一次性批量加载本 run 的全部 Record |
| 12 | 可观测性有两套 | `Observer`（[types.go](file:///Users/bytedance/code/personal/go/workflow/types.go)）和 `Subscribe/emit`（[events.go](file:///Users/bytedance/code/personal/go/workflow/events.go#L131)）重复，事件字段也不一致。建议只保留 Event |
| 13 | 两层 Context 带来仪式感 | 最简单的流程也必须写 `NewWorkflow` + `RunFlow("main", flow, in)` 两层，而且 key 会多一层 `main|` 前缀。编译期禁止 Flow 嵌套的收益是真实的，但默认路径应该可以一步到位 |
| 14 | 手写字符串 key 负担重 | 每个原语都要求调用方手写唯一 key，并受正则 `^[A-Za-z0-9_]+$` 约束；循环中需要 `fmt.Sprintf("x_%d", i)`。key 写错或重复要到运行时才发现 |
| 15 | WaitAny 泛型签名别扭 | `WaitAny[R, Q any]` 加 `Signal = WaitCase[any]`，调用时需要显式写出 `[string]`；能力还依赖可选接口的类型断言，到运行时才报 `ErrWaitAnyUnsupported` |
| 16 | 错误类型过多 | `Suspended / Permanent / Replay / Panic / Operation / retryable` 加多个哨兵错误，用户需要理解的分类太多；`ReplayError` 丢失了原始错误身份 |
| 17 | 时钟来源两套 | `Options.Now` 和 store 的 `now` 各自独立，Sleep 是否到期由引擎时钟判断、租约由存储时钟判断，时钟漂移时可能出现边界不一致 |

---

## 3. 关于 “用 panic 来阻塞” 的专项评估

### 3.1 先把概念拆开

工作流里“不能继续往下走”有三类，性质完全不同：

| 类别 | 例子 | 本质 | 推荐表达 |
| --- | --- | --- | --- |
| 挂起 | WaitFor 未到、Sleep 未到期、WaitAny 无赢家 | **控制流**：这次执行到此为止，下次重放再继续 | **panic（框架私有哨兵）** |
| 基础设施或系统故障 | store 超时、lease 冲突、context 取消 | 本次 Drive 作废，需要重试 | **panic（框架私有哨兵）**，用户无需处理 |
| 业务结果 | 卡被拒、库存不足、审批驳回 | **数据**：用户需要据此分支 | **error 返回值**（保持 Go 惯用法） |

当前实现把三类都塞进 `error`，所以用户必须处处 `if err != nil { return }`，而且吞错会出事（1.1）；同时引擎又在产生点把所有错误判定为失败（1.2），导致第三类“本应由用户处理的错误”也无法处理。**两个缺陷来自同一个设计决策。**

### 3.2 方案对比

**方案 A：现状（全部用 error）**

```go
func (OrderFlow) Run(f *workflow.FlowContext, id string) (string, error) {
    submitted, err := f.Run("submit", submitAction, id)
    if err != nil { return "", err }
    approval, err := f.WaitFor[string]("approval", submitted)
    if err != nil { return "", err }
    if err := f.Sleep("cool_down", time.Hour); err != nil { return "", err }
    return f.Run("ship", shipAction, approval)
}
```

**方案 B：挂起和系统故障走 panic，业务错误走 error（推荐）**

```go
func (OrderFlow) Run(f *workflow.FlowContext, id string) (string, error) {
    submitted, err := f.Run("submit", submitAction, id) // err 只可能是业务错误
    if err != nil {
        return f.Run("compensate", refundAction, id)     // 补偿分支真正可达
    }
    approval := f.WaitFor[string]("approval", submitted) // 没有 err：未到就 panic 挂起
    f.Sleep("cool_down", time.Hour)                      // 同上
    return f.Run("ship", shipAction, approval)
}
```

**方案 C：全部 panic（Must 风格）**

```go
func (OrderFlow) Run(f *workflow.FlowContext, id string) string {
    submitted := f.Run("submit", submitAction, id) // 业务失败也 panic
    ...
}
```

### 3.3 优劣分析

| 维度 | A 全 error | **B 挂起 panic + 业务 error** | C 全 panic |
| --- | --- | --- | --- |
| 样板代码 | 多 | **WaitFor/Sleep/Now/UUID 完全没有 err** | 最少 |
| 吞掉挂起的风险 | 高（已实测出事） | **结构上不可能**（配合下文的 poisoned 标记） | 不可能 |
| 业务错误可分支（补偿） | 现状不可（缺陷 2） | **可以** | 必须 `recover`，很别扭 |
| Go 惯用性 | 高 | 中：与 `net/http` 的 `ErrAbortHandler`、`testing.T.FailNow`（`runtime.Goexit`）同类做法，业界有先例 | 低 |
| 用户 `recover()` 误吞 | 无此问题 | 有风险，需要兜底 | 同左 |
| 并发（Go / Future） | 简单 | 子 goroutine 的 panic 必须被捕获并转交给 owner（现有 `invoke` 已有基础） | 同左 |
| 调试体验 | 好 | 框架需要过滤自身栈帧 | 差 |
| 与业界对照 | — | Cloudflare Workflows / Inngest / Restate TS 用 throw 挂起；Temporal Go 用阻塞 goroutine 加确定性调度；本质都是**不让用户代码看到挂起** | — |

### 3.4 方案 B 必须配套的保障

DESIGN.md 记录了当初从 panic 迁到 error 的理由：“panic 混用业务失败、存储故障、挂起和冲突”。这个问题确实存在，但它的成因是**混用**，而不是 panic 本身。重新引入 panic 时必须收窄用途并加上兜底：

1. **私有哨兵类型**：`type suspendSignal struct{ pending []Pending }` 和 `type abortSignal struct{ cause error }` 不导出，用户无法构造，也不能用 `errors.As` 拿到。
2. **execution 级 poisoned 标记**：第一次挂起或中止时设置原子标记 `ex.halted`。此后：
   - 任何原语调用（`claim`）都直接再次 panic；
   - Drive 收尾时只要 `halted=true`，就**忽略 handler 的返回值**，以 pending 或 abort 为准。

   这样即使用户 `recover()` 吞掉了 panic，也写不进任何持久化记录（彻底消除 1.1）。项目记忆里旧版 `awaiting` 原子标记就是这个思路。
3. **Action 边界隔离**：Action 内部禁止调用原语（现在已经通过类型隔离做到）。Action 自身的 panic 仍转换为 error 处理，不能当作挂起。
4. **Future / Go**：子 goroutine 中的挂起 panic 由 `async` 捕获后存入 Future，**在 `Wait()` 时于调用方 goroutine 重新 panic**；`Wait()` 不返回 err，或只返回业务 err。
5. **WaitAny**：多候选挂起时一次性收集所有 pending 后再 panic（现在的 pending 合并逻辑可以复用）。
6. **错误语义同步修正**：引擎**只看 handler 最终返回的 error** 来判定失败。原语返回的业务 error 不再自动记入 `execution.errs`，也不再 cancel。这是解决 1.2 的关键，与 panic 改造相互独立，但应一起做。
7. **文档约束**：`defer` 在挂起时会执行，不得在 defer 中产生副作用；不要在 Flow 中 `recover()` 所有 panic（或者只 re-panic 未知值）。

### 3.5 结论

- **推荐方案 B**。它同时解决了“写起来啰嗦”和“吞错导致数据损坏”两个问题，并让业务错误重新具备可处理性。
- **不推荐方案 C**。业务错误是数据，用户经常需要分支处理；全部 panic 会把补偿逻辑逼进 `recover`，比现状更差。
- 如果暂时不想改 API，最小止血措施（仍用 error 模型）是：**挂起后 poison execution，后续 claim 一律失败**。这能消除 1.1 的数据损坏，但样板代码不会减少。

---

## 4. 是否需要重构

### 4.1 保留（设计合理，不必动）

- `Action[I, O]` 接口、`AsAction`、依赖注入方式
- Record 快照加 `Commit(lease, record, expectedVersion)`：CAS 与 fencing 的组合是正确的
- 独立的 `lease` 包与 Runner（续租覆盖提交）
- REQUEST / WAKE / CANCEL 三类通知分离
- WaitAny 的存储层原子语义（赢家选择、取消失败方）
- WorkflowContext / FlowContext 的编译期分层（但需要提供简化入口）

### 4.2 需要重构（按优先级）

| 阶段 | 内容 | 解决的问题 | 影响面 |
| --- | --- | --- | --- |
| R1 控制流 | 挂起和系统中止改为私有 panic，加 poisoned 标记；WaitFor/Sleep/Now/UUID/WaitAny 去掉 err 返回值；Future.Wait 重新抛出 | P0-1，体验 | **公开 API 不兼容**，示例和测试需要同步修改 |
| R2 错误语义 | 只以 handler 逃逸的 error 判定失败；去掉原语级 `remember → cancel`；`ErrChanged` 归为终态 `NonDeterministic`；Flow 内 panic 归为终态 | P0-2、P0-3、P1-10 | 中 |
| R3 Run 状态 | 新增 `RunRecord`（状态、输出、错误、attempt）加 run 级 lease；终态后 Drive 直接返回已持久化的结果 | P1-4 | Infrastructure 接口新增方法 |
| R4 重试策略 | `ActionOptions{Retry: {MaxAttempts, Backoff, NonRetryable}}`，Action 级重试；run 级最大重试次数加死信 | P1-5 | 中 |
| R5 调度契约 | 把 `Pending/Ack` 纳入正式接口，提供参考 `Worker`（拉取通知 → Drive → Ack）；`ResolveSignal` 改为不需要 version 的幂等接口，或 opaque token | P1-8、P1-9 | 中 |
| R6 版本化 | `f.Version("change_id", min, max)` 类 patch 原语；放宽 `$definition` 对 Version 的严格相等 | P1-6、P1-7 | 中 |
| R7 性能与收敛 | Drive 时批量预加载 Record；合并 Observer 与 Event；提供 `NewWorkflowFromFlow` 单层简化入口 | P2 | 小 |

**建议实施顺序：R1 + R2 一起做**（它们是同一个设计决策的两面，一次性改掉，避免 API 改两次）→ R3 → R4/R5 → R6/R7。

### 4.3 风险提示

- R1 是破坏性 API 变更，`examples/`、debugger、视频用例都要同步修改；建议在一个独立分支一次性完成。
- R2 改变失败判定，现有“依赖原语错误自动失败”的测试会失效，需要逐个确认预期。
- R3 需要 debugger 的 SQLite Store 同步实现新接口。

---

## 5. 附：探针与验证

- 现有测试：`go test ./workflow/...` 全部通过（含 debugger）。
- 探针测试：`/tmp/wfeval/eval_test.go`、`eval2_test.go`、`eval3_test.go`，通过 `go test -overlay` 注入运行，覆盖 1.1 至 1.6 的场景，仓库源码零改动。

---

## 6. 修复状态（2026-10-17）

用户确认的定位：

1. Engine 只是**执行内核**，run 级状态由业务方实现 → R3 不做。
2. **不做版本迁移**：执行与定义版本绑定，热修复发新版本跑新 run，老 run 继续用老代码 → R6 不做；漂移改为终态 `Failed` + `ErrChanged`，便于业务方识别后路由。

| 问题 | 状态 | 实现 |
| --- | --- | --- |
| P0-1 挂起可被吞 | 已修复 | 私有 `haltSignal` panic + 每个编排路径的 `strand.halted` 标记；`claim` 先检查标记，recover 后的原语再次停止 |
| P0-2 已处理错误仍失败 | 已修复 | 删除原语级 `remember → cancel`；只有 handler 返回的 error 决定 `Failed` |
| P0-3 漂移无终态 | 已修复 | `fatalError`：ErrChanged / ErrInvalid / 解码失败 / 编排 panic → `Failed` |
| P1-5 无重试策略 | 已修复 | `ActionOptions.Retry`（指数退避，租约内重试，耗尽提交为业务失败） |
| P1-9 Resolve 需 version | 已修复 | 新增 `Engine.Signal(ctx, key, response)` |
| P1-10 Flow panic 可重试 | 已修复 | 编排 panic 归为终态 |
| P2-12 两套观测 | 已修复 | 移除 `Options.Observer`，统一 `Subscribe` |
| P2-16 错误类型过多 | 已修复 | 移除 `SuspendedError` / `ReplayError` / `retryableExecutionError`，业务失败统一为 `*ActionError` |
| P1-8 调度契约不完整 | 已修复 | 新增可选 `NotificationInfrastructure{Pending, Ack}`，接口注释写明 REQUEST/WAKE/CANCEL 的至少一次投递契约；Memory 与 debugger Client 有编译期断言；`ExampleNotificationInfrastructure` 给出可运行的调度循环。按定位不提供参考 Worker（调度归业务方） |
| P2-11 逐步 Get | 已修复 | 新增可选 `RecordLister`；Drive 开始时一次预加载终态记录（已结束 Action、已解析等待、检查点），回放零 Get/Put，类型漂移校验照旧；未实现时退回逐步读取 |
| P2-13 两层 Context 仪式感 | 已修复 | 新增 `NewWorkflowFromFlow(name, version, flow)`，单 Flow 直接作为根，key 不带 `main\|` 前缀 |
| P2-15 WaitAny 强制统一响应类型 | 已修复 | 改为非泛型 `f.WaitAny(group, cases...) Case`（见 7.4b：与 WaitFor/Sleep 一样不返回 error），Case 各自类型化：`OnSignal[R]` / `OnKind[R]` / `OnSleep` / `OnSleepUntil`，按指针身份返回赢家，`Value()` 取类型化响应；同组可混合不同响应类型；nil / 重复使用 / Kind 不匹配 → `Failed` + `ErrInvalid`。`ErrWaitAnyUnsupported` 作为可选能力保留（为 WaitAny 强制扩展 Infrastructure 会让所有存储实现都要实现分组原子操作） |
| P2-17 两套时钟 | 已修复 | 新增可选 `Clock`；`Options.Now` 为空时默认采用存储时钟 |
| P2-14 手写 key | 按设计保留 | Go/GoFlow 分支调度顺序不确定，按调用顺序自动编号会使回放错位；错误 key 在登记前以 `Failed` + `ErrInvalid` 快速失败 |
| P1-7 输入严格比较 | 已修复 | 回放身份改为 resolved key + Kind + 类型路径（`Record.Type`），输入只观测：类型路径与 key 路径逐层对齐（挂载 Flow `flow[I,O]`、Loop `loop[I,O]`、WaitAny `any`、叶子 `action[I,O]` / `signal[Req,Resp]` / `sleep` / `now[...]` / `uuid[...]`），因此 Flow 层与 Action 层 key 都不变时，任一层 I/O 类型变化仍是 `Failed` + `ErrChanged`；同类型输入变化沿用持久化结果并发出 `input_changed`；被中断的 Action 重新执行时记录改用本次输入。无 Type 的旧记录退回严格比较；WaitAny 由导出的 `SameWaitMember` 统一比较；debugger SQLite 自动加列。`$definition` 仍严格比较输入：它是 run 身份，输入不同属于路由错误而非代码演进 |
| P1-4 run 级状态 / P1-6 版本迁移 | 按定位不做 | 业务方负责 |

---

## 7. 二次评估（2026-10-17）

范围：P1-7 落地后，对 `workflow/`（不含 legacy）重新通读 engine / flow / future / action / wait_any / wait_any_memory / memory / events / types / infrastructure 以及 debugger 的 store / wire / server / client / run，并补充探针。

### 7.1 P1-7 两层 key 的处理

key 路径与类型路径逐层对齐，每多一层 key 命名空间就多一段类型：

| 定义 | resolved key | Type |
| --- | --- | --- |
| `NewWorkflowFromFlow` 内 `f.Run("charge", Action[int,int])` | `charge` | `action[int,int]` |
| `RunFlow("pay", Flow[Order,string])` 内 `f.Run("charge", Action[Order,int])` | `pay\|charge` | `flow[main.Order,string]\|action[main.Order,int]` |
| `GoFlow("pay", …)` | 与 RunFlow 相同 | 与 RunFlow 相同 |
| `Loop("poll", Action[I,O])` 第 n 次 | `poll\|attempt_n` / `poll\|sleep_n` | `loop[I,O]\|action[I,O]` / `loop[I,O]\|sleep` |
| `WaitAny("g", OnSignal[R]("a", req))` | `g\|a` | `any\|signal[Req,R]` |

- 两层 key 都没变，但挂载 Flow 的 I/O 变了：内部所有记录的 Type 前缀都会不同，返回 `Failed` + `ErrChanged`，且不会重复执行 Action（`TestReplayIdentity` 已覆盖）。
- 两层 key 和类型都没变，只有输入变了：沿用持久化结果，并发出 `input_changed`。WaitFor 不会重发请求，Sleep 也不改截止时间。
- 兼容性：Type 为空的旧记录退回严格输入比较；被中断后重新执行的旧 Action 会顺带升级成带 Type 的记录；debugger SQLite 打开时自动补 `type_path` 列，已用旧表结构测试；`Record.Type` 使用 `omitempty`。

### 7.2 已知取舍（有意为之）

1. **类型名就是身份**：类型串来自 `reflect.Type.String()`，类型改名或换包时即使 JSON 不变也算漂移。这和“执行绑定定义版本”的定位一致：改签名就发新版本。另外，`String()` 只带包名、不带完整路径，不同路径下同名包里的同名类型会被视为同一类型。这属于漏报而不是误报，风险低。
2. **输入变化只告警**：`input_changed` 不会让流程失败。如果业务要求“输入变了就必须失败”，可以订阅这个事件后自行中止，或者把关键输入编入 key。
3. **WaitAny 成员请求变化不发事件**：组记录已经存在时，比较由存储适配器内的 `SameWaitMember` 完成，引擎拿不到旧请求，所以只能保证不失败、不重发。
4. **`$definition` 仍严格比较 name + version + input**：它是 run 身份，输入不同说明调用方把一个 run 路由给了另一份输入，必须失败。

### 7.3 本轮新发现并已修复

| 问题 | 影响 | 修复 |
| --- | --- | --- |
| `RetryPolicy.delay` 溢出 | 当 `Multiplier` 很大或尝试次数很多时，先把 float64 转成 `time.Duration` 再与 `MaxInterval` 比较，超出范围的转换结果依平台而定，可能变成负数，从而得到零延迟，造成重试风暴 | 先在 float64 中与上限比较，`next >= ceiling` 时直接返回上限，之后才转换；`engine_test` 增加 `Multiplier: 1e19` 用例 |

### 7.4 复核后确认无问题的点

- **跨 strand 等待 Future**：GoFlow A 在 `Wait()` 等 GoFlow B 的 Future，B 挂起时，A 会再次停止，不会拿到零值；Pending 只列出 B 的等待，root 停在 `Wait()` 处，后续原语不会登记（探针 `/tmp/wfprobe`，通过 overlay 注入）。
- **预加载与 Type**：`terminalRecords` 只收录终态记录，回放时依旧走 `sameStep`，类型漂移照样能拦截（`optional_infrastructure_test` 已覆盖）。
- **Type 在各存储路径中的传递**：Memory 的 Commit/Put/Resolve/WaitAny/settle/FailWait/advance 都使用 `copyRecord` 或原记录，Type 不会丢；debugger 的 wire、store 读写、Records、Detail 都带 Type，UI 抽屉会展示。
- **Running Action 重新执行**：先用 `sameStep` 校验，再改写 Type/Input 后提交；旧记录如果输入不同，在改写前就会报 `ErrChanged`。
- debugger 的 `Run` 循环（ACK 只在非 Running 时进行，失败后保留并重试）、`waitForWake` 复核、事件 forwarder 的有界缓冲：语义正确。超过 4096 条的事件会被丢弃，但只影响观测，并且会打日志。

### 7.4b WaitAny 去掉 error 返回值（用户确认方案 B）

- 问题：`(Case, error)` 中的 err 只在“所有候选都被 FailWait”时非 nil，含 Sleep 的组永远为 nil，却强迫每个调用方写 `if err != nil`；返回 `(nil, err)` 时忽略 err 直接 `switch winner` 会静默落空；且与 `WaitFor`（单返回值、不可失败）不对称。
- 定位：WaitFor / Sleep / WaitAny 都是**外部信号**，只与系统 API 相关，不是业务步骤；业务失败只来自 Action（`*ActionError`）。
- 改动：签名改为 `WaitAny(key, cases...) Case`；全部候选失败时 `abort(fatal(ErrWaitFailed))`，Drive 报告 `Failed` + `ErrWaitFailed`，WaitAny 之后的编排不会执行（`wait_any_test` 断言 after==0 且重复 Drive 稳定）。单个候选失败仍只剔除该候选（`Case.Failure()`）。
- 降级方式：提供方用 Signal 回答类型化失败值使其成为赢家，或在组里加 Sleep 兜底。
- 兼容性：源码级不兼容（去掉第二返回值），存储格式与 FAILED 组的持久化语义不变。

### 7.4c 主包瘦身：调试/测试设施移入子包（用户要求）

- 原则：主包只保留**引擎会调用的东西**和**存储适配器必须/可选实现的契约**；只为测试、示例、本地调试存在的设施全部移出，降低 `infrastructure.go` 的心智负担。
- `workflow/memory`（新子包）：
  - `MemoryInfrastructure` → `memory.Infrastructure`，`NewMemoryInfrastructure()` → `memory.New()`。
  - `NotificationInfrastructure` → `memory.Outbox`；`Notification` 与 `Request/Wake/CancelNotification` 常量一并移入。理由：引擎从不调用投递接口，它只是参考调度契约；内核契约只保留“Put/Resolve 原子登记投递意图”这一句。
  - 新增测试辅助 `SetClock`、`Seed`、`Stats`，替代测试对私有字段（`now`、`records`、`leases`、`waitGroups`）的直接读写。
  - `TestInfrastructure`（原 `TestMemoryInfrastructure`）与 `ExampleOutbox`（原 `ExampleNotificationInfrastructure`）随包迁移。
- `workflow/debugger`：删除与 `memory.Outbox` 重复的 `Delivery` 接口，改为断言 `memory.Outbox`；`Notification` 类型改用 `memory` 包。
- 主包测试改为黑盒 `package workflow_test`（通过公开 API + `memory` 子包），只有私有退避公式 `RetryPolicy.delay` 保留一个白盒测试 `retry_internal_test.go`。
- 复核主包剩余导出：均为引擎 API、编排 DSL、错误/事件模型或适配器契约（`Infrastructure`、`WaitAnyInfrastructure`、`RecordLister`、`Clock`、`Record`、`WaitRequest`/`WaitAnyState`、`SameWaitMember`），无仅供调试/测试的符号。
- 兼容性：源码级不兼容（导入路径/名称变化，迁移表见 README）；存储格式与 debugger SQLite 结构不变。
- 测试目录：12 个黑盒行为测试文件移入 `workflow/tests`（package tests）；4 个 Example 文件合并为主包 `example_test.go`（Example 需与被文档化的标识符同包才会出现在 godoc）；主包仅余 `example_test.go` 与 `retry_internal_test.go`。迁移前后 PASS 数一致（128）。

### 7.5 剩余风险（不属于缺陷，属于定位边界）

- 外部副作用的 exactly-once 仍需要业务侧使用 `ActionKey` 加 run 身份做幂等，Commit 超时后提交可能已经生效。
- 调度、run 级状态、版本路由都归业务方，引擎只提供 `NotificationInfrastructure` 契约和示例。
- 编排代码里的 `defer` 不能产生副作用，halt 展开时这些 defer 也会执行（文档已写明）。

### 7.6 总体评估

| 维度 | 第一次评估 | 当前 |
| --- | --- | --- |
| 控制流正确性（挂起不可吞、零值不落库） | 不合格 | 合格：halt + strand 标记，recover 后再次停止 |
| 错误语义（业务 / 可重试 / 终态） | 不合格 | 合格：`*ActionError` / Running / `Failed` 三分 |
| 回放身份 | 过严（输入字节比较） | 合理：key + Kind + 类型路径，输入只观测，两层 key 对齐 |
| 存储一致性（CAS + lease fencing） | 良好 | 良好，未改动 |
| API 易用性 | 一般 | 良好：`NewWorkflowFromFlow`、`Signal`、异构 `WaitAny`、`Retry` |
| 性能（回放 IO） | 逐步 Get | 可选一次预加载 |

结论：第一次评估列出的 P0/P1/P2 问题，除按定位不做的 P1-4 和 P1-6、按设计保留的 P2-14 外，已全部关闭；本轮另外发现的一处数值溢出也已修复。当前没有已知的正确性缺陷。验证：`go vet ./workflow/... ./lease/...`、`go test -race -count=2 ./workflow/... ./lease/...`、`go run ./workflow/examples/wait_any` 全部通过。
