# Workflow 重构验证报告

## 范围

新版 workflow 的核心类型、Engine.Drive/runAction、FlowContext 编排与等待、Future、MemoryInfrastructure；旧版 workflow/legacy 保留并独立运行历史测试。

测试采用标准库 testing、接口 fake、可控时钟和 channel，同包场景测试与外部包可执行例子并存；没有新增测试库依赖。没有调用真实视频 API。

## 缺陷分析与回归映射

| 旧实现中的问题 | 新版验证 | 结果 |
| --- | --- | --- |
| Flow 层信号没有持久化、Sleep 重驱延后期限 | TestFlowContextWaitFor、TestFlowContextSleep | 等待登记持久化，原 Until 稳定 |
| 请求确认与恢复共享投递身份造成唤醒消耗 | TestFlowContextWaitFor、memory.TestInfrastructure/resolve_racing_registration_returns_resolved_snapshot | 请求确认后 WAKE 保留，重复注册读到完成响应 |
| 普通 IO/存储错误被永久写 FAILED | TestEngineDrive/ordinary_failure_retries、TestEngineInfrastructureFailure | 错误可重试，已完成提交可回放 |
| 旧租约 owner CAS 写入结果 | memory.TestInfrastructure/fencing_expiry_CAS_and_deep_copies | 过期及被接管 owner Commit 均拒绝 |
| 重复 key 返回其他输入的结果 | TestFlowContextRun | 重复及非法 key 拒绝，输入变化拒绝 |
| Future 并发 Wait 竞争及阻塞 | TestFutureWait | 16 个并发等待者读取同一结果 |
| 未 Wait 的分支错误被吞掉或驱动结束仍运行 | TestFlowContextGoFlow | 分支错误汇总，返回前兄弟已退出 |
| Loop 冗余驱动重复执行已完成 attempt | TestFlowContextLoop | 等待期间不重复调用，完成及耗尽回放不重跑 |

这些是旧设计问题到新版正确行为的回归映射。新版测试均通过，未将旧版的错误行为作为新用例期望；不宣称保留的 legacy 已修复。

## 生成的用例

- tests/engine_test.go：构造参数、根输入与版本、回放、普通错误重试、永久错误、panic 堆栈、超时、取消、忽略挂起的兜底、Observer 隔离、key 校验、竞争驱动和递归 Flow。
- tests/flow_test.go：信号注册与完成、陈旧与重复回调、请求确认后 WAKE、稳定休眠期限、立即休眠、检查点回放、Loop 完成/耗尽、Future 并发 Wait、多分支挂起、错误取消与汇合。
- tests/infrastructure_test.go：存储 generation/expiry fencing、版本 CAS、深拷贝、terminal 状态保护、Resolve/Register 交错、所有方法 context 取消、读写/续租/释放/信号登记故障。
- example_test.go：外部包完整“提交订单 → 等待审批 → 完成回调 → 恢复”例子，输出由 Go 示例测试验证。

共 13 个顶层测试/示例入口、24 个子测试；统计来自 go test -json，新版入口与子测试全部通过。子测试数不代表所有表驱动输入数量。

## 验证结果

| 命令 | 结果 |
| --- | --- |
| `go test -race -count=1 -timeout=30s ./workflow ./workflow/legacy` | 通过 |
| `go test -race -count=1 -coverprofile=<临时目录>/coverage.out ./workflow/...` | 通过；新版 87.1%，legacy 86.4% |
| `go vet ./workflow/...` | 通过 |
| `go test ./...` | 全仓通过 |
| `go test -json -count=1 ./workflow` | 新版测试与可执行例子全部通过 |

Step5 第一轮生成 → 验证 → 复核通过（all-pass），没有需要保留的失败断言。之后新增故障/恢复用例和示例均重新验证通过。测试流程的源码修改限制不适用于用户已明确授权的本次生产代码重构。

Step6：项目未要求 CI 覆盖率卡口，门禁检查跳过；上表覆盖率为补充诊断的包级语句覆盖率，不冒充增量覆盖率。未为提高百分比而删除测试或放宽断言。

Step7：执行 utree flush；测试统计、验证结论与局限均已记录。

## 验证局限

