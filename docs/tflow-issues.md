# tflow Issues

用于记录当前设计/实现里发现但暂不立即修改的问题，后续统一汇总处理。

## 1. Rerun 语义混淆：失败重试 vs 成功重放

当前 `Engine.Rerun` 的语义更接近“恢复失败/取消的执行单元”：

- 只允许 main 处于 `Failed` / `Canceled`。
- 只允许选择 `Failed` / `Canceled` subtask。
- 会将 subtask `Attempt++`，状态改回 `Pending`，清理 `ExternalRef` / `Error`。
- 会将 main 改回 `Running` 并写入 `ReconcileCommand`。

这个能力放在框架层有合理性，因为它维护的是框架状态机不变量：attempt 隔离、旧回调失效、summary 一致性、reconcile 命令原子写入、并发 rerun 只允许一个成功等。

但用户需求可能是“成功的 subtask 也能 rerun”。这时当前 `Rerun` 名字和范围就容易混淆，因为成功重跑不是失败恢复，而是业务重放/重算。

待决策点：

- 是否把现有 `Rerun` 改名或收窄为 `Retry` / `RetrySubTasks`，表达失败/取消恢复语义。
- 是否新增 `ReplaySubTasks` / `ResetSubTasks` 一类更通用的框架原语，允许 terminal subtask 包括 `Succeeded` / `Failed` / `Canceled` 进入下一次 attempt。
- 如果允许成功重放，框架只负责生成新 attempt 和状态机一致性；旧成功业务结果的可见性、覆盖、废弃或版本选择必须由业务结果层定义。
- API 是否应按 keys 精确选择，而不是按状态 scope 粗选，避免误重放成功结果。
- 是否需要显式命名 receipt / command / docs 中的语义：`retry failed work` 与 `replay materialized work` 是两件事。

倾向方案：

- 保留框架层的 attempt 重置原语，但不要让它承载业务策略。
- 将“失败/取消恢复”和“成功/任意终态重放”拆成两个命名清楚的 API。
- 文档明确要求业务侧用 `Execution.Key()` / attempt 版本处理旧输出和新输出的关系。
