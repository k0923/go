package flow

import (
	"context"
	"fmt"
	"time"
)

type WorkflowContext[Input any, Output any] struct {
	GoContext[Input, Output]
}

func (ctx *WorkflowContext[Input, Output]) Go[T any](
	key string,
	handler func(ctx *GoContext[Input, Output]) T,
) IResult[T] {
	ensureContext(ctx.Context, nil)
	return ctx.goResult(key, handler)
}

func newWorkflowContext[Input any, Output any](
	ctx context.Context,
	infrastructure Infrastructure[Input, Output],
	input Input,
) *WorkflowContext[Input, Output] {
	return &WorkflowContext[Input, Output]{
		GoContext: *newGoContext(ctx, "", infrastructure, input),
	}
}

func (ctx *WorkflowContext[Input, Output]) setState(output Output, status FlowStatus, errors []string) {
	if err := ctx.infrastructure.SetState(ctx.Context, State[Input, Output]{
		Input:  ctx.input,
		Output: output,
		Status: status,
		Errors: errors,
	}); err != nil {
		ctx.infrastructure.OnError(err)
	}
}

func (ctx *WorkflowContext[Input, Output]) run(handler func(ctx *WorkflowContext[Input, Output]) Output, signal Signal) {
	if signal != nil {
		event, err := ctx.infrastructure.GetEvent(ctx.Context, signal.GetKey())
		if err != nil {
			ctx.infrastructure.OnError(err)
			return
		}
		if event == nil {
			return
		}
		if event.GetVersion() != signal.GetVersion() {
			// 版本不一致，忽略
			return
		}
		signalEvent, ok := event.(Signal)
		if !ok {
			ctx.infrastructure.OnError(
				NewSystemError(event, fmt.Errorf("event %s is not a signal", event.GetKey())),
			)
			return
		}
		if resolveErr := signalEvent.ResolveSignal(signal); resolveErr != nil {
			if _, ok := resolveErr.(*ConflictError); ok {
				return
			}
			ctx.infrastructure.OnError(resolveErr)
			return
		}
		newEvent, err := ctx.infrastructure.UpdateEvent(ctx.Context, signalEvent)
		if err != nil {
			ctx.infrastructure.OnError(err)
			return
		}
		if newEvent == nil {
			// 事件冲突，忽略
			return
		}
		ctx.lock.Lock()
		ctx.events[newEvent.GetKey()] = newEvent
		ctx.lock.Unlock()
	}

	result := run(func() Output {
		ensureContext(ctx.Context, nil)
		return handler(ctx)
	})

	if err := result.err; err != nil {
		if _, ok := err.(*ConflictError); ok {
			return
		}
		ctx.infrastructure.OnError(err)
		ctx.setState(result.value, FlowStatusFailed, []string{err.Error()})
		return
	}
	if signal := result.signal; signal != nil {
		return
	}
	ctx.setState(result.value, FlowStatusCompleted, nil)
}

func (ctx *WorkflowContext[Input, Output]) goResult[T any](
	key string,
	handler func(*GoContext[Input, Output]) T,
) IResult[T] {
	return newWaitedResult(func() *fixedResult[T] {
		return run(func() T {
			return ctx.executeResultEvent(
				&AsyncEvent{
					Key:       key,
					StartedAt: time.Now(),
					Status:    FlowStatusRunning,
					Version:   0,
				},
				func(executionContext StepContext[Input, Output]) (T, error) {
					goContext := newGoContext(
						executionContext.Context,
						key,
						ctx.infrastructure,
						ctx.input,
					)
					goContext.lease = executionContext.lease
					return handler(goContext), nil
				},
				nil,
			)
		})
	})
}
