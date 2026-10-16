# 重构与测试任务

- [x] Step1 测试准备
- [x] Step2 加载 Go 测试知识与项目上下文
- [x] Step3 确定范围：现代 API、引擎、持久化、上下文、迁移
- [x] Step4 缺陷映射：挂起丢失、错误误分类、过期写入、重复 key、并发汇合
- [x] Step5 生成、验证、修复：新版与 legacy race 测试全部通过
- [x] Step6 覆盖率：CI 门禁跳过；诊断包级语句覆盖率新版 87.1%
- [x] Step7 文档、可执行示例、测试报告；utree flush 已执行

## 通用租约抽取

- [x] 独立 lease.Token、Infrastructure、Runner、错误与配置
- [x] workflow 嵌入 lease.Infrastructure，Lease 保留为 Token 别名
- [x] 续租覆盖完整执行和最终提交，生命周期由 Runner 管理
- [x] 独立生命周期测试和长耗时提交集成测试通过
- [x] 全仓测试、race、vet、文档和报告更新完成

## 对外可观测事件

- [x] Step1 测试准备
- [x] Step2 Go 测试规则与既有 Observer 契约
- [x] Step3 明确事件类型、订阅与生命周期验证范围
- [x] Step4 分析异常隔离、重复回放与并发订阅风险
- [x] Step5 实现、生成测试、验证与修复；全部通过
- [x] Step6 门禁跳过；诊断包级语句覆盖率 89.7%
- [x] Step7 使用示例、兼容性说明与测试报告

## WaitAny 多路等待

- [x] Step1 准备测试
- [x] Step2 复用 Go 测试规则和持久化等待模型
- [x] Step3 范围：WaitAny、原子赢家选择、失败隔离和示例
- [x] Step4 风险：并发回调、残留请求、重复驱动、单候选异常
- [x] Step5 测试生成、验证、修复；8 个子测试及可执行示例通过
- [x] Step6 CI 门禁跳过；诊断包级语句覆盖率 89.5%
- [x] Step7 文档、完整双等待运行程序和报告；utree flush 已执行

## 统一 Signal

- [x] WaitFor/Sleep 信号描述与混合 WaitAny
- [x] 定时截止时间稳定、超时结算、赢家 Kind 及兼容旧 WaitCase
- [x] 7 个新增场景及混合示例通过，旧 WaitAny 测试保持通过
- [x] workflow/lease race 和 vet 通过；全仓 fluent 包编译状态已记录
- [x] 类型注释、使用文档和验证报告已更新

## Action 接口落地

- [x] Step1 测试准备
- [x] Step2 Go 测试规则与公开调用契约
- [x] Step3 范围：Action/ActionFn、Run/Go/Loop、示例和视频用例
- [x] Step4 空接口、typed-nil、适配与回放边界分析
- [x] Step5 测试生成、验证、修复；全部相关测试通过
- [x] Step6 CI 门禁跳过；workflow 包级语句覆盖率 90.6%
- [x] Step7 文档、迁移说明和报告；全仓 condition 编译错误单独记录

## 多层并行等待验证

- [x] Step1 测试准备
- [x] Step2 Go 规则与并行执行契约
- [x] Step3 GoFlow/RunFlow/Drive 及信号恢复范围
- [x] Step4 后代提前完成问题实测确认
- [x] Step5 先复现，再修复 flowScope 汇合；4 个入口/7 个子测试通过 20 轮 race
- [x] Step6 workflow/legacy/debugger/lease race 与 vet 通过；workflow 覆盖率 90.8%，增量门禁跳过
- [x] Step7 并行使用约束与验证报告；utree flush

## 三层调用权限

- [x] Step1 测试准备
- [x] Step2 加载 Go 规则与新层级规范
- [x] Step3 RunFlow/GoFlow/Drive 与并行等待测试范围
- [x] Step4 新契约下禁止递归；权限错误不产生任务/记录
- [x] Step5 4 个入口/12 个子测试连续 20 轮 race 通过
- [x] Step6 workflow 全目录 race 与 vet 通过；90.9% 包级覆盖率，增量门禁跳过
- [x] Step7 CALL_HIERARCHY 规范、迁移说明及报告；utree flush

## 编译期调用层级

- [x] Step1 测试准备
- [x] Step2 加载 Go 规则与上下文
- [x] Step3 独立 WorkflowContext、根定义与 Flow 方法集
- [x] Step4 类型隔离、异步汇合与迁移风险分析
- [x] Step5 实现、用例迁移、验证与修复；七种非法调用编译失败，行为测试通过
- [x] Step6 workflow 全目录 race、vet 通过；未要求覆盖率卡口，跳过
- [x] Step7 使用规范、迁移说明与报告；utree flush 已执行
