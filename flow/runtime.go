package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"
)

type StepContext[Input any, Output any] struct {
	context.Context
	key            string
	lease          *ExecutionLease
	infrastructure Infrastructure[Input, Output]
	input          Input
	events         map[string]Event
	lock           *sync.Mutex
}

func (ctx StepContext[Input, Output]) GetKey() string {
	return ctx.key
}

func (ctx StepContext[Input, Output]) GetInput() Input {
	return ctx.input
}

func (ctx StepContext[Input, Output]) WaitFor[Request any, Response any](
	key string,
	option WaitForOption[Request],
) Response {
	return ctx.waitFor[Request, Response](key, option)
}

func (ctx StepContext[Input, Output]) Sleep(key string, duration time.Duration) {
	ctx.sleep(key, duration)
}

func (ctx StepContext[Input, Output]) SleepUntil(key string, until time.Time) {
	ctx.sleepUntil(key, until)
}

func (ctx StepContext[Input, Output]) Now(key string) time.Time {
	return ctx.now(key)
}

func (ctx StepContext[Input, Output]) UUID(key string) string {
	return ctx.uuid(key)
}

type GoContext[Input any, Output any] struct {
	StepContext[Input, Output]
}

func (ctx *GoContext[Input, Output]) Step[T any](
	key string,
	handler func(ctx StepContext[Input, Output]) (T, error),
) T {
	return ctx.step(key, handler)
}

type WorkflowContext[Input any, Output any] struct {
	GoContext[Input, Output]
}

func (ctx *WorkflowContext[Input, Output]) Go[T any](
	key string,
	handler func(ctx *GoContext[Input, Output]) T,
) IResult[T] {
	return ctx.goResult(key, handler)
}

func newStepContext[Input any, Output any](
	ctx context.Context,
	key string,
	infrastructure Infrastructure[Input, Output],
	input Input,
) StepContext[Input, Output] {
	return StepContext[Input, Output]{
		Context:        ctx,
		key:            key,
		infrastructure: infrastructure,
		input:          input,
		events:         make(map[string]Event),
		lock:           &sync.Mutex{},
	}
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

func newWorkflowContext[Input any, Output any](
	ctx context.Context,
	infrastructure Infrastructure[Input, Output],
	input Input,
) *WorkflowContext[Input, Output] {
	return &WorkflowContext[Input, Output]{
		GoContext: *newGoContext(ctx, "", infrastructure, input),
	}
}

type StepOption[Input any, Output any, Result any] struct {
	Handler func(ctx StepContext[Input, Output]) (Result, error)
}

type WaitForOption[Request any] struct {
	Request Request
}

type comparableResultEvent interface {
	ResultEvent
	comparable
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
		if err := signalEvent.ResolveSignal(signal); err != nil {
			ctx.infrastructure.OnError(err)
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
				func() (T, error) {
					goContext := newGoContext(
						ctx.Context,
						key,
						ctx.infrastructure,
						ctx.input,
					)
					return handler(goContext), nil
				},
				nil,
			)
		})
	})
}

func (ctx StepContext[Input, Output]) waitFor[Request any, Response any](key string, option WaitForOption[Request]) Response {
	waitForSignalEvent := ctx.getEvent[*WaitForSignalEvent](key)
	if waitForSignalEvent != nil {
		switch waitForSignalEvent.Status {
		case FlowStatusCompleted:
			return ctx.unmarshal[Response](waitForSignalEvent.Response, waitForSignalEvent)
		case FlowStatusFailed:
			panic(NewLegacyBizError(waitForSignalEvent, fmt.Errorf("wait for signal %s failed", key)))
		case FlowStatusRunning:
			panic(NewConflictError(waitForSignalEvent))
		case FlowStatusSuspended:
			panic(NewConflictError(waitForSignalEvent))
		default:
			panic(NewSystemError(waitForSignalEvent, fmt.Errorf("unknown status %s", waitForSignalEvent.Status)))
		}
	}
	waitForSignalEvent = &WaitForSignalEvent{
		Key:       key,
		Status:    FlowStatusSuspended,
		StartedAt: time.Now(),
		Version:   0,
	}
	waitForSignalEvent.Request = ctx.marshal(option.Request, waitForSignalEvent)
	waitForSignalEvent = ctx.addEvent(waitForSignalEvent)
	panic(waitForSignalEvent)
}

func (ctx StepContext[Input, Output]) now(key string) time.Time {
	return ctx.checkpoint(key, func(now time.Time) time.Time {
		return now
	})
}

func (ctx StepContext[Input, Output]) uuid(key string) string {
	return ctx.checkpoint(key, func(time.Time) string {
		return uuid.NewString()
	})
}

func (ctx StepContext[Input, Output]) checkpoint[T any](key string, generate func(time.Time) T) T {
	checkpointEvent := ctx.getEvent[*CheckpointEvent](key)
	if checkpointEvent != nil {
		return ctx.unmarshal[T](checkpointEvent.GetResult(), checkpointEvent)
	}

	now := time.Now()
	value := generate(now)
	checkpointEvent = &CheckpointEvent{
		Key:       key,
		StartedAt: now,
		EndedAt:   now,
		Status:    FlowStatusCompleted,
		Version:   0,
	}
	checkpointEvent.SetResult(ctx.marshal(value, checkpointEvent))
	ctx.addEvent(checkpointEvent)
	return value
}

