# Workflow 核心架构重构

## 决策

Action 采用标准 Run 接口，并提供 ActionFn/AsAction 函数适配；Flow 保留具名接口，Workflow 由 NewWorkflow 构造并保存私有顶层处理函数。独立 WorkflowContext 只提供 Flow 调用；FlowContext 保持非泛型，只提供 Action、信号与检查点原语。两种上下文不互相嵌入。外部客户端与 Repository 注入 Action，Context 不承载业务扩展能力。执行引擎显式接收基础设施；根输入、Flow 输入和 Action 输入都显式传递。

```mermaid
flowchart TD
    Caller["调度方：实例隔离、版本路由、重试"] --> Engine["Engine.Drive：根输入及驱动结果"]
    Definition["Workflow：Name + Version + 私有入口"] --> Engine
    Engine --> Root["WorkflowContext：顶层编排"]
    Root --> Flow["Flow：仅由顶层 RunFlow / GoFlow 调用"]
    Flow --> Context["FlowContext：命名空间、Action、等待、检查点"]
    Context --> Action["Action：标准 context + input → output, error"]
    Context --> Wait["WaitFor / Sleep：独立等待快照"]
    Action --> Commit["租约 fencing + Record CAS"]
    Wait --> Store["Infrastructure：实例范围内原子存储"]
    Commit --> Store
    Callback["外部回调"] --> Resolve["ResolveSignal：响应 + WAKE 原子提交"]
    Resolve --> Store
    Store --> Delivery["REQUEST / WAKE 持久化投递"]
    Delivery --> Caller
    Engine --> Observer["Subscribe：执行、回放、挂起、错误"]
```

## 原设计与处理

| 原设计问题 | 新设计 |
| --- | --- |
| WorkflowContext → FlowContext → ActionContext，公开能力与实际租约范围不一致 | 独立 WorkflowContext/FlowContext，不继承能力；Action 使用标准 context |
| Action 挂起导致从头重跑副作用 | 等待在 Flow，副作用通过独立 Action Run 提交 |
| 根 RunFlow 接收却不使用 input | Workflow 泛型定义，Drive 显式传入顶层 handler |
| 同步、异步、Loop 分叉维护错误和状态 | 同一个 runAction；Go 复用，Loop 每 attempt 复用 |
| Flow 递归挂载导致层级混乱 | FlowContext 无 RunFlow/GoFlow；WorkflowContext 无 Action/信号方法，编译期隔离 |
| panic 混用业务失败、存储故障、挂起和冲突 | 挂起与框架故障用私有 panic + strand 停止标记（不可吞、不可伪造）；业务失败用 `*ActionError` 返回；Drive 以 Result 报告 |
| 挂起以 error 传递，被忽略后零值副作用被提交 | 挂起不再是 error；recover 后同一 strand 的原语再次停止 |
| 原语出错即取消执行，补偿不可达 | 只有 handler 返回的 error 决定失败 |
| 事件 CAS 未携带租约导致旧 owner 写入 | Commit 同时验证当前 lease generation、expiry 和 record version |
| 更新挂起状态与信号登记不同事务 | 等待原语单独 Put，等待记录和消息一次原子提交 |
| 请求 outbox 确认可能掩盖后续恢复投递 | REQUEST 与 WAKE 是不同幂等消息 |
| 缺少依赖时静默启用内存数据库 | NewEngine 必须显式提供 Infrastructure |
| Future 单次 channel 消费导致并发 Wait 死锁 | close(done) 广播，结果不可变 |

## Action 纯函数化的评估

数学纯函数要求没有副作用、隐藏输入或可变状态。当前库必须支持 HTTP、数据库和消息，因此把所有 Action 宣称为纯函数会与用途矛盾。

选定方案是“标准接口 + 函数适配器 + 明确纯度契约”：

1. Action 的可调用签名是标准 context、明确输入和输出；取消和 trace 仍需 context，因此保留 context 参数。
2. 轻量纯函数在 Flow 中直接执行，不生成存储记录；昂贵纯计算可由 Pure 适配后持久化。
3. IO 函数同样可作为 Action，其业务结果应依赖 input，依赖通过具名对象的方法绑定；外部幂等键显式传递。
4. Now/UUID 从 Flow 检查点获取后作为 input 传递，避免 Action 中引入重试相关非确定值。
5. 不提供 Action 访问 Flow 的后门；不得闭包捕获 FlowContext、可变流程局部变量或共享业务状态。

收益是具名 Action 显式持有窄依赖，并可独立测试；普通函数和客户端方法通过 AsAction 适配，保持类型推断；调度能力由类型边界约束。限制是 Go 类型系统无法证明函数纯度、捕获状态或数据不可变，工程规范和代码检查仍然必要。

## 执行与一致性