`workflow/memory` 是进程内参考实现。没有证明具体 MongoDB、Redis 或 MQ 适配器的原子性，也没有执行真实 API、跨进程宕机或时钟漂移测试。生产部署应对自定义 Infrastructure 的 Commit、Put、Resolve 原子契约运行适配器测试。

IO 副作用到结果提交之间仍存在不确定窗口，需要外部幂等。不响应 context 的 Action 会阻塞汇合。新版 Record 与旧事件 schema 不兼容，旧在途实例需要保留旧版路由或显式迁移。

## 独立 lease 包抽取后的验证

范围：lease.NewRunner、Runner.Run 生命周期，以及 workflow 通过 Runner 保护完整提交的集成路径。上文覆盖率是首次重构快照，本节为抽取后的最新结果。

缺陷分析：原 workflow 在最终提交前停止续租，长耗时提交会耗尽剩余有效期；生命周期散落于 workflow，无法单独复用。新增集成测试让最终 Commit 等待三个续租周期，断言提交完成且随后释放。Runner 本身不管理业务提交，仍由存储验证 token。

生成用例：lease/lease_test.go 覆盖构造默认值、参数校验、正常续租、nil Claim、占用冲突、续租错误/nil/Generation 变化、业务与释放错误合并、取消后有界清理、context values 保留、panic 清理、并发复用。workflow/lease_integration_test.go 验证续租覆盖提交。

验证：`go test -race -count=1 -timeout=30s -cover ./lease ./workflow ./workflow/legacy` 全部通过；包级语句覆盖率 lease 96.3%、workflow 86.7%、legacy 86.4%。`go vet ./lease ./workflow/...`、`go test ./...`、`git diff --check` 均通过。新增测试第一轮生成、验证、复核通过，无保留失败测试。utree flush 已再次执行。

## 可订阅事件机制

范围：新增 Event/EventError/EventType、Engine.Subscribe、分发隔离和驱动/Action/等待触发点；现有 Options.Observer/Observation 保持原公开签名与 phase。

缺陷分析：本次没有确认的缺陷条目。测试重点验证并发快照、回放与实际执行的区分、Resolve 幂等、校验失败的结束事件和异常隔离；未把不同 Drive 上同 key 的挂起观察视为重复交付。

生成用例：tests/events_test.go 包含正常执行/回放精确事件序列、JSON 往返、ID 唯一性、拒绝/取消驱动、暂时/永久/panic/超时、读取失败和抢租约冲突、快照修改隔离、监听者/Observer panic、损坏错误格式化方法、Observer 阶段兼容、筛选/重入退订、并发订阅变更、多分支、信号重复 Resolve、Sleep 与立即休眠。example_test.go 提供外部包筛选订阅示例。

验证结果：新增 3 个顶层测试与 1 个可执行示例，15 个子测试全部通过（go test -json 统计）。第一次生成、验证通过；补充冲突、坏错误格式化和示例后重新验证通过，未保留失败用例或放宽断言。

`go test -race -count=1 -timeout=30s ./workflow/... ./lease`、新版视频模拟用例 race 测试、`go vet ./workflow/... ./lease`、`go test ./...` 和 `git diff --check` 均通过。最后一次 workflow 包级语句覆盖率为 89.7%；CI 增量覆盖率门禁按未要求跳过。

交付边界：同步进程内观察事件，监听者必须及时返回。每次触发对当时订阅快照逐一通知，panic 隔离；已快照通知可能在退订后完成。事件并不与业务存储构成事务，进程崩溃及存储提交结果未知时不保证持久化交付，可靠调度继续使用 outbox。

## WaitAny 多路等待

范围：FlowContext.WaitAny、可选 WaitAnyInfrastructure、memory.Infrastructure 原子等待组、原 Resolve 的赢家选择与取消、Engine.FailWait、CANCEL 通知和双监听者清理示例。

缺陷分析：本次无确认的未修复缺陷。重点验证首个完成者唯一性、取消通知与请求清理、失败隔离、快照深拷贝、迟到回调和稳定回放。

生成用例：tests/wait_any_test.go 的 8 个子测试覆盖全部候选登记、赢家取消败者且后续 Action 只执行一次、同时回调仅接受一个赢家、单候选失败、全候选失败与唤醒、请求编码失败及 panic 隔离、非法拓扑无部分登记、旧适配器与 context 取消、请求/快照不可变及非法响应不改变赢家。example_test.go 验证完整输出；examples/wait_any/main.go 提供双并行外部监听者的可运行清理程序。

