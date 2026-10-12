package flow

import (
	"context"
	"encoding/json"
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

type GoResult[Input any, Output any] struct {
	result      Output
	isCompleted bool
	resultChan  chan Output
	runtime     *runtime[Input]
	key         string
}

func (gr *GoResult[Input, Output]) Wait() Output {
	if gr.isCompleted {
		return gr.result
	}
	gr.result = <-gr.resultChan
	gr.isCompleted = true
	return gr.result
}

type runtime[Input any] struct {
	context.Context
	infrastructure Infrastructure
	input          Input
	lock           sync.Mutex
	events         map[string]Event
}

func (runtime *runtime[Input]) Go[Output any](key string, handler func(ctx *runtime[Input]) Output) IResult[Output] {
	checkpointEvent := runtime.getEvent[*CheckpointEvent](runtime.Context, key)
	if checkpointEvent != nil {
		switch checkpointEvent.Status {
		case FlowStatusFailed:
			return newFixedResult[Output](func() any {
				return NewBizError("flow is suspended", nil)
			})
		case FlowStatusCompleted:
			return newFixedResult[Output](func() any {
				return runtime.unmarshal[Output](checkpointEvent.Data)
			})
		case FlowStatusRunning:
		case FlowStatusSuspended:
		}
	}

	waitedResult := newWaitedResult[Output](func() Output {
		checkpointEvent = runtime.addEvent(runtime.Context, &CheckpointEvent{
			Key:       key,
			StartedAt: time.Now(),
			Version:   0,
			Status:    FlowStatusRunning,
		})
		output := handler(runtime)
		checkpointEvent.Data = runtime.marshal(output)
		checkpointEvent.Status = FlowStatusCompleted
		checkpointEvent.EndedAt = time.Now()
		// todo 需要补一个出错的处理
		runtime.updateEvent(runtime.Context, checkpointEvent)
		return output
	})

	return waitedResult
}

func (runtime *runtime[Input]) WaitFor[Request any, Response any](ctx context.Context, key string, option WaitForOption[Request]) Response {
	waitForSignalEvent := runtime.getEvent[*WaitForSignalEvent](ctx, key)
	if waitForSignalEvent != nil {
		switch waitForSignalEvent.Status {
		case FlowStatusCompleted:
			return runtime.unmarshal[Response](waitForSignalEvent.Response)
		case FlowStatusFailed:
			panic(NewSystemError(fmt.Sprintf("wait for signal %s failed", key)))
		case FlowStatusRunning:
			panic(waitForSignalEvent.BuildSignal())
		case FlowStatusSuspended:
			panic(NewBizError("flow is suspended", nil))
		default:
			panic(NewSystemError(fmt.Sprintf("wait for signal %s status %s is not supported", key, waitForSignalEvent.Status)))
		}
	}
	waitForSignalEvent = runtime.addEvent(ctx, &WaitForSignalEvent{
		Key:       key,
		Status:    FlowStatusRunning,
		StartedAt: time.Now(),
		Version:   0,
		Request:   runtime.marshal(option.Request),
	})
	signal := waitForSignalEvent.BuildSignal()
	runtime.raiseSignal(ctx, signal)
	panic(signal)
}

func (runtime *runtime[Input]) Now(ctx context.Context, key string) time.Time {
	checkpointEvent := runtime.getEvent[*CheckpointEvent](ctx, key)
	if checkpointEvent != nil {
		return runtime.unmarshal[time.Time](checkpointEvent.Data)
	}
	now := time.Now()
	checkpointEvent = &CheckpointEvent{
		Data:      runtime.marshal(now),
		Key:       key,
		StartedAt: now,
		EndedAt:   now,
		Status:    FlowStatusCompleted,
		Version:   0,
	}
	runtime.addEvent(ctx, checkpointEvent)
	return now
}

func (runtime *runtime[Input]) UUID(ctx context.Context, key string) string {
	checkpointEvent := runtime.getEvent[*CheckpointEvent](ctx, key)
	if checkpointEvent != nil {
		return runtime.unmarshal[string](checkpointEvent.Data)
	}
	now := time.Now()
	value := uuid.NewString()
	checkpointEvent = &CheckpointEvent{
		Data:      runtime.marshal(value),
		Key:       key,
		StartedAt: now,
		EndedAt:   now,
		Status:    FlowStatusCompleted,
		Version:   0,
	}
	runtime.addEvent(ctx, checkpointEvent)
	return value
}

func (runtime *runtime[Input]) Step[Result any](ctx context.Context, key string, handler func(ctx StepContext[Input]) (Result, error)) Result {
	stepEvent := runtime.getEvent[*StepResultEvent](ctx, key)
	if stepEvent != nil {
		if stepEvent.Status == FlowStatusFailed {
			panic(NewBizError(fmt.Sprintf("step %s failed:%s", key, stepEvent.Error)))
		}
		if stepEvent.Status == FlowStatusCompleted {
			return runtime.unmarshal[Result](stepEvent.Result)
		}
		// todo handle running status
	}
	stepEvent = &StepResultEvent{
		Key:       key,
		StartedAt: time.Now(),
		Status:    FlowStatusRunning,
		Version:   0,
	}

	stepEvent = runtime.addEvent(ctx, stepEvent)
	stepContext := StepContext[Input]{
		Context: ctx,
		name:    key,
		input:   runtime.input,
	}
	result, err := handler(stepContext)
	stepEvent.EndedAt = time.Now()

	if err == nil {
		stepEvent.Result = runtime.marshal(result)
		stepEvent.Status = FlowStatusCompleted
	} else {
		stepEvent.Error = err.Error()
		stepEvent.Status = FlowStatusFailed
	}

	runtime.updateEvent(ctx, stepEvent)
	if err != nil {
		panic(NewBizError(fmt.Sprintf("step %s failed", key), err))
	} else {
		return result
	}
}

func (runtime *runtime[Input]) Sleep(ctx context.Context, key string, duration time.Duration) {
	until := time.Now().Add(duration)
	runtime.SleepUntil(ctx, key, until)
}

func (runtime *runtime[Input]) SleepUntil(ctx context.Context, key string, until time.Time) {
	sleepEvent := runtime.getEvent[*SleepEvent](ctx, key)
	if sleepEvent == nil {
		sleepEvent = runtime.addEvent(ctx, &SleepEvent{
			Key:       key,
			StartedAt: time.Now(),
			Status:    FlowStatusRunning,
			Version:   0,
			Until:     until,
		})
	}
	duration := time.Until(sleepEvent.Until)
	if duration <= 0 {
		if sleepEvent.Status == FlowStatusRunning {
			sleepEvent.Status = FlowStatusCompleted
			sleepEvent.EndedAt = time.Now()
			runtime.updateEvent(ctx, sleepEvent)
		}
		return
	}

	signal := sleepEvent.BuildSignal()
	runtime.raiseSignal(ctx, signal)
	panic(signal)
}

func (runtime *runtime[Input]) unmarshal[T any](result json.RawMessage) T {
	var t T
	if err := json.Unmarshal(result, &t); err != nil {
		panic(NewBizError(fmt.Sprintf("fail to unmarshal result %s", err.Error()), err))
	}
	return t
}

func (runtime *runtime[Input]) marshal(data any) json.RawMessage {
	result, err := json.Marshal(data)
	if err != nil {
		panic(NewBizError(fmt.Sprintf("fail to marshal data %s", err.Error()), err))
	}
	return json.RawMessage(result)
}

func (runtime *runtime[Input]) getEventKey(event Event) string {
	return fmt.Sprintf("%s:%s", event.GetEventType(), event.GetKey())
}

func (runtime *runtime[Input]) getEvent[T Event](ctx context.Context, key string) T {
	event, err := runtime.infrastructure.GetEvent(ctx, key)
	if err != nil {
		panic(NewInfrastructureError(fmt.Sprintf("fail to get event %s", key), err))
	}
	if event == nil {
		var t T
		return t
	}
	if evt, ok := event.(T); ok {
		return evt
	} else {
		panic(NewBizError(fmt.Sprintf("event %s is %s, not %s", key, event.GetEventType(), reflect.TypeOf((*T)(nil)).Elem())))
	}
}

func (runtime *runtime[Input]) addEvent[T Event](ctx context.Context, event T) T {
	result, err := runtime.infrastructure.AddEvent(ctx, event)
	if err != nil {
		panic(NewInfrastructureError(fmt.Sprintf("fail to add event %s", event.GetKey()), err))
	}
	if result == nil {
		panic(ErrConflict)
	}
	if evt, ok := result.(T); ok {
		return evt
	} else {
		panic(NewBizError(fmt.Sprintf("event %s is %s, not %s", result, event.GetEventType(), reflect.TypeOf((*T)(nil)).Elem())))
	}
}

func (runtime *runtime[Input]) updateEvent[T Event](ctx context.Context, event T) T {
	result, err := runtime.infrastructure.UpdateEvent(ctx, event)
	if err != nil {
		panic(NewInfrastructureError(fmt.Sprintf("fail to update event %s", event.GetKey()), err))
	}
	if result == nil {
		panic(ErrConflict)
	}
	if evt, ok := result.(T); ok {
		return evt
	} else {
		panic(NewBizError(fmt.Sprintf("event %s is %s, not %s", result, event.GetEventType(), reflect.TypeOf((*T)(nil)).Elem())))
	}
}

func (runtime *runtime[Input]) raiseSignal(ctx context.Context, signal Signal) {
	if err := runtime.infrastructure.RaiseSignal(ctx, signal); err != nil {
		panic(NewInfrastructureError(fmt.Sprintf("fail to raise signal %s", signal.GetKey()), err))
	}
}
