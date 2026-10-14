package flow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type GoContext[Input any, Output any] struct {
	StepContext[Input, Output]
}

func (ctx *GoContext[Input, Output]) Step[T any](
	key string,
	handler func(ctx StepContext[Input, Output]) (T, error),
) T {
	ensureContext(ctx.Context, nil)
	return ctx.step(key, handler)
}

func (ctx *GoContext[Input, Output]) Loop[T any](
	key string,
	option LoopOption,
	handler func(ctx *GoContext[Input, Output], attempt int) (LoopResult[T], error),
) T {
	ensureContext(ctx.Context, nil)
	return ctx.loop(key, option, handler)
}

func newGoContext[Input any, Output any](
	ctx context.Context,
	key string,
	infrastructure Infrastructure[Input, Output],
	input Input,
) *GoContext[Input, Output] {
	return &GoContext[Input, Output]{
		StepContext: newStepContext(ctx, key, infrastructure, input),
	}
}

type LoopOption struct {
	Interval    time.Duration
	MaxAttempts int
}

type LoopResult[T any] struct {
	Result T
	Done   bool
}

func (ctx *GoContext[Input, Output]) loop[T any](
	key string,
	option LoopOption,
	handler func(ctx *GoContext[Input, Output], attempt int) (LoopResult[T], error),
) T {
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

	event := ctx.getEvent[*LoopEvent](key)
	if event != nil {
		switch event.GetStatus() {
		case FlowStatusFailed:
			panic(NewLegacyBizError(event, errors.New(event.GetError())))
		case FlowStatusCompleted:
			return ctx.unmarshal[T](event.GetResult(), event)
		case FlowStatusRunning, FlowStatusSuspended:
		default:
			panic(NewSystemError(event, fmt.Errorf("unknown status %s", event.GetStatus())))
		}
	}

	return ctx.withLease(newEvent, func(executionContext StepContext[Input, Output]) T {
		goContext := &GoContext[Input, Output]{
			StepContext: executionContext,
		}
		return goContext.executeLeasedLoop(
			newEvent,
			option,
			handler,
		)
	})
}

func (ctx *GoContext[Input, Output]) executeLeasedLoop[T any](
	newEvent *LoopEvent,
	option LoopOption,
	handler func(ctx *GoContext[Input, Output], attempt int) (LoopResult[T], error),
) T {
	event := ctx.getEvent[*LoopEvent](newEvent.GetKey())
	if event != nil {
		switch event.GetStatus() {
		case FlowStatusFailed:
			panic(NewLegacyBizError(event, errors.New(event.GetError())))
		case FlowStatusCompleted:
			return ctx.unmarshal[T](event.GetResult(), event)
		case FlowStatusRunning:
		case FlowStatusSuspended:
			event.SetStatus(FlowStatusRunning)
			event = ctx.updateEvent(event)
		default:
			panic(NewSystemError(event, fmt.Errorf("unknown status %s", event.GetStatus())))
		}
	} else {
		event = ctx.addEvent(newEvent)
	}

	if event.Attempt >= option.MaxAttempts {
		err := fmt.Errorf("loop max attempts exceeded: %d", option.MaxAttempts)
		event.SetError(err)
		event = ctx.updateEvent(event)
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
		output *fixedResult[LoopResult[T]],
		wrapError func(Event, error) error,
	) LoopResult[T] {
		if output.err != nil {
			output.err = bindContextError(event, output.err)
			event.SetError(output.err)
			event = ctx.updateEvent(event)
			if wrapError != nil {
				panic(wrapError(event, output.err))
			}
		}
		if output.signal != nil {
			event.SetStatus(FlowStatusSuspended)
			event = ctx.updateEvent(event)
		}
		return output.Wait()
	}

	loopContext := &GoContext[Input, Output]{
		StepContext: ctx.StepContext,
	}
	loopContext.key = event.GetKey()
	output := handleOutput(run(func() LoopResult[T] {
		ensureContext(ctx.Context, event)
		result, err := handler(loopContext, event.Attempt)
		if err != nil {
			panic(err)
		}
		return result
	}), wrapLoopError)

	if !output.Done && event.Attempt+1 >= option.MaxAttempts {
		err := fmt.Errorf("loop max attempts exceeded: %d", option.MaxAttempts)
		event.SetError(err)
		event = ctx.updateEvent(event)
		panic(NewBizError(event, err))
	}

	return handleOutput(run(func() LoopResult[T] {
		if output.Done {
			event.SetResult(ctx.marshal(output.Result, event))
			event.SetStatus(FlowStatusCompleted)
			event = ctx.updateEvent(event)
			return output
		}

		nextAttempt := event.Attempt + 1
		event.Attempt = nextAttempt
		event.SetStatus(FlowStatusSuspended)
		event = ctx.updateEvent(event)
		ctx.sleep(loopSleepKey(event.GetKey(), event.Attempt), option.Interval)
		return output
	}), nil).Result
}

func loopSleepKey(key string, attempt int) string {
	return fmt.Sprintf("%s/sleep/%d", key, attempt)
}
