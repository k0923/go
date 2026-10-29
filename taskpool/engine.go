package taskpool

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Engine orchestrates one instance through Store, Factory and Dispatcher.
// It contains no mutable runtime state, locks, channels or memory-mode branches.
// Concurrent Drain calls are coordinated by Store action ownership; business
// callbacks must support the concurrency selected by the application.
// Engine 通过 Store、Factory、Dispatcher 编排一个实例，不保存可变运行状态，
// 不包含锁、channel 或内存模式分支。并发 Drain 由 Store 的动作所有权协调，
// 业务回调应支持应用选择的并行度。
type Engine[T any] struct {
	store      Store[T]
	factory    Factory[T]
	dispatcher Dispatcher[T]
	options    Options
}

// New binds an application-provided Store, Factory and Dispatcher for one
// distributed instance. Restore a pool by binding its existing infrastructure.
// New 为一个分布式实例绑定业务提供的 Store、Factory、Dispatcher，
// 恢复任务池时绑定已有基础设施。
// Store must already contain its configured execution chains and initial claim
// intents. New validates dependencies only and never initializes storage.
// Store 应已包含业务配置的执行链与初次认领意图，New 只校验依赖，不初始化存储。
func New[T any](store Store[T], factory Factory[T], dispatcher Dispatcher[T], opts Options) (*Engine[T], error) {
	if isNil(store) || isNil(factory) || isNil(dispatcher) || opts.OperationTimeout < 0 || opts.CleanupTimeout < 0 {
		return nil, ErrInvalid
	}
	if opts.CleanupTimeout == 0 {
		opts.CleanupTimeout = 5 * time.Second
	}
	return &Engine[T]{store: store, factory: factory, dispatcher: dispatcher, options: opts}, nil
}

// State reads authoritative scheduling state without waiting or polling.
// State 主动读取权威调度状态，不等待或轮询。
func (e *Engine[T]) State(ctx context.Context) (State, error) {
	return e.store.State(ctx)
}

// Report delegates task association and atomic completion to the bound Store.
// It records one replacement intent and never invokes Factory, Dispatcher or
// Drain. Business decides whether and when to advance after reporting.
// Report 将任务关联和原子完成交给绑定的 Store，只记录一次替补意图，
// 不调用工厂、派发器或 Drain，业务决定上报后是否、何时继续推进。
func (e *Engine[T]) Report(ctx context.Context, task T) error { return e.store.Report(ctx, task) }

// Drain attempts at most limit actions and visits each slot at most once in
// this call. Synchronous Report never refills that slot within the same call;
// claim results stay pending for a later dispatch. External callback errors
// retain their actions while other slots continue; persistence/release errors
// or context cancellation stop the batch. Concurrent or reentrant Drain uses
// exactly the same Store ownership contract, without an Engine-local lock.
// Drain 最多尝试 limit 个动作，本次调用每槽位最多一次，同步 Report 不在本轮
// 补充同一槽位，认领结果留到后续调用派发。外部回调错误保留动作并继续其他槽位，
// 持久化、释放错误或上下文取消停止本轮。并发或重入 Drain 使用同一 Store
// 所有权契约，不依赖 Engine 本地锁。
func (e *Engine[T]) Drain(ctx context.Context, limit int) (DrainReceipt, error) {
	var receipt DrainReceipt
	if limit <= 0 {
		return receipt, ErrInvalid
	}
	excluded := make([]int, 0)
	var batchErr error
	for receipt.Attempted < limit {
		if err := ctx.Err(); err != nil {
			return receipt, errors.Join(batchErr, err)
		}
		acquired, err := e.store.Acquire(ctx, excluded)
		if err != nil {
			return receipt, errors.Join(batchErr, err)
		}
		if acquired == nil {
			return receipt, batchErr
		}
		d := *acquired
		excluded = append(excluded, d.Slot)
		receipt.Attempted++
		err, stop := e.process(ctx, d)
		if err == nil {
			receipt.Succeeded++
		} else {
			receipt.Failed++
			batchErr = errors.Join(batchErr, err)
		}
		if stop {
			return receipt, batchErr
		}
	}
	return receipt, batchErr
}

// process executes one exclusively acquired action. External calls may fail independently;
// persistence failures stop Drain because their commit outcome can be uncertain.
// Operation context covers only claiming or reliable handoff. Async execution
// must use an application-managed lifetime after Dispatcher returns.
// process 执行一个独占领取的动作，外部调用错误按槽位隔离；持久化结果可能不确定，
// 因此存储错误停止 Drain。操作上下文只覆盖认领或可靠接管，异步任务在派发器返回后
// 应使用业务管理的执行生命周期。
func (e *Engine[T]) process(ctx context.Context, d Delivery[T]) (error, bool) {
	operation := ctx
	if e.options.OperationTimeout > 0 {
		var cancel context.CancelFunc
		operation, cancel = context.WithTimeout(ctx, e.options.OperationTimeout)
		defer cancel()
	}
	var operationErr error
	switch d.Kind {
	case DispatchTask:
		operationErr = e.dispatch(operation, d.Task)
	case ClaimNext:
		operationErr = e.claim(operation, d)
	default:
		operationErr = ErrInvalid
	}

	// Confirm successful handoff even after request cancellation. Claim saving is
	// already coordinated by Store.Claim; this cleanup confirms or releases ownership.
	// 请求取消后仍确认成功接管，认领保存已由 Store.Claim 协调；此清理确认或释放所有权。
	cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), e.options.CleanupTimeout)
	defer cleanupCancel()
	stop := d.Kind != DispatchTask && d.Kind != ClaimNext
	if operationErr == nil && d.Kind == DispatchTask {
		operationErr = e.store.ConfirmDispatch(cleanup, d)
		stop = operationErr != nil
	}
	if operationErr != nil && d.Kind == ClaimNext {
		var factoryErr *FactoryError
		stop = !errors.As(operationErr, &factoryErr)
	}
	if operationErr != nil {
		releaseErr := e.store.Release(cleanup, d)
		return &OperationError{Slot: d.Slot, Kind: d.Kind, Cause: errors.Join(operationErr, releaseErr)}, stop || releaseErr != nil
	}
	return nil, false
}

// dispatch converts dispatcher panics into recoverable operation errors.
// dispatch 将派发器 panic 转换为可恢复的操作错误。
func (e *Engine[T]) dispatch(ctx context.Context, task T) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("dispatcher panicked: %v", value)
		}
	}()
	return e.dispatcher.Dispatch(ctx, task)
}

// claim lets Store coordinate one source claim with saving its result. The
// wrapped factory distinguishes recoverable source errors from Store failures.
// claim 由 Store 协调一次来源认领及结果保存，包装工厂以区分可恢复来源错误和存储错误。
func (e *Engine[T]) claim(ctx context.Context, d Delivery[T]) error {
	return e.store.Claim(ctx, d, FactoryFunc[T](e.next))
}

// next invokes the task source without a predecessor or synthetic initial Task.
// FactoryError indicates no task was claimed and no result should be committed.
// next 调用任务来源，不传前置任务或虚构初始 Task。
// FactoryError 表示没有认领结果可提交。
func (e *Engine[T]) next(ctx context.Context) (task T, ok bool, err error) {
	defer func() {
		if value := recover(); value != nil {
			var zero T
			task, ok, err = zero, false, &FactoryError{Cause: fmt.Errorf("factory panicked: %v", value)}
		}
	}()
	task, ok, err = e.factory.Claim(ctx)
	if err != nil {
		var zero T
		return zero, false, &FactoryError{Cause: err}
	}
	return task, ok, nil
}
