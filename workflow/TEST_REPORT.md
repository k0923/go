# Workflow 重构验证报告

## 范围

新版 workflow 的核心类型、Engine.Drive/runAction、FlowContext 编排与等待、Future、MemoryInfrastructure；旧版 workflow/legacy 保留并独立运行历史测试。

测试采用标准库 testing、接口 fake、可控时钟和 channel，同包场景测试与外部包可执行例子并存；没有新增测试库依赖。没有调用真实视频 API。

## 缺陷分析与回归映射

| 旧实现中的问题 | 新版验证 | 结果 |
| --- | --- | --- |
| Flow 层信号没有持久化、Sleep 重驱延后期限 | TestFlowContextWaitFor、TestFlowContextSleep | 等待登记持久化，原 Until 稳定 |
| 请求确认与恢复共享投递身份造成唤醒消耗 | TestFlowContextWaitFor、TestMemoryInfrastructure/resolve_racing_registration_returns_resolved_snapshot | 请求确认后 WAKE 保留，重复注册读到完成响应 |
| 普通 IO/存储错误被永久写 FAILED | TestEngineDrive/ordinary_failure_retries、TestEngineInfrastructureFailure | 错误可重试，已完成提交可回放 |
| 旧租约 owner CAS 写入结果 | TestMemoryInfrastructure/fencing_expiry_CAS_and_deep_copies | 过期及被接管 owner Commit 均拒绝 |
| 重复 key 返回其他输入的结果 | TestFlowContextRun | 重复及非法 key 拒绝，输入变化拒绝 |
| Future 并发 Wait 竞争及阻塞 | TestFutureWait | 16 个并发等待者读取同一结果 |
| 未 Wait 的分支错误被吞掉或驱动结束仍运行 | TestFlowContextGoFlow | 分支错误汇总，返回前兄弟已退出 |
| Loop 冗余驱动重复执行已完成 attempt | TestFlowContextLoop | 等待期间不重复调用，完成及耗尽回放不重跑 |

这些是旧设计问题到新版正确行为的回归映射。新版测试均通过，未将旧版的错误行为作为新用例期望；不宣称保留的 legacy 已修复。

## 生成的用例

- engine_test.go：构造参数、根输入与版本、回放、普通错误重试、永久错误、panic 堆栈、超时、取消、忽略挂起的兜底、Observer 隔离、key 校验、竞争驱动和递归 Flow。
- flow_test.go：信号注册与完成、陈旧与重复回调、请求确认后 WAKE、稳定休眠期限、立即休眠、检查点回放、Loop 完成/耗尽、Future 并发 Wait、多分支挂起、错误取消与汇合。
- infrastructure_test.go：存储 generation/expiry fencing、版本 CAS、深拷贝、terminal 状态保护、Resolve/Register 交错、所有方法 context 取消、读写/续租/释放/信号登记故障。
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

MemoryInfrastructure 是进程内参考实现。没有证明具体 MongoDB、Redis 或 MQ 适配器的原子性，也没有执行真实 API、跨进程宕机或时钟漂移测试。生产部署应对自定义 Infrastructure 的 Commit、Put、Resolve 原子契约运行适配器测试。

IO 副作用到结果提交之间仍存在不确定窗口，需要外部幂等。不响应 context 的 Action 会阻塞汇合。新版 Record 与旧事件 schema 不兼容，旧在途实例需要保留旧版路由或显式迁移。

## 独立 lease 包抽取后的验证

范围：lease.NewRunner、Runner.Run 生命周期，以及 workflow 通过 Runner 保护完整提交的集成路径。上文覆盖率是首次重构快照，本节为抽取后的最新结果。

缺陷分析：原 workflow 在最终提交前停止续租，长耗时提交会耗尽剩余有效期；生命周期散落于 workflow，无法单独复用。新增集成测试让最终 Commit 等待三个续租周期，断言提交完成且随后释放。Runner 本身不管理业务提交，仍由存储验证 token。

生成用例：lease/lease_test.go 覆盖构造默认值、参数校验、正常续租、nil Claim、占用冲突、续租错误/nil/Generation 变化、业务与释放错误合并、取消后有界清理、context values 保留、panic 清理、并发复用。workflow/lease_integration_test.go 验证续租覆盖提交。

验证：`go test -race -count=1 -timeout=30s -cover ./lease ./workflow ./workflow/legacy` 全部通过；包级语句覆盖率 lease 96.3%、workflow 86.7%、legacy 86.4%。`go vet ./lease ./workflow/...`、`go test ./...`、`git diff --check` 均通过。新增测试第一轮生成、验证、复核通过，无保留失败测试。utree flush 已再次执行。

## 可订阅事件机制