func (ctx StepContext[Input, Output]) executeResultEvent[T any, E comparableResultEvent](
	newEvent E,
	handler func() (T, error),
	wrapHandlerError func(Event, error) error,
) T {
	event := ctx.getEvent[E](newEvent.GetKey())
	var zero E
	if event != zero {
		switch event.GetStatus() {
		case FlowStatusFailed:
			panic(NewLegacyBizError(event, errors.New(event.GetError())))
		case FlowStatusCompleted:
			return ctx.unmarshal[T](event.GetResult(), event)
		case FlowStatusRunning:
			panic(NewConflictError(event))
		case FlowStatusSuspended:
			event.SetStatus(FlowStatusRunning)
			event = ctx.updateEvent(event)
		default:
			panic(NewSystemError(event, fmt.Errorf("unknown status %s", event.GetStatus())))
		}
	} else {
		event = ctx.addEvent(newEvent)
	}

	handleOutput := func(output *fixedResult[T], wrapError func(Event, error) error) T {
		if output.err != nil {
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

	result := handleOutput(run(func() T {
		result, err := handler()
		if err != nil {
			panic(err)
		}
		return result
	}), wrapHandlerError)

	return handleOutput(run(func() T {
		event.SetResult(ctx.marshal(result, event))
		event.SetStatus(FlowStatusCompleted)
		event = ctx.updateEvent(event)
		return result
	}), nil)
}

func (ctx StepContext[Input, Output]) step[T any](
	key string,
	handler func(ctx StepContext[Input, Output]) (T, error),
) T {
	return ctx.executeResultEvent(
		&StepResultEvent{
			Key:       key,
			StartedAt: time.Now(),
			Status:    FlowStatusRunning,
			Version:   0,
		},
		func() (T, error) {
			stepContext := ctx
			stepContext.key = key
			return handler(stepContext)
		},
		func(event Event, err error) error {
			return NewBizError(event, err)
		},
	)
}

func (ctx StepContext[Input, Output]) sleep(key string, duration time.Duration) {
	until := time.Now().Add(duration)
	ctx.sleepUntil(key, until)
}

func (ctx StepContext[Input, Output]) sleepUntil(key string, until time.Time) {
	sleepEvent := ctx.getEvent[*SleepSignalEvent](key)
	if sleepEvent == nil {
		sleepEvent = ctx.addEvent(&SleepSignalEvent{
			Key:       key,
			StartedAt: time.Now(),
			Status:    FlowStatusSuspended,
			Version:   0,
			Until:     until,
		})
	}
	duration := time.Until(sleepEvent.Until)
	if duration <= 0 {
		if sleepEvent.Status != FlowStatusCompleted {
			sleepEvent.Status = FlowStatusCompleted
			sleepEvent.EndedAt = time.Now()
			ctx.updateEvent(sleepEvent)
		}
		return
	}
	panic(sleepEvent)
}

func (ctx StepContext[Input, Output]) unmarshal[T any](result json.RawMessage, event Event) T {
	var t T
	if err := json.Unmarshal(result, &t); err != nil {
		panic(NewUnMarshalError(event, err))
	}
	return t
}

func (ctx StepContext[Input, Output]) marshal(data any, event Event) json.RawMessage {
	result, err := json.Marshal(data)
	if err != nil {
		panic(NewMarshalError(event, err))
	}
	return json.RawMessage(result)
}

func (ctx StepContext[Input, Output]) convertEvent[T Event](event Event) T {
	if evt, ok := event.(T); ok {
		return evt
	} else {
		panic(NewSystemError(event, fmt.Errorf("event %s is %s, not %s", event.GetKey(), event.GetEventType(), reflect.TypeOf((*T)(nil)).Elem())))
	}
}

func (ctx StepContext[Input, Output]) getEvent[T Event](key string) T {
	ctx.lock.Lock()
	cached, exists := ctx.events[key]
	ctx.lock.Unlock()
	if exists {
		return ctx.convertEvent[T](cached)
	}
	event, err := ctx.infrastructure.GetEvent(ctx.Context, key)
	if err != nil {
		panic(NewInfrastructureError(nil, err, InfrastructureMethodGetEvent))
	}
	if event == nil {
		var t T
		return t
	}
	return ctx.convertEvent[T](event)
}

func (ctx StepContext[Input, Output]) addEvent[T Event](event T) T {
	result, err := ctx.infrastructure.AddEvent(ctx.Context, event)
	if err != nil {
		panic(NewInfrastructureError(event, err, InfrastructureMethodAddEvent))
	}
	if result == nil {
		panic(NewConflictError(event))
	}
	if evt, ok := result.(T); ok {
		return evt
	} else {
		panic(NewSystemError(event, fmt.Errorf("event %s is %s, not %s", result, event.GetEventType(), reflect.TypeOf((*T)(nil)).Elem())))
	}
}

func (ctx StepContext[Input, Output]) updateEvent[T Event](event T) T {
	result, err := ctx.infrastructure.UpdateEvent(ctx.Context, event)
	if err != nil {
		panic(NewInfrastructureError(event, err, InfrastructureMethodUpdateEvent))
	}
	if result == nil {
		panic(NewConflictError(event))
	}
	if evt, ok := result.(T); ok {
		return evt
	} else {
		panic(NewSystemError(event, fmt.Errorf("event %s is %s, not %s", result, event.GetEventType(), reflect.TypeOf((*T)(nil)).Elem())))
	}
}

func run[Output any](handler func() Output) (result *fixedResult[Output]) {
	result = &fixedResult[Output]{}
	defer func() {
		recoverResult := recover()
		if recoverResult == nil {
			return
		}
		switch output := recoverResult.(type) {
		case error:
			result.err = output
			return
		case Signal:
			result.signal = output
			return
		default:
			result.err = NewUnknownError(output)
			return
		}
	}()
	result.value = handler()
	return
}
