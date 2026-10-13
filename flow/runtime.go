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

type StepContext[Input any] struct {
	context.Context
	name  string
	input Input
}

func (ct StepContext[Input]) GetName() string {
	return ct.name
}

type StepOption[Input any, Result any] struct {
	Handler func(ctx StepContext[Input]) (Result, error)
}

type WaitForOption[Request any] struct {
	Request Request
}

type comparableResultEvent interface {
	ResultEvent
	comparable
}

type Runtime[Input any, Output any] struct {
	context.Context
	infrastructure Infrastructure[Input, Output]
	input          Input
	events         map[string]Event
	lock           sync.Mutex
}

func (runtime *Runtime[Input, Output]) setState(output Output, status FlowStatus, errors []string) {
	if err := runtime.infrastructure.SetState(runtime.Context, State[Input, Output]{
		Input:  runtime.input,
		Output: output,
		Status: status,
		Errors: errors,
	}); err != nil {
		runtime.infrastructure.OnError(err)
	}
}

func (runtime *Runtime[Input, Output]) run(handler func(ctx *Runtime[Input, Output]) Output, signal Signal) {
	if signal != nil {
		event, err := runtime.infrastructure.GetEvent(runtime.Context, signal.GetKey())
		if err != nil {
			runtime.infrastructure.OnError(err)
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
			runtime.infrastructure.OnError(
				NewSystemError(event, fmt.Errorf("event %s is not a signal", event.GetKey())),
			)
			return
		}
		if err := signalEvent.ResolveSignal(signal); err != nil {
			runtime.infrastructure.OnError(err)
			return
		}
		newEvent, err := runtime.infrastructure.UpdateEvent(runtime.Context, signalEvent)
		if err != nil {
			runtime.infrastructure.OnError(err)
			return
		}
		if newEvent == nil {
			// 事件冲突，忽略
			return
		}
		runtime.lock.Lock()
		runtime.events[newEvent.GetKey()] = newEvent
		runtime.lock.Unlock()
	}

	result := run(func() Output {
		return handler(runtime)
	})

	if err := result.err; err != nil {
		if _, ok := err.(*ConflictError); ok {
			return
		}
		runtime.infrastructure.OnError(err)
		runtime.setState(result.value, FlowStatusFailed, []string{err.Error()})
		return
	}
	if signal := result.signal; signal != nil {
		return
	}
	runtime.setState(result.value, FlowStatusCompleted, nil)
}

func (runtime *Runtime[Input, Output]) GetInput() Input {
	return runtime.input
}

func (runtime *Runtime[Input, Output]) Go[T any](key string, handler func(ctx *Runtime[Input, Output]) T) IResult[T] {
	return newWaitedResult(func() *fixedResult[T] {
		return run(func() T {
			return runtime.executeResultEvent(
				runtime.Context,
				&AsyncEvent{
					Key:       key,
					StartedAt: time.Now(),
					Status:    FlowStatusRunning,
					Version:   0,
				},
				func() (T, error) {
					return handler(runtime), nil
				},
				nil,
			)
		})
	})
}

