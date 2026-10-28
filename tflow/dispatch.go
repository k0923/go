package tflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DispatchPending leases durable work and performs external side effects after
// commit. A delivery error preserves its command. Retry may repeat a side effect;
// dispatchers/executors must honor stable execution and event idempotency keys.
// DispatchPending 租约式领取持久 Command，并在事务外执行外部副作用。
// 派发失败会保留 Command，后续重试可能重复触发副作用，使用方必须幂等。
func (e *Engine) DispatchPending(ctx context.Context, limit int, dispatcher Dispatcher) (int, error) {
	receipt, err := e.DispatchBatch(ctx, limit, dispatcher)
	return receipt.Succeeded, err
}

// DispatchBatch budgets claims, not successes. Failed commands are released with
// backoff and other claimable work continues. Cleanup failure stops the batch.
// Error means some work failed; inspect the receipt before retrying.
func (e *Engine) DispatchBatch(ctx context.Context, limit int, dispatcher Dispatcher) (DispatchReceipt, error) {
	var receipt DispatchReceipt
	if limit <= 0 {
		return receipt, ErrInvalid
	}
	var batchErr error
	for receipt.Claimed < limit {
		if err := ctx.Err(); err != nil {
			return receipt, errors.Join(batchErr, err)
		}
		d, err := e.store.ClaimCommand(ctx, e.options.DeliveryLease)
		if err != nil {
			return receipt, errors.Join(batchErr, err)
		}
		if d == nil {
			return receipt, batchErr
		}
		receipt.Claimed++
		command := d.Command
		started := time.Now()
		dispatchCtx, cancel := context.WithTimeout(ctx, e.options.DeliveryTimeout)
		err = e.deliver(dispatchCtx, command, dispatcher)
		cancel()
		// A canceled request must not strand the command until the next lease expiry.
		cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), e.options.CleanupTimeout)
		if err != nil {
			receipt.Failed++
			delay, policyErr := e.retryDelay(*d, err)
			releaseErr := e.store.ReleaseCommand(cleanup, *d, delay, err.Error())
			cleanupCancel()
			deliveryErr := fmt.Errorf("deliver %s: %w", command.ID, err)
			batchErr = errors.Join(batchErr, deliveryErr, policyErr, releaseErr)
			e.observe(ctx, Observation{Operation: "delivery", CommandID: command.ID, Kind: command.Kind, Duration: time.Since(started), Error: errors.Join(err, releaseErr)})
			if releaseErr != nil {
				return receipt, batchErr
			}
			continue
		}
		ackErr := e.store.AckCommand(cleanup, *d)
		cleanupCancel()
		e.observe(ctx, Observation{Operation: "delivery", CommandID: command.ID, Kind: command.Kind, Duration: time.Since(started), Error: ackErr})
		if ackErr != nil {
			receipt.Failed++
			return receipt, errors.Join(batchErr, ackErr)
		}
		receipt.Succeeded++
	}
	return receipt, batchErr
}

func (e *Engine) deliver(ctx context.Context, command Command, dispatcher Dispatcher) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("tflow delivery panicked: %v", value)
		}
	}()
	command, skip, err := e.prepareCommand(ctx, command)
	if err != nil || skip {
		return err
	}
	if command.Kind == ReconcileMain {
		return e.Reconcile(ctx)
	}
	if command.Kind == MainTerminated && IsNil(dispatcher) {
		return e.Handle(ctx, command, nil)
	}
	if IsNil(dispatcher) {
		return ErrDispatcherUnavailable
	}
	return dispatcher.Dispatch(ctx, command)
}

// Handle consumes a trusted persisted Command after transport, or directly in
// process. It validates execution identity at the receiving boundary, calls the
// executor and merges outcomes. It does not Ack a broker or Store delivery.
// Obsolete start/resume commands are successful no-ops. Retransmission can still
// repeat IO: the executor must be idempotent and handle cancellation races.
//
// executor may be nil for reconciliation/terminal notification. MainTerminated
// uses the historical Task's TerminationHandler, falling back to the Engine's handler.
// Handle 消费一条可信的持久 Command。同步执行和 MQ/RPC 消费都应走这里，
// 以复用执行身份校验、过期指令跳过、Executor 调用和结果归并。Handle
// 不负责 broker ack 或 Store delivery ack。
func (e *Engine) Handle(ctx context.Context, command Command, executor Executor) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("tflow command handling panicked: %v", value)
		}
	}()
	command, skip, err := e.prepareCommand(ctx, command)
	if err != nil || skip {
		return err
	}
	switch command.Kind {
	case ReconcileMain:
		return e.Reconcile(ctx)
	case MainTerminated:
		event := MainTermination{ID: command.ID, Main: command.Main.MainProgress}
		if handler, ok := command.Main.Task.(TerminationHandler); ok {
			return handler.OnTerminated(ctx, event)
		}
		if IsNil(e.options.TerminationHandler) {
			return nil
		}
		return e.options.TerminationHandler.OnTerminated(ctx, event)
	}
	if IsNil(executor) {
		return ErrExecutorUnavailable
	}
	var result DispatchResult
	switch command.Kind {
	case StartSub:
		result, err = executor.Start(ctx, command.Sub.Task, command.Sub.Execution(e.MainID()))
	case ResumeSub:
		result, err = executor.Resume(ctx, command.Sub.Task, command.Sub.Execution(e.MainID()), command.Event)
	case CancelSub:
		return executor.Cancel(ctx, command.Sub.Task, command.Sub.Execution(e.MainID()))
	}
	if err != nil {
		return err
	}
	return e.applyResult(ctx, command.Sub, result)
}

