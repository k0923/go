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
	lease          *Lease
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
	ensureContext(ctx.Context, nil)
	return ctx.waitFor[Request, Response](key, option)
}

func (ctx StepContext[Input, Output]) Sleep(key string, duration time.Duration) {
	ensureContext(ctx.Context, nil)
	ctx.sleep(key, duration)
}

func (ctx StepContext[Input, Output]) SleepUntil(key string, until time.Time) {
	ensureContext(ctx.Context, nil)
	ctx.sleepUntil(key, until)
}

func (ctx StepContext[Input, Output]) Now(key string) time.Time {
	ensureContext(ctx.Context, nil)
	return ctx.now(key)
}

func (ctx StepContext[Input, Output]) UUID(key string) string {
	ensureContext(ctx.Context, nil)
	return ctx.uuid(key)
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

func (ctx StepContext[Input, Output]) withLease[T any](
	event Event,
	handler func(StepContext[Input, Output]) T,
) T {
	if ctx.lease != nil {
		return handler(ctx)
	}

	var output T
	var lease *Lease
	lease = NewLease(
		ctx.infrastructure,
		event.GetKey(),
		executionLeaseTTL,
		func(executionContext context.Context) {
			ctx.Context = executionContext
			ctx.lease = lease
			output = handler(ctx)
		},
	)
	err := lease.Run(ctx.Context)
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
	infrastructureErr := NewInfrastructureError(
		event,
		leaseErr.Cause,
		InfrastructureMethod(leaseErr.Operation),
	)
	ctx.infrastructure.OnError(infrastructureErr)
	if leaseErr.Operation == LeaseOperationClaim {
		panic(infrastructureErr)
	}
	panic(NewConflictError(event))
}

func (ctx StepContext[Input, Output]) executeResultEvent[T any, E comparableResultEvent](
	newEvent E,
	handler func(StepContext[Input, Output]) (T, error),
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
		case FlowStatusRunning, FlowStatusSuspended:
		default:
			panic(NewSystemError(event, fmt.Errorf("unknown status %s", event.GetStatus())))
		}
	}

	return ctx.withLease(newEvent, func(executionContext StepContext[Input, Output]) T {
		return executionContext.executeLeasedResultEvent(
			newEvent,
			handler,
			wrapHandlerError,
		)
	})
}

func (ctx StepContext[Input, Output]) executeLeasedResultEvent[T any, E comparableResultEvent](
	newEvent E,
	handler func(StepContext[Input, Output]) (T, error),
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

	result := handleOutput(run(func() T {
		ensureContext(ctx.Context, event)
		result, err := handler(ctx)
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
		func(executionContext StepContext[Input, Output]) (T, error) {
			stepContext := executionContext
			stepContext.key = key
			return handler(stepContext)
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
