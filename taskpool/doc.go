// Package taskpool orchestrates a factory-fed task window through one Store
// interface. Engine contains only Store, Factory, Dispatcher and immutable
// options, without locks, channels or runtime-mode branches.
// taskpool 通过同一 Store 接口编排由工厂供给的任务窗口，Engine 只保存 Store、Factory、
// Dispatcher 和固定配置，不包含锁、channel 或运行模式分支。
//
// Applications create a Store with configured execution chains and initial claim
// intents. New(store, factory, dispatcher, options) only binds that prepared
// instance; initialization, duplicate creation and restoration belong to the
// adapter. A prepared instance with zero chains is complete.
// Factory.Claim(ctx) is the only task source for initial and replacement tasks;
// it has no completed-task parameter and never receives a synthetic zero Task.
// 业务创建包含已配置执行链与初次认领意图的 Store，New(store, factory, dispatcher,
// options) 只绑定这个已准备实例，初始化、重复创建与恢复属于适配器，零执行链表示完成。
// Factory.Claim(ctx) 是初次和补充任务的唯一来源，不接收已完成任务参数，
// 也不会收到虚构的零值 Task。
//
// Drain(ctx, limit) visits each slot at most once in that call. A first Drain
// claims tasks, a later Drain dispatches them. Report(ctx, task) only records
// completion and one subsequent claim intent. Business decides whether and when
// to continue; a synchronous report cannot auto-refill within the current call.
// Factory ok=false permanently retires the requesting chain, not a temporary pause.
// Drain(ctx, limit) 本次调用每槽位最多一次，首次 Drain 认领，后续 Drain 派发。
// Report(ctx, task) 只记录完成和一次后续认领意图，业务决定是否、何时继续；
// 同步上报不在本轮自动补充。工厂 ok=false 永久结束请求链，不表示暂时暂停。
//
// Store.Claim coordinates source consumption and saving a task/dispatch intent
// or retiring an empty chain. Initial claims and replenishment share the same
// recovery contract. Durable adapters use a shared transaction or their own
// idempotent source protocol; simply dequeuing then saving in separate steps
// cannot recover a crash between those steps. Adapter context may supply source
// transaction scope. FactoryError is returned only for a source callback failure
// with a recoverable/rolled-back claim; storage failures stop the Drain batch.
// Store.Claim 协调来源消费和任务及派发意图保存，或结束没有任务的执行链，初次与
// 补充认领使用同一恢复契约。持久适配器使用共享事务或自身的幂等来源协议，
// 普通出队后分开保存无法恢复两步之间的宕机；适配器上下文可携带来源事务范围。
// FactoryError 只用于来源回调失败且认领已回滚或可恢复的情况，存储失败停止本轮。
//
// Store.Acquire owns one slot until confirmation, release or infrastructure
// expiry. Report during dispatch preserves that ownership before a replacement
// becomes eligible. Concurrent Engines/Drain coordinate entirely through Store.
// Delivery tokens are opaque infrastructure receipts, not task references or
// business identities. Tasks require no Key or comparability.
// Store.Acquire 独占槽位直到确认、释放或基础设施控制的到期，派发期间 Report 保持
// 独占，所有权结束后才使替补可处理。并发 Engine 或 Drain 完全通过 Store 协调。
// Delivery 令牌只是基础设施回执，不是任务引用或业务身份，Task 不要求 Key 或可比较。
//
// All retired chains mean completion, exposed by State.Completed(). The core
// does not wait, poll or supply completion channels; applications can query State
// or integrate completion into their own infrastructure. Callback failures retain
// work while other slots continue. Business owns cancellation, execution results,
// retries, task association, routing, encoding and recovery workers. No in-memory
// Store, background goroutines or global registry are provided.
// 全部链结束后整体完成，通过 State.Completed() 查询。核心不等待、不轮询或提供
// 完成 channel，业务可主动查询 State 或在自身基础设施处理完成。回调失败保留动作
// 并继续其他槽位，业务负责取消、执行结果、业务重试、任务关联、路由、编码及恢复
// worker。库不提供内存 Store、后台 goroutine 或全局注册表。
package taskpool