func (e *Engine) prepareCommand(ctx context.Context, command Command) (Command, bool, error) {
	if err := ctx.Err(); err != nil {
		return command, false, err
	}
	if command.MainID != e.MainID() || command.ID == "" {
		return command, false, ErrInvalid
	}
	m, err := e.store.GetMain(ctx)
	if err != nil {
		return command, false, err
	}
	switch command.Kind {
	case ReconcileMain:
		return command, false, nil
	case MainTerminated:
		if ValidateMain(command.Main) != nil || !command.Main.Status.Terminal() ||
			command.ID != TerminationCommand(e.MainID(), command.Main).ID ||
			command.Main.Version > m.Version {
			return command, false, ErrInvalid
		}
		return command, false, nil
	case StartSub, ResumeSub, CancelSub:
		if ValidateSub(command.Sub) != nil {
			return command, false, ErrInvalid
		}
		if command.Kind == ResumeSub {
			if IsNil(command.Event) || command.EventKey == "" || command.Event.Key() != command.EventKey {
				return command, false, ErrInvalid
			}
		}
		if command.ID != NewCommand(command.Kind, e.MainID(), command.Sub, command.Event).ID {
			return command, false, ErrInvalid
		}
		current, err := e.store.GetSub(ctx, command.Sub.Key)
		if err != nil {
			return command, false, err
		}
		if command.Kind == CancelSub {
			if command.Sub.Status != SubCanceled || command.Sub.Attempt > current.Attempt ||
				(command.Sub.Attempt == current.Attempt && current.Status != SubCanceled) {
				return command, false, ErrInvalid
			}
			// Preserve the old task/ref even if the current sub has been rerun.
			return command, false, nil
		}
		if m.Status.Terminal() || current.Status != SubRunning || current.Attempt != command.Sub.Attempt {
			return command, true, nil
		}
		command.Sub = current
		return command, false, nil
	default:
		return command, false, ErrInvalid
	}
}

// ExecutorDispatcher adapts an arbitrary function/state machine/remote engine.
// It does not recursively drain commands when a synchronous executor completes.
type ExecutorDispatcher struct {
	engine   *Engine
	executor Executor
}

func NewExecutorDispatcher(e *Engine, executor Executor) (*ExecutorDispatcher, error) {
	if e == nil || IsNil(executor) {
		return nil, ErrInvalid
	}
	return &ExecutorDispatcher{engine: e, executor: executor}, nil
}

// Drain delivers up to limit commands using the Engine's configured delivery
// adapter. A zero count means none were claimable now, not necessarily complete.
// Drain 使用构造时的 Executor/Dispatcher 推进命令；返回零不代表业务已完成。
func (e *Engine) Drain(ctx context.Context, limit int) (int, error) {
	return e.DispatchPending(ctx, limit, e.options.Dispatcher)
}

func (d *ExecutorDispatcher) Dispatch(ctx context.Context, command Command) error {
	return d.engine.Handle(ctx, command, d.executor)
}

func (e *Engine) applyResult(ctx context.Context, observed SubRecord, result DispatchResult) error {
	switch result.Status {
	case Accepted, Waiting, ExecutionSucceeded, ExecutionFailed:
	default:
		return ErrInvalid
	}
	if result.ExternalRef != "" {
		err := e.store.WithTx(ctx, func(tx Tx) error {
			sub, err := tx.Sub(observed.Key)
			if err != nil {
				return err
			}
			m, err := tx.Main()
			if err != nil {
				return err
			}
			// Acceptance can arrive after cancellation/rerun. Preserve a historical
			// cancellation intent instead of attaching the old run to the new attempt.
			if sub.Attempt != observed.Attempt || sub.Status == SubCanceled || m.Status == MainCanceled {
				old := observed
				old.ExternalRef = result.ExternalRef
				old.Status = SubCanceled
				return tx.PutCommand(NewCommand(CancelSub, e.MainID(), old, nil))
			}
			if sub.ExternalRef != "" && sub.ExternalRef != result.ExternalRef {
				return ErrConflict
			}
			if sub.ExternalRef == result.ExternalRef {
				return nil
			}
			version := sub.Version
			sub.ExternalRef, sub.UpdatedAt = result.ExternalRef, e.options.Clock.Now()
			sub.Version++
			return tx.SaveSub(version, sub.SubState)
		})
		if err != nil {
			return err
		}
	}
	if result.Status == Accepted || result.Status == Waiting {
		return nil
	}
	status := SubSucceeded
	if result.Status == ExecutionFailed {
		status = SubFailed
	}
	_, err := e.report(ctx, Report{
		Execution: observed.Execution(e.MainID()), Status: status, Error: result.Error,
	})
	if errors.Is(err, ErrStaleAttempt) || errors.Is(err, ErrAlreadyTerminal) {
		return nil
	}
	if err != nil {
		return err
	}
	return e.Reconcile(ctx)
}