func (runtime *Runtime[Input, Output]) WaitFor[Request any, Response any](ctx context.Context, key string, option WaitForOption[Request]) Response {
	waitForSignalEvent := runtime.getEvent[*WaitForSignalEvent](ctx, key)
	if waitForSignalEvent != nil {
		switch waitForSignalEvent.Status {
		case FlowStatusCompleted:
			return runtime.unmarshal[Response](waitForSignalEvent.Response, waitForSignalEvent)
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
	waitForSignalEvent.Request = runtime.marshal(option.Request, waitForSignalEvent)
	waitForSignalEvent = runtime.addEvent(ctx, waitForSignalEvent)
	panic(waitForSignalEvent)
}

func (runtime *Runtime[Input, Output]) Now(ctx context.Context, key string) time.Time {
	return runtime.checkpoint(ctx, key, func(now time.Time) time.Time {
		return now
	})
}

func (runtime *Runtime[Input, Output]) UUID(ctx context.Context, key string) string {
	return runtime.checkpoint(ctx, key, func(time.Time) string {
		return uuid.NewString()
	})
}

func (runtime *Runtime[Input, Output]) checkpoint[T any](ctx context.Context, key string, generate func(time.Time) T) T {
	checkpointEvent := runtime.getEvent[*CheckpointEvent](ctx, key)
	if checkpointEvent != nil {
		return runtime.unmarshal[T](checkpointEvent.GetResult(), checkpointEvent)
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
	checkpointEvent.SetResult(runtime.marshal(value, checkpointEvent))
	runtime.addEvent(ctx, checkpointEvent)
	return value
}

func (runtime *Runtime[Input, Output]) executeResultEvent[T any, E comparableResultEvent](
	ctx context.Context,
	newEvent E,
	handler func() (T, error),
	wrapHandlerError func(Event, error) error,
) T {
	event := runtime.getEvent[E](ctx, newEvent.GetKey())
	var zero E
	if event != zero {
		switch event.GetStatus() {
		case FlowStatusFailed:
			panic(NewLegacyBizError(event, errors.New(event.GetError())))
		case FlowStatusCompleted:
			return runtime.unmarshal[T](event.GetResult(), event)
		case FlowStatusRunning:
			panic(NewConflictError(event))
		case FlowStatusSuspended:
			event.SetStatus(FlowStatusRunning)
			event = runtime.updateEvent(ctx, event)
		default:
			panic(NewSystemError(event, fmt.Errorf("unknown status %s", event.GetStatus())))
		}
	} else {
		event = runtime.addEvent(ctx, newEvent)
	}

	handleOutput := func(output *fixedResult[T], wrapError func(Event, error) error) T {
		if output.err != nil {
			event.SetError(output.err)
			event = runtime.updateEvent(ctx, event)
			if wrapError != nil {
				panic(wrapError(event, output.err))
			}
		}
		if output.signal != nil {
			event.SetStatus(FlowStatusSuspended)
			event = runtime.updateEvent(ctx, event)
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
		event.SetResult(runtime.marshal(result, event))
		event.SetStatus(FlowStatusCompleted)
		event = runtime.updateEvent(ctx, event)
		return result
	}), nil)
}

// Step handler should only perform one atomic side effect and must not call runtime APIs.
func (runtime *Runtime[Input, Output]) Step[T any](ctx context.Context, key string, handler func(ctx StepContext[Input]) (T, error)) T {
	return runtime.executeResultEvent(
		ctx,
		&StepResultEvent{
			Key:       key,
			StartedAt: time.Now(),
			Status:    FlowStatusRunning,
			Version:   0,
		},
		func() (T, error) {
			return handler(StepContext[Input]{
				Context: ctx,
				name:    key,
				input:   runtime.input,
			})
		},
		func(event Event, err error) error {
			return NewBizError(event, err)
		},
	)
}

func (runtime *Runtime[Input, Output]) Sleep(ctx context.Context, key string, duration time.Duration) {
	until := time.Now().Add(duration)
	runtime.SleepUntil(ctx, key, until)
}

func (runtime *Runtime[Input, Output]) SleepUntil(ctx context.Context, key string, until time.Time) {
	sleepEvent := runtime.getEvent[*SleepSignalEvent](ctx, key)
	if sleepEvent == nil {
		sleepEvent = runtime.addEvent(ctx, &SleepSignalEvent{
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
			runtime.updateEvent(ctx, sleepEvent)
		}
		return
	}
	panic(sleepEvent)
}

func (runtime *Runtime[Input, Output]) unmarshal[T any](result json.RawMessage, event Event) T {
	var t T
	if err := json.Unmarshal(result, &t); err != nil {
		panic(NewUnMarshalError(event, err))
	}
	return t
}

func (runtime *Runtime[Input, Output]) marshal(data any, event Event) json.RawMessage {
	result, err := json.Marshal(data)
	if err != nil {
		panic(NewMarshalError(event, err))
	}
	return json.RawMessage(result)
}

func (runtime *Runtime[Input, Output]) convertEvent[T Event](event Event) T {
	if evt, ok := event.(T); ok {
		return evt
	} else {
		panic(NewSystemError(event, fmt.Errorf("event %s is %s, not %s", event.GetKey(), event.GetEventType(), reflect.TypeOf((*T)(nil)).Elem())))
	}
}

func (runtime *Runtime[Input, Output]) getEvent[T Event](ctx context.Context, key string) T {
	if evt, exist := runtime.events[key]; exist {
		return runtime.convertEvent[T](evt)
	}
	event, err := runtime.infrastructure.GetEvent(ctx, key)
	if err != nil {
		panic(NewInfrastructureError(nil, err, InfrastructureMethodGetEvent))
	}
	if event == nil {
		var t T
		return t
	}
	return runtime.convertEvent[T](event)
}

func (runtime *Runtime[Input, Output]) addEvent[T Event](ctx context.Context, event T) T {
	result, err := runtime.infrastructure.AddEvent(ctx, event)
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

func (runtime *Runtime[Input, Output]) updateEvent[T Event](ctx context.Context, event T) T {
	result, err := runtime.infrastructure.UpdateEvent(ctx, event)
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

func newRuntime[Input any, Output any](ctx context.Context, infrastructure Infrastructure[Input, Output], input Input) *Runtime[Input, Output] {
	return &Runtime[Input, Output]{
		Context:        ctx,
		infrastructure: infrastructure,
		input:          input,
		events:         make(map[string]Event),
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