验证：新增测试首次生成/验证通过；增加 CANCEL 通知、快照边界后重新验证通过。`go test -race -count=1 -timeout=30s -cover ./workflow/... ./lease`、`go vet ./workflow/... ./lease`、`go test ./...`、`git diff --check` 均通过。最后一次 workflow 包级语句覆盖率为 89.5%，未要求的 CI 增量门禁跳过。utree flush 已执行。

`go run ./workflow/examples/wait_any` 实际输出：两个 active waits，secondary 完成后 continued，primary=CANCELLED，provider listeners stopped。库内等待无常驻 goroutine；外部跨进程业务须消费 CANCEL 协作清理。原子等待组和历史结果保留用于回放，属于持久化记录，不是残留监听句柄。

## 统一 Signal 与混合等待

WaitFor 和 Sleep 统一为 Signal 描述。保留原 WaitCase 泛型调用，新增 WaitForSignal、SleepSignal、SleepUntilSignal 构造器；WaitAnyResult 增加赢家 Kind。存储保存 Kind、Duration、Until、WinnerKind，相对定时截止时间只初始化一次。定时信号到期后按最早截止时间结算，迟到的响应或失败报告不能取代它。未知 Kind 由参考存储明确拒绝，自定义适配器可扩展新种类。

> 2026-10-17 更新：上述 WaitCase / WaitForSignal / SleepSignal / WaitAnyResult 已被异构 Case API 取代（`OnSignal[R]` / `OnKind[R]` / `OnSleep` / `OnSleepUntil`，`f.WaitAny(group, cases...)` 返回赢家 Case），存储格式与语义不变，测试已全部迁移。见 [WAIT_ANY.md](WAIT_ANY.md)。

tests/signals_test.go 新增 7 个子测试：回复获胜取消定时、超时获胜与回放、迟到回调不可抢占、多个定时的最早截止时间、立即定时、失败外部信号与健康定时、未知种类无部分登记。example_test.go 验证可运行的“回复或超时”示例。

验证：新增场景、旧 WaitAny 场景及示例均通过；`go test -race -count=1 -timeout=30s ./workflow/... ./lease` 与对应 vet 通过。覆盖率卡口未要求，未追加门禁。没有保留失败的 workflow 用例。

## Action 标准接口实施

范围：正式 Action/ActionFn/AsAction/Pure、统一 Run/Go/Loop 执行路径、typed-nil 校验，以及示例、debugger 和新版视频用例的调用迁移。FlowContext 保持非泛型；视频客户端和 API Key 移入具名提交/查询 Action。

缺陷分析：本次无确认的未修复缺陷。接口替换后原生函数不能直接作为 Action，初次编译检查按预期暴露未迁移调用；调用已适配为 AsAction 或具名 Action，没有降低类型安全。nil 入口在租约和步骤写入前统一拒绝。

生成用例：tests/action_test.go 的 4 个顶层测试、9 个子测试全部通过（go test -json 统计）。覆盖 ActionFn 的 context/input/output/error 转发、nil 函数、AsAction 类型推断、指针/值实现、Run/Go/Loop 回放及 15 个 nil 接入组合（3 条执行路径 × 5 类 nil）。既有错误、并发、轮询、WaitAny 和事件断言保留，函数调用仅增加适配。

验证结果：`go test -race -count=1 -timeout=60s -coverprofile=<临时路径> ./workflow/... ./lease` 通过；workflow 包级语句覆盖率 90.6%，debugger 71.6%，legacy 86.4%，lease 96.3%。`go vet ./workflow/... ./lease` 通过。新版视频模拟用例 race 测试通过；具名 Action 示例两次 Drive 仅查询一次 Repository。没有调用真实视频 API。

全仓 `go test ./...` 被独立 condition 编译错误阻断：condition/typed.go 与 condition/schema.go 重复定义 MaxDepth、SelfField、argPath、literal、scalar，且 literal 推断失败。当前 condition 工作区改动未在本任务中修改。相关 condition/examples 构建失败由同一错误导致，workflow 包本身通过。