范围：新增 Event/EventError/EventType、Engine.Subscribe、分发隔离和驱动/Action/等待触发点；现有 Options.Observer/Observation 保持原公开签名与 phase。

缺陷分析：本次没有确认的缺陷条目。测试重点验证并发快照、回放与实际执行的区分、Resolve 幂等、校验失败的结束事件和异常隔离；未把不同 Drive 上同 key 的挂起观察视为重复交付。

生成用例：events_test.go 包含正常执行/回放精确事件序列、JSON 往返、ID 唯一性、拒绝/取消驱动、暂时/永久/panic/超时、读取失败和抢租约冲突、快照修改隔离、监听者/Observer panic、损坏错误格式化方法、Observer 阶段兼容、筛选/重入退订、并发订阅变更、多分支、信号重复 Resolve、Sleep 与立即休眠。events_example_test.go 提供外部包筛选订阅示例。

验证结果：新增 3 个顶层测试与 1 个可执行示例，15 个子测试全部通过（go test -json 统计）。第一次生成、验证通过；补充冲突、坏错误格式化和示例后重新验证通过，未保留失败用例或放宽断言。

`go test -race -count=1 -timeout=30s ./workflow/... ./lease`、新版视频模拟用例 race 测试、`go vet ./workflow/... ./lease`、`go test ./...` 和 `git diff --check` 均通过。最后一次 workflow 包级语句覆盖率为 89.7%；CI 增量覆盖率门禁按未要求跳过。

交付边界：同步进程内观察事件，监听者必须及时返回。每次触发对当时订阅快照逐一通知，panic 隔离；已快照通知可能在退订后完成。事件并不与业务存储构成事务，进程崩溃及存储提交结果未知时不保证持久化交付，可靠调度继续使用 outbox。

## WaitAny 多路等待

范围：FlowContext.WaitAny、可选 WaitAnyInfrastructure、MemoryInfrastructure 原子等待组、原 Resolve 的赢家选择与取消、Engine.FailWait、CANCEL 通知和双监听者清理示例。

缺陷分析：本次无确认的未修复缺陷。重点验证首个完成者唯一性、取消通知与请求清理、失败隔离、快照深拷贝、迟到回调和稳定回放。

生成用例：wait_any_test.go 的 8 个子测试覆盖全部候选登记、赢家取消败者且后续 Action 只执行一次、同时回调仅接受一个赢家、单候选失败、全候选失败与唤醒、请求编码失败及 panic 隔离、非法拓扑无部分登记、旧适配器与 context 取消、请求/快照不可变及非法响应不改变赢家。wait_any_example_test.go 验证完整输出；examples/wait_any/main.go 提供双并行外部监听者的可运行清理程序。

验证：新增测试首次生成/验证通过；增加 CANCEL 通知、快照边界后重新验证通过。`go test -race -count=1 -timeout=30s -cover ./workflow/... ./lease`、`go vet ./workflow/... ./lease`、`go test ./...`、`git diff --check` 均通过。最后一次 workflow 包级语句覆盖率为 89.5%，未要求的 CI 增量门禁跳过。utree flush 已执行。

`go run ./workflow/examples/wait_any` 实际输出：两个 active waits，secondary 完成后 continued，primary=CANCELLED，provider listeners stopped。库内等待无常驻 goroutine；外部跨进程业务须消费 CANCEL 协作清理。原子等待组和历史结果保留用于回放，属于持久化记录，不是残留监听句柄。

## 统一 Signal 与混合等待

WaitFor 和 Sleep 统一为 Signal 描述。保留原 WaitCase 泛型调用，新增 WaitForSignal、SleepSignal、SleepUntilSignal 构造器；WaitAnyResult 增加赢家 Kind。存储保存 Kind、Duration、Until、WinnerKind，相对定时截止时间只初始化一次。定时信号到期后按最早截止时间结算，迟到的响应或失败报告不能取代它。未知 Kind 由参考存储明确拒绝，自定义适配器可扩展新种类。

signals_test.go 新增 7 个子测试：回复获胜取消定时、超时获胜与回放、迟到回调不可抢占、多个定时的最早截止时间、立即定时、失败外部信号与健康定时、未知种类无部分登记。signals_example_test.go 验证可运行的“回复或超时”示例。

验证：新增场景、旧 WaitAny 场景及新旧示例均通过；`go test -race -count=1 -timeout=30s ./workflow/... ./lease` 与对应 vet 通过。全仓 `go test ./...` 被独立的 fluent/v2 编译错误阻断：`fluent/v2/request.go:359: r.client.idleTimeout undefined (type *Client has no field or method idleTimeout)`，该包未在本次任务中修改。覆盖率卡口未要求，未追加门禁。没有保留失败的 workflow 用例。
