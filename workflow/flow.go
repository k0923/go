package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type LoopOutput interface {
	Done() bool
}

type FlowRunOption struct {
}

type Flow[Input any, Output any] interface {
	Run(ctx *FlowContext, input Input) Output
}

type FlowContext struct {
	ActionContext
}

func newFlowContext(
	ctx context.Context,
	key string,
	api Infrastructure,
) *FlowContext {
	return &FlowContext{
		ActionContext: *newActionContext(ctx, key, api),
	}
}

func (flowCtx *FlowContext) Go[Input any, Output any](
	key string,
	action Action[Input, Output],
	input Input,
) IResult[Output] {
	ensureContext(flowCtx.ActionContext.Context, nil)
	return newWaitedResult(func() *fixedResult[Output] {
		return run(func() Output {
			return flowCtx.Run(key, action, input, ActionRunOption{})
		})
	})
}

func (flowCtx *FlowContext) Run[Input any, Output any](
	key string,
	action Action[Input, Output],
	input Input,
	option ActionRunOption,
) Output {
	ensureContext(flowCtx.ActionContext.Context, nil)
	key = flowCtx.apiKey(key)
	return flowCtx.executeResultEvent(
		&StepResultEvent{
			Key:       key,
			StartedAt: time.Now(),
			Status:    FlowStatusRunning,
			Version:   0,
		},
		func(executionContext context.Context) (Output, error) {
			actionContext := newActionContext(
				executionContext,
				key,
				flowCtx.api,
			)
			return action.Run(actionContext, input), nil
		},
		func(event Event, err error) error {
			var contextErr *ContextError
			if errors.As(err, &contextErr) {
				return err
			}
			return NewBizError(event, err)
		},
	)
}

func (flowCtx *FlowContext) Loop[Input any, Output LoopOutput](
	key string,
	action Action[Input, Output],
	input Input,
	option LoopRunOption,
) Output {
	ensureContext(flowCtx.ActionContext.Context, nil)
	key = flowCtx.apiKey(key)
	if option.Interval < minLoopInterval {
		panic(NewSystemError(nil, fmt.Errorf("loop interval must be at least %s", minLoopInterval)))
	}
	if option.MaxAttempts <= 0 {
		panic(NewSystemError(nil, fmt.Errorf("loop max attempts must be positive")))
	}

	newEvent := &LoopEvent{
		Key:       key,
		StartedAt: time.Now(),
		Status:    FlowStatusRunning,
		Version:   0,
	}

	event := flowCtx.getEvent[*LoopEvent](key)
	if event != nil {
		switch event.GetStatus() {
		case FlowStatusFailed:
			panic(NewLegacyBizError(event, errors.New(event.GetError())))
		case FlowStatusCompleted:
			return flowCtx.unmarshal[Output](event.GetResult(), event)
		case FlowStatusRunning, FlowStatusSuspended:
		default:
			panic(NewSystemError(event, fmt.Errorf("unknown status %s", event.GetStatus())))
		}
	}

	return flowCtx.withLease(newEvent, func(executionContext context.Context) Output {
		executionFlowContext := *flowCtx
		executionFlowContext.Context = executionContext
		return executionFlowContext.executeLeasedLoop(
			newEvent,
			action,
			input,
			option,
		)
	})
}

func (flowCtx *FlowContext) withLease[T any](
	event Event,
	handler func(context.Context) T,
) T {
	var output T
	slot := &suspendSlot{}
	lease := NewLease(
		&atomicReleaseInfrastructure{
			LeaseInfrastructure: flowCtx.api,
			api:                 flowCtx.api,
			slot:                slot,
		},
		event.GetKey(),
		executionLeaseTTL,
		func(executionContext context.Context) {
			output = handler(executionContext)
		},
	)
	err := lease.Run(withSuspendSlot(flowCtx.Context, slot))
	if err == nil {
		return output
	}
	if errors.Is(err, ErrLeaseConflict) {
		panic(NewConflictError(event))
	}

	var leaseErr *LeaseError
	if !errors.As(err, &leaseErr) {
		panic(NewSystemError(event, err))
	}
	apiErr := NewInfrastructureError(
		event,
		leaseErr.Cause,
		InfrastructureMethod(leaseErr.Operation),
	)
	flowCtx.api.OnError(apiErr)
	if leaseErr.Operation == LeaseOperationClaim {
		panic(apiErr)
	}
	panic(NewConflictError(event))
}

// suspendSlot 暂存本次被租约保护的执行中待发布的挂起信号，
// 供租约释放时并入同一个事务。
type suspendSlot struct {
	mu     sync.Mutex
	signal Signal
}

func (slot *suspendSlot) stage(signal Signal) {
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.signal == nil {
		slot.signal = signal
	}
}

func (slot *suspendSlot) staged() Signal {
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.signal
}