Action 路径：读取 → 完成/失败回放 → lease.Runner 申请租约 → 重新读取 → 必要时创建 RUNNING → 执行并续租 → 原子提交结果（仍续租）→ 停止续租 → 有界释放租约。

租约生命周期抽到独立的 `github.com/k0923/go/lease` 包。Runner 管理 Claim、Renew、取消、清理；Token 由业务存储用于 fencing。workflow.Infrastructure 嵌入 lease.Infrastructure，workflow.Lease 保留为 Token 类型别名。lease 不依赖 workflow，也不感知状态、事件、信号或事务。

续租失败会取消 Action context；即便执行者没及时停止，Commit 仍由存储校验租约，拒绝过期或被新代接管的写入。写成功但客户端超时的未知结果由后续 Get + replay 收敛。外部副作用与 Commit 不在一个事务，仍需外部幂等保证。

等待路径：Put 同时提交悬挂快照与请求消息；Resolve 同时提交响应与唤醒消息。没有“正在等待的 Action 已改状态、但 signal/outbox 尚未提交”的跨事务窗口。由于 Flow 不持租约，回调驱动不受等待单元租约阻挡。

两个驱动可同时遍历 Flow，Action 按 key 排他，检查点按 key insert-if-absent 选定唯一值。输入及定义版本检查防止错误回放；它不能识别保持相同版本和 key 时偷偷修改业务函数的行为，部署必须遵循版本契约。

## 并发政策

框架注册的异步分支归属于单次 Drive；Drive 会等待全部分支退出，错误取消兄弟 IO。挂起只登记 Pending，不取消兄弟，避免独立分支完成结果被无意义中断。Future 允许多个调用方等待同一结果，未 Wait 的错误仍由 Drive 接收。

WorkflowContext 只拥有 RunFlow/GoFlow，FlowContext 只拥有 Action 和信号方法。没有 topLevel 标记，也没有运行期权限错误；Flow 内的跨流程框架调用直接编译失败。Workflow 不暴露执行方法，也不实现 Flow，只通过 Engine.Drive 进入。允许的框架调用图只有“顶层 → Flow → Action”。普通 Go 方法调用和闭包捕获仍需遵守 CALL_HIERARCHY 中的编码边界。

每个 RunFlow/GoFlow 使用独立 flowScope 跟踪 Flow 中的异步 Action 和错误，Flow 返回前汇合 scope。Loop 和 WaitAny 的内部命名空间继承当前 scope，不创建可跨流程调用的上下文。scope 的可变状态使用 execution.mu 保护；等待时不持锁，关闭 scope 后拒绝继续注册执行点。全局 execution.wg 继续保证 Drive 没有残留分支。

这个选择优先保证驱动结束时没有残留执行。挂起通知的返回延迟可能等于最长在途 Action 的耗时；需要短时响应时给 IO 设置 timeout，或由外部调度拆分长任务。不能安全杀死不响应 context 的 Go 函数，使用约束必须明确。

## 扩展与边界

- 存储扩展：Infrastructure 明确原子方法契约，生产适配器独立实现；实例隔离仍在包装层。
- 投递扩展：投递不在内核契约内，参考契约为 `memory.Outbox`（Pending/Ack，至少一次），引擎不调用它；通知中实例路由由适配器绑定；调度器遇到冲突和暂时错误必须保留 WAKE 重试。
- 回放扩展：可选 RecordLister 让 Drive 一次预加载终态记录，回放不再逐步读写；只缓存不可变结果，未结束的记录仍走原子方法。
- 时钟扩展：可选 Clock 在 Options.Now 为空时作为默认时钟，使 Sleep 截止与租约到期同源。
- 等待扩展：WaitAny 的 Case 各自携带响应类型（OnSignal/OnSleep/OnSleepUntil/OnKind），按指针身份返回赢家；自定义 Kind 由 WaitAnyInfrastructure 实现识别。
- 观测扩展：Subscribe 事件覆盖生命周期、回放和错误，panic 被隔离；审计日志与 CAS 快照分离。
- 定义扩展：Flow 接口保留具名类型和函数适配；Workflow 负责稳定名称与版本。
- 当前边界：没有生产数据库适配器、自动调度器、工作流代码沙箱、历史代码热迁移、实例终止或补偿事务。

## 迁移策略

原实现及旧测试放入 workflow/legacy，未将旧版已知问题包装成新版兼容承诺。新版核心位于 workflow 包。旧事件 schema 不能直接作为新版 Record 使用，已有在途实例继续路由到 legacy；新实例使用新版及独立存储范围。

带 workflow_video_enhance_fast build tag 的本地 tmp 用例保留原 API，包引用指向 legacy；未执行真实外部 API。未来可将提交和查询方法改为普通 Action，将 Loop 留在 Flow 层，独立迁移这个业务用例。