测试生成/验证/复核通过，未保留失败的 workflow 用例。未要求的 CI 增量门禁跳过；上述是诊断包级覆盖率。API 是已授权的源码变更：原函数经 AsAction/ActionFn 适配，key/input/output/Record 格式保持不变。公开 Action 接口已落地，设计文档与正式示例已同步。

## 多层并行等待

以下为上一版递归方案的历史验证记录。当前三层调用规范禁止 Flow 嵌套，测试已迁移为平级并行场景，见本文“三层调用权限”。

范围：GoFlow、RunFlow 的后代汇合与 Drive 挂起聚合，独立实例隔离、重复驱动、并发回调、混合 Sleep/WaitFor/WaitAny、异常与取消。测试位于 tests/parallel_wait_test.go。

缺陷分析：确认并修复一项 P1 完成边界缺陷。子 Flow 启动 GoFlow 后不 Wait，自身返回成功，原 Future/RunFlow 会提前报告成功；父 Flow 检查返回错误仍可能执行后续 Action，最终 Drive 才返回 Suspended。第一轮 `go test -count=1 -timeout=15s ./workflow -run '^TestNestedParallelWaits$'` 两个子场景均失败，稳定显示两个 pending 尚未完成而 continuation calls=1。不是死锁或测试时序问题。

处理：给每个子 Flow 增加 flowScope，返回前汇合该作用域的异步任务及错误；GoFlow 和 RunFlow 共用同一执行路径。Loop/WaitAny 派生命名空间继承 scope，关闭后拒绝继续注册。公开 API、key 和存储格式不变。挂起不取消健康兄弟，实际错误沿用驱动级取消策略。

生成用例：4 个顶层测试、7 个子测试。两种子 Flow 调用路径先挂起两个孙分支，逐一回调并验证只在全部完成后执行 continuation；12 个独立 Workflow 同时运行三层并行树，共 48 个外部等待和 48 个 Sleep，每个外部等待竞争四次重复回调，分批恢复后回放不重复执行 Action；同一作用域拒绝不同输入；16 个同实例驱动并发登记只生成一个 REQUEST，解析后只生成一个 WAKE；未等待后代错误阻止父 continuation；取消后 IO 退出、租约释放、持久信号保留；嵌套 WaitAny 的两组分别由响应和超时获胜，迟到回调不能复活失败方。

验证结果：修复后新增测试连续 20 轮 race 验证通过，go test -json 统计为 4 个入口、7 个子测试，0 失败。`go test -race -count=1 -timeout=60s -coverprofile=<临时路径> ./workflow/... ./lease` 和 `go vet ./workflow/... ./lease` 通过，workflow 语句覆盖率 90.8%。未要求增量覆盖率门禁，跳过；不保留失败用例。本轮未重跑全仓，也未调用真实第三方 API。

使用边界：Infrastructure 每个独立实例绑定独立作用域；恢复调度必须路由到同一实例并保留失败 WAKE 重试。等待点只保留持久记录，不保留监听 goroutine。正在执行的 Action 必须响应 context 并设置超时；框架不会强制杀死用户函数，不支持跨 Future 循环等待或用原生 goroutine 调用逃逸 FlowContext。高扇出需外部并发预算。业务代码仍必须检查等待和子 Flow 返回的错误。

结论：复现用例已确认原问题，修复后常规与回归测试全部通过；无本轮未修复的确认缺陷。

## 三层调用权限

以下为先前运行期权限方案的历史记录，现已被本文“编译期调用层级”取代。

范围：仅 workflow 目录，正式引擎的 Drive 根授权、RunFlow/GoFlow 校验、公开类型注释、架构/使用规范与相关测试。legacy 保留历史实现，未替换其他 package 或修改其文件。

设计与缺陷分析：本轮为明确的架构约束变更，先前递归能力符合旧契约，不作为存量缺陷计数。新契约限定“顶层 → Flow → Action”，Flow 内 RunFlow/GoFlow、自调用、循环调用均禁止。私有 topLevel 权限仅在 Drive 根上下文设置，子上下文不继承。ErrFlowCallForbidden 包装 ErrInvalid；违规调用在 claim、任务创建和存储访问前拒绝，忽略错误也不能使 Drive 报告完成。