type suspendSlotContextKey struct{}

func withSuspendSlot(ctx context.Context, slot *suspendSlot) context.Context {
	return context.WithValue(ctx, suspendSlotContextKey{}, slot)
}

// stageSuspend 在执行单元挂起时登记信号。
func stageSuspend(ctx context.Context, signal Signal) {
	if slot, ok := ctx.Value(suspendSlotContextKey{}).(*suspendSlot); ok && slot != nil {
		slot.stage(signal)
	}
}

// isSuspendSignal 判断信号是否为框架可挂起提交的信号。
// 业务自行 panic 的其他 Signal 不参与挂起提交。
func isSuspendSignal(signal Signal) bool {
	switch signal.(type) {
	case *WaitForSignalEvent, *SleepSignalEvent:
		return true
	default:
		return false
	}
}

// atomicReleaseInfrastructure 是交给 Lease 的租约基础设施，对 Lease 完全透明：
// Lease 仍只调用 ReleaseLease 与 ClaimLease/RenewLease。若本次执行暂存了挂起信号，
// 这里把「持久化信号 + 释放租约」并入同一个事务，保证信号对外可见与租约释放同时生效。
type atomicReleaseInfrastructure struct {
	LeaseInfrastructure
	api  Infrastructure
	slot *suspendSlot
}

