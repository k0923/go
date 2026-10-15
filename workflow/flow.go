package workflow

import (
	"context"
	"errors"
	"fmt"
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
	lease := NewLease(
		flowCtx.api,
		event.GetKey(),
		executionLeaseTTL,
		func(executionContext context.Context) {
			output = handler(executionContext)
		},
	)
	err := lease.Run(flowCtx.Context)
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
			event.SetStatus(FlowStatusSuspended)
			event = flowCtx.updateEvent(event)
		}
		return output.Wait()
	}

	output := handleOutput(run(func() Output {
		ensureContext(flowCtx.Context, event)
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