生成及迁移用例：tests/parallel_wait_test.go 的 4 个顶层入口、12 个子测试。权限测试覆盖顶层同步/异步挂载与 Flow 内同步/异步调用的四种组合，已完成错误 Future、忽略错误防护、不注册 key、无被调用步骤/租约/消息、自调用与循环调用。先前嵌套并行测试按新规范展平，继续验证 12 个独立实例 × 8 个平级 Flow 的等待恢复、重复回调、作用域隔离、16 个冗余驱动的通知幂等、Flow 内异步 Action 汇合与取消，以及两个平级 Flow 的 WaitAny 响应/超时竞争。tests/engine_test.go 的执行点验证迁移为合法的顶层 → Flow → Action。

验证：第一轮最小范围 race 通过，无未解释的编译或测试失败；全体新增场景连续 20 轮 race 通过，go test -json 统计为 4 个入口、12 个子测试、0 失败。`go test -race -count=1 -timeout=60s -coverprofile=<临时路径> ./workflow/...` 和 `go vet ./workflow/...` 全部通过。workflow 语句覆盖率 90.9%，debugger 71.6%，legacy 86.4%。未要求增量覆盖率门禁，跳过；未重跑其他包或真实第三方 API。

边界：运行期校验约束框架入口；Go 代码直接调用其他 Flow.Run、闭包偷用根 Context、Action 持有 Engine 并重入等属于禁止的代码模式，不能把本库视为沙箱。规范明确 Action 不接收编排上下文，也不得持有可重入依赖。公开方法签名保持不变，嵌套调用能力被明确撤销，旧在途定义需按版本和存储范围迁移。

结论：新权限规则及合法平级并行恢复场景全部通过，无本轮确认的未修复缺陷。全部业务源码和测试文件修改均位于 workflow 目录。

## 编译期调用层级

范围：仅 workflow 目录。新增独立 WorkflowContext，将 RunFlow/GoFlow 从 FlowContext 移到顶层；Workflow 通过 NewWorkflow 构造，保存私有入口，不实现 Flow，也不公开 Run。Engine.Drive 是定义执行入口。顶层不提供 Action、信号或检查点原语；所有执行点放在 Flow 中。

缺陷分析：先前运行期方案虽能拒绝嵌套框架调用，但公开 FlowContext 仍提供调用方法，与编译期禁止的要求不符。本轮按新 API 契约修正；flowScope 保留，因为异步 Action 的完成边界与调用权限是独立职责。无新增确认的运行期缺陷。

生成及迁移用例：TestFlowCallHierarchy 使用外部业务包真实编译，断言七种拒绝结果：FlowContext 的 RunFlow、GoFlow；WorkflowContext 的 Run、Sleep；Workflow 的 Run；将 Workflow 传入 RunFlow；将 FlowContext 转换为 WorkflowContext。顶层同步/异步调用、Flow 异步 Action 汇合、取消、12 个独立实例 × 8 个平级 Flow 挂起恢复、重复回调、WaitAny 竞争继续保留。原子原语单测用包内辅助函数保留空前缀，公开层级测试与全部外部示例通过真实 RunFlow/GoFlow 验证。debugger 双实例场景拆为 prepare、shipping、notify、finish 平级 Flow。

验证循环：首轮 `go test ./workflow/... -count=1 -timeout=90s` 中，行为测试通过；编译期测试正确拒绝非法调用，但泛型推断错误文本与“missing method Run”断言不符。给该负例显式提供类型参数，使编译器报告接口缺失，未放宽断言。修正后 `go test -race ./workflow/... -count=1 -timeout=90s` 全部通过；`go vet ./workflow/...` 与 `git diff --check -- workflow` 通过。两个可运行示例另行验证。未要求 CI 覆盖率卡口，覆盖率检查跳过，不以包级覆盖率替代增量覆盖率。

迁移边界：这是源码 API 变更，移除 Workflow.Flow、FlowContext.RunFlow/GoFlow、topLevel 与 ErrFlowCallForbidden。调用方改用 NewWorkflow，根 handler 接收 WorkflowContext；根 Action/等待下移到 Flow。debugger 示例使用 v2 定义及新 key，旧在途实例继续路由到旧版代码。公开 Flow.Run 和 Go 闭包仍须遵守编码规范，编译期约束不等于任意用户代码沙箱。
