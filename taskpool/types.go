package taskpool

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

var (
	// ErrInvalid rejects invalid dependencies, durations or operations.
	// ErrInvalid 表示依赖、时长或操作参数无效。
	ErrInvalid = errors.New("invalid taskpool argument")
	// ErrOwnershipLost means infrastructure no longer recognizes the action holder.
	// ErrOwnershipLost 表示基础设施不再认可该动作的当前持有者。
	ErrOwnershipLost = errors.New("taskpool operation ownership lost")
)

// Factory is the sole task source for both initialization and replenishment.
// Claim returns one task, or ok=false to retire the requesting chain permanently.
// Error means no successful claim result. Calls may overlap across slots.
// Store.Claim coordinates source consumption and slot persistence. Durable
// factories must participate in the adapter's transaction or idempotent claim
// protocol through their bound infrastructure and adapter-supplied context.
// Factory 是初始化与后续补充的唯一任务来源，Claim 返回一条任务，
// ok=false 永久结束请求链，错误表示没有成功认领结果，不同槽位可并发调用。
// Store.Claim 协调来源消费与槽位保存；持久工厂应通过绑定的基础设施和适配器
// 提供的上下文参与事务或幂等认领协议。
type Factory[T any] interface {
	Claim(context.Context) (task T, ok bool, err error)
}

// FactoryFunc adapts a function to the instance-bound Factory contract.
// FactoryFunc 将函数适配为实例绑定的 Factory，遵守相应认领契约。
type FactoryFunc[T any] func(context.Context) (T, bool, error)

// Claim requests one task from the bound source without a predecessor parameter.
// Claim 从绑定来源请求一条任务，不传前置任务参数。
func (f FactoryFunc[T]) Claim(ctx context.Context) (T, bool, error) {
	if f == nil {
		var zero T
		return zero, false, ErrInvalid
	}
	return f(ctx)
}

// Dispatcher starts or reliably hands off a business Task without a framework
// reference or completion wrapper. Business reports the task separately through
// Engine.Report, then decides whether to Drain. Returning nil without reporting
// means the runtime owns recovery of the unfinished task. Repeated delivery must
// be idempotent according to the instance-bound dispatcher's own task identity.
// ctx covers handoff only; asynchronous execution/reporting uses business contexts.
// Dispatcher 启动或可靠移交业务 Task，不传框架引用或完成包装器。
// 业务另行通过 Engine.Report 上报 Task，再决定是否 Drain。返回 nil 但尚未上报，
// 表示运行时负责未完成任务的恢复；重复投递由实例派发器按自身业务身份保证幂等。
// ctx 只覆盖接管，异步执行和上报使用业务管理的上下文。
type Dispatcher[T any] interface {
	Dispatch(context.Context, T) error
}

// DispatcherFunc adapts a function to task-only dispatch.
// DispatcherFunc 将函数适配为只接收 Task 的派发接口。
type DispatcherFunc[T any] func(context.Context, T) error

// Dispatch forwards the opaque business task to the bound function.
// Dispatch 将核心不解释的业务任务交给绑定函数。
func (f DispatcherFunc[T]) Dispatch(ctx context.Context, task T) error {
	if f == nil {
		return ErrInvalid
	}
	return f(ctx, task)
}

// Options bounds engine-side calls, independently of infrastructure ownership.
// Lease duration, renewal and retry timing belong to the Store implementation.
// Options 限定引擎调用时长，与基础设施所有权分开；租约时长、续租和重试时机由 Store 配置。
type Options struct {
	// OperationTimeout bounds claim/handoff calls. Zero uses the caller context.
	// When Store uses leases, applications must coordinate this timeout with its
	// lease/renewal configuration. Async task lifetimes belong to business.
	// OperationTimeout 限定认领或接管，零值使用调用方上下文；Store 使用租约时，
	// 业务应使超时与基础设施租约或续租配置协调，异步执行生命周期由业务管理。
	OperationTimeout time.Duration
	// CleanupTimeout bounds ownership confirmation/release after external work.
	// Zero uses five seconds, detached from request cancellation.
	// CleanupTimeout 限定外部动作后的确认或释放，零值五秒，与请求取消分离。
	CleanupTimeout time.Duration
}

// DrainReceipt counts attempted scheduling actions, not business outcomes.
// Claiming and dispatching a replacement are separate actions in separate calls.
// Reaching the limit retains all remaining work without returning a budget error.
// DrainReceipt 统计尝试的调度动作，不表示业务结果；替补认领与派发是两次调用中的
// 独立动作，达到预算后保留剩余工作，不把预算耗尽当作错误。
type DrainReceipt struct {
	// Attempted counts actions acquired within this call.
	// Attempted 统计本次领取并尝试的动作数。
	Attempted int
	// Succeeded counts successful handoffs or committed claim results.
	// Succeeded 统计成功接管或已保存认领结果的动作数。
	Succeeded int
	// Failed counts actions that encountered interface or storage errors.
	// Failed 统计遇到外部接口或存储错误的动作数。
	Failed int
}

// FactoryError marks a source callback error with no committed claim result.
// Store adapters preserve this classification only after successful rollback or
// equivalent recovery. Storage/rollback failures must use a different error, so
// Drain stops rather than treating infrastructure damage as a slot-only error.
// FactoryError 标记没有已提交认领结果的来源回调错误，Store 仅在成功回滚或恢复后
// 保留此分类；存储或回滚失败必须返回其他错误，使 Drain 停止而非视为单槽位错误。
type FactoryError struct {
	// Cause preserves the source error for errors.Is/errors.As.
	// Cause 保留来源错误供 errors.Is/errors.As 使用。
	Cause error
}

// Error describes failure to obtain a task, not its business execution result.
// Error 描述任务获取失败，不表示任务业务执行结果。
func (e *FactoryError) Error() string { return fmt.Sprintf("taskpool factory: %v", e.Cause) }

// Unwrap exposes the original source error.
// Unwrap 暴露原始来源错误。
func (e *FactoryError) Unwrap() error { return e.Cause }

// OperationError identifies the scheduling action that failed, preserving its
// underlying cause for errors.Is/errors.As. It never classifies business results.
// OperationError 定位失败调度动作，保留原因供 errors.Is/errors.As 使用，
// 不分类业务结果。
type OperationError struct {
	// Slot is an internal scheduling coordinate, not a business task identity.
	// Slot 是内部调度坐标，不是业务任务身份。
	Slot int
	// Kind identifies dispatch or replacement claim.
	// Kind 区分派发或替补认领动作。
	Kind OperationKind
	// Cause contains the interface error and any ownership cleanup error.
	// Cause 包含接口错误及可能的所有权清理错误。
	Cause error
}

// Error describes the failed operation using instance-local coordinates.
// Error 使用实例内坐标描述失败动作。
func (e *OperationError) Error() string {
	return fmt.Sprintf("taskpool %s slot=%d: %v", e.Kind, e.Slot, e.Cause)
}

// Unwrap exposes the original cause without losing error identity.
// Unwrap 暴露原始原因，保留错误身份。
func (e *OperationError) Unwrap() error { return e.Cause }

// isNil detects typed nil dependencies without inspecting business fields.
// isNil 识别接口中的类型化 nil，不检查业务字段。
func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