func (infrastructure *atomicReleaseInfrastructure) ReleaseLease(
	ctx context.Context,
	lease ExecutionLease,
) (bool, error) {
	signal := infrastructure.slot.staged()
	if signal == nil {
		return infrastructure.LeaseInfrastructure.ReleaseLease(ctx, lease)
	}
	err := infrastructure.api.Transaction(ctx, func(tx Infrastructure) error {
		if err := tx.Suspend(ctx, signal); err != nil {
			return err
		}
		released, err := tx.ReleaseLease(ctx, lease)
		if err != nil {
			return err
		}
		if !released {
			return ErrLeaseConflict
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrLeaseConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (flowCtx *FlowContext) executeResultEvent[T any, E comparableResultEvent](
	newEvent E,
	handler func(context.Context) (T, error),
	wrapHandlerError func(Event, error) error,
) T {
	event := flowCtx.getEvent[E](newEvent.GetKey())
	var zero E
	if event != zero {
		switch event.GetStatus() {
		case FlowStatusFailed:
			panic(NewLegacyBizError(event, errors.New(event.GetError())))
		case FlowStatusCompleted:
			return flowCtx.unmarshal[T](event.GetResult(), event)
		case FlowStatusRunning, FlowStatusSuspended:
		default:
			panic(NewSystemError(event, fmt.Errorf("unknown status %s", event.GetStatus())))
		}
	}

	return flowCtx.withLease(newEvent, func(executionContext context.Context) T {
		executionFlowContext := *flowCtx
		executionFlowContext.Context = executionContext
		return executionFlowContext.executeLeasedResultEvent(
			newEvent,
			handler,
			wrapHandlerError,
		)
	})
}

func (flowCtx *FlowContext) executeLeasedResultEvent[T any, E comparableResultEvent](
	newEvent E,
	handler func(context.Context) (T, error),
	wrapHandlerError func(Event, error) error,
) T {
	event := flowCtx.getEvent[E](newEvent.GetKey())
	var zero E
	if event != zero {
		switch event.GetStatus() {
		case FlowStatusFailed:
			panic(NewLegacyBizError(event, errors.New(event.GetError())))
		case FlowStatusCompleted:
			return flowCtx.unmarshal[T](event.GetResult(), event)
		case FlowStatusRunning:
		case FlowStatusSuspended:
			event.SetStatus(FlowStatusRunning)
			event = flowCtx.updateEvent(event)
		default:
			panic(NewSystemError(event, fmt.Errorf("unknown status %s", event.GetStatus())))
		}
	} else {
		event = flowCtx.addEvent(newEvent)
	}

	handleOutput := func(output *fixedResult[T], wrapError func(Event, error) error) T {
		if output.err != nil {
			output.err = bindContextError(event, output.err)
			event.SetError(output.err)
			event = flowCtx.updateEvent(event)
			if wrapError != nil {
				panic(wrapError(event, output.err))
			}
		}
		if output.signal != nil {
			if isSuspendSignal(output.signal) {
				stageSuspend(flowCtx.Context, output.signal)
			}
			event.SetStatus(FlowStatusSuspended)
			event = flowCtx.updateEvent(event)
		}
		return output.Wait()
	}

	result := handleOutput(run(func() T {
		ensureContext(flowCtx.Context, event)
		result, err := handler(flowCtx.Context)
		if err != nil {
			panic(err)
		}
		return result
	}), wrapHandlerError)

	return handleOutput(run(func() T {
		event.SetResult(flowCtx.marshal(result, event))
		event.SetStatus(FlowStatusCompleted)
		event = flowCtx.updateEvent(event)
		return result
	}), nil)
}

// awaitAttemptInterval 确认进入本次 attempt 之前，上一个 attempt 留下的等待间隔已经到期。
// 只检查「已存在」的 sleep 事件：不存在说明本次 attempt 不是由间隔等待进入的
// （例如首次进入，或上一次 attempt 内部因 WaitFor 挂起），直接放行。
// 存在且未到期时重新抛出该 sleep 信号：本次驱动不推进 attempt、也不执行 action，
// 从而确保 Loop 只在自己的间隔到期时前进，不会被其它 Loop 或 WaitFor 的恢复推动。
func (flowCtx *FlowContext) awaitAttemptInterval(event *LoopEvent) {
	sleepEvent := flowCtx.getEvent[*SleepSignalEvent](
		fmt.Sprintf("%s|sleep|%d", event.GetKey(), event.Attempt),
	)
	if sleepEvent == nil {
		return
	}
	if time.Until(sleepEvent.Until) <= 0 {
		if sleepEvent.Status != FlowStatusCompleted {
			sleepEvent.Status = FlowStatusCompleted
			sleepEvent.EndedAt = time.Now()
			flowCtx.updateEvent(sleepEvent)
		}
		return
	}
	panic(sleepEvent)
}

func (flowCtx *FlowContext) executeLeasedLoop[Input any, Output LoopOutput](
	newEvent *LoopEvent,
	action Action[Input, Output],
	input Input,
	option LoopRunOption,
) Output {
	event := flowCtx.getEvent[*LoopEvent](newEvent.GetKey())
	if event != nil {
		switch event.GetStatus() {
		case FlowStatusFailed:
			panic(NewLegacyBizError(event, errors.New(event.GetError())))
		case FlowStatusCompleted:
			return flowCtx.unmarshal[Output](event.GetResult(), event)
		case FlowStatusRunning:
		case FlowStatusSuspended:
			event.SetStatus(FlowStatusRunning)
			event = flowCtx.updateEvent(event)
		default:
			panic(NewSystemError(event, fmt.Errorf("unknown status %s", event.GetStatus())))
		}
	} else {
		event = flowCtx.addEvent(newEvent)
	}

	if event.Attempt >= option.MaxAttempts {
		err := fmt.Errorf("loop max attempts exceeded: %d", option.MaxAttempts)
		event.SetError(err)
		event = flowCtx.updateEvent(event)
		panic(NewBizError(event, err))
	}

	wrapLoopError := func(event Event, err error) error {
		var contextErr *ContextError
		if errors.As(err, &contextErr) {
			return err
		}
		return NewBizError(event, err)
	}
	handleOutput := func(
		output *fixedResult[Output],
		wrapError func(Event, error) error,
	) Output {
		if output.err != nil {
			output.err = bindContextError(event, output.err)
			event.SetError(output.err)
			event = flowCtx.updateEvent(event)
			if wrapError != nil {
				panic(wrapError(event, output.err))
			}
		}
		if output.signal != nil {
			if isSuspendSignal(output.signal) {
				stageSuspend(flowCtx.Context, output.signal)
			}
			event.SetStatus(FlowStatusSuspended)
			event = flowCtx.updateEvent(event)
		}
		return output.Wait()
	}

	output := handleOutput(run(func() Output {
		ensureContext(flowCtx.Context, event)
		// 只在本次 attempt 的等待间隔到期后才推进：其它 Loop 的 sleep 到期、
		// WaitFor 恢复等与本次 attempt 无关的驱动，都不会推进本 Loop。
		flowCtx.awaitAttemptInterval(event)
		actionContext := newActionContext(
			flowCtx.Context,
			fmt.Sprintf("%s|%d", event.GetKey(), event.Attempt),
			flowCtx.api,
		)
		return action.Run(actionContext, input)
	}), wrapLoopError)

	if !output.Done() && event.Attempt+1 >= option.MaxAttempts {
		err := fmt.Errorf("loop max attempts exceeded: %d", option.MaxAttempts)
		event.SetError(err)
		event = flowCtx.updateEvent(event)
		panic(NewBizError(event, err))
	}

	return handleOutput(run(func() Output {
		if output.Done() {
			event.SetResult(flowCtx.marshal(output, event))
			event.SetStatus(FlowStatusCompleted)
			event = flowCtx.updateEvent(event)
			return output
		}

		event.Attempt++
		event.SetStatus(FlowStatusSuspended)
		event = flowCtx.updateEvent(event)
		flowCtx.sleep(
			fmt.Sprintf("%s|sleep|%d", event.GetKey(), event.Attempt),
			option.Interval,
		)
		return output
	}), nil)
}
