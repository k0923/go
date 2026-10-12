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

type runtime[Input any, Output any] struct {
	infrastructure Infrastructure
}

func (runtime *runtime[Input, Output]) runBranch(ctx context.Context, input Input, branches []string, handler func(ctx *FlowContext[Input, Output])) (result any) {
	if handler == nil {
		return nil
	}

	defer func() {
		output := recover()
		if output != nil {
			result = output
			return
		}
	}()

	flowCtx := newBranchContext(ctx, input, runtime, branches)
	handler(flowCtx)
	return
}

func (runtime *runtime[Input, Output]) Parallel(ctx context.Context, input Input, branches []string, name string, branchList ...Branch[Input, Output]) {
	if len(branchList) == 0 {
		return
	}
	var branchSignal BranchSignal = make(BranchSignal)
	for _, branch := range branchList {
		if branch.Key == "" {
			panic(NewBizError("parallel branch key is required", ErrBranchKeyRequired))
		}
		if _, ok := branchSignal[branch.Key]; ok {
			panic(NewBizError(fmt.Sprintf("parallel branch key %s must be unique", branch.Key), ErrDuplicateBranchKey))
		}
		branchSignal[branch.Key] = nil
	}
	lock := sync.Mutex{}
	wg := sync.WaitGroup{}
	wg.Add(len(branchList))
	for index, branch := range branchList {
		go func(index int, branch Branch[Input, Output]) {
			newBranches := append([]string(nil), branches...)
			newBranches = append(newBranches, name, branch.Key)
			result := runtime.runBranch(ctx, input, newBranches, branch.Handler)
			lock.Lock()
			branchSignal[branch.Key] = result
			lock.Unlock()
			wg.Done()
		}(index, branch)
	}
	wg.Wait()
	for _, result := range branchSignal {
		if result == nil {
			continue
		}
		switch result.(type) {
		case error, Signal:
			panic(branchSignal)
		}
	}
}

func (runtime *runtime[Input, Output]) WaitFor[Request any, Response any](ctx *FlowContext[Input, Output], branches []string, key string, option WaitForOption[Request]) Response {
	waitForSignalEvent := runtime.getEvent[*WaitForSignalEvent](ctx, branches, key)
	if waitForSignalEvent != nil {
		switch waitForSignalEvent.Status {
		case FlowStatusFinished:
			return runtime.unmarshal[Response](waitForSignalEvent.Response)
		case FlowStatusFailed:
			panic(NewSystemError(fmt.Sprintf("wait for signal %s failed", key)))
		case FlowStatusRunning:
			panic(waitForSignalEvent.BuildSignal())
		default:
			panic(NewSystemError(fmt.Sprintf("wait for signal %s status %s is not supported", key, waitForSignalEvent.Status)))
		}
	}
	waitForSignalEvent = runtime.addEvent(ctx, &WaitForSignalEvent{
		Key:       key,
		Status:    FlowStatusRunning,
		StartedAt: time.Now(),
		Version:   0,
		Branches:  append([]string(nil), branches...),
		Request:   runtime.marshal(option.Request),
	})
	signal := waitForSignalEvent.BuildSignal()
	runtime.raiseSignal(ctx, signal)
	panic(signal)
}

func (runtime *runtime[Input, Output]) Now(ctx *FlowContext[Input, Output], branches []string, name string) time.Time {
	checkpointEvent := runtime.getEvent[*CheckpointEvent](ctx, branches, name)
	if checkpointEvent != nil {
		return runtime.unmarshal[time.Time](checkpointEvent.Data)
	}
	now := time.Now()
	checkpointEvent = &CheckpointEvent{
		Data:      runtime.marshal(now),
		Key:       name,
		StartedAt: now,
		EndedAt:   now,
		Status:    FlowStatusFinished,
		Version:   0,
		Branches:  append([]string(nil), branches...),
	}
	runtime.addEvent(ctx, checkpointEvent)
	return now
}

func (runtime *runtime[Input, Output]) UUID(ctx *FlowContext[Input, Output], branches []string, key string) string {
	checkpointEvent := runtime.getEvent[*CheckpointEvent](ctx, branches, key)
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
		Status:    FlowStatusFinished,
		Version:   0,
		Branches:  append([]string(nil), branches...),
	}
	runtime.addEvent(ctx, checkpointEvent)
	return value
}

func (runtime *runtime[Input, Output]) Step[Result any](ctx context.Context, branches []string, key string, handler func(ctx StepContext) (Result, error)) Result {
	stepEvent := runtime.getEvent[*StepResultEvent](ctx, branches, key)
	if stepEvent != nil {
		if stepEvent.Status == FlowStatusFailed {
			panic(NewBizError(fmt.Sprintf("step %s failed:%s", key, stepEvent.Error)))
		}
		if stepEvent.Status == FlowStatusFinished {
			return runtime.unmarshal[Result](stepEvent.Result)
		}
		// todo handle running status
	}
	stepEvent = &StepResultEvent{
		Key:       key,
		StartedAt: time.Now(),
		Status:    FlowStatusRunning,
		Version:   0,
		Branches:  append([]string(nil), branches...),
	}

	stepEvent = runtime.addEvent(ctx, stepEvent)
	stepContext := StepContext{
		Context: ctx,
		name:    key,
	}
	result, err := handler(stepContext)
	stepEvent.EndedAt = time.Now()

	if err == nil {
		stepEvent.Result = runtime.marshal(result)
		stepEvent.Status = FlowStatusFinished
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

func (runtime *runtime[Input, Output]) Sleep(ctx *FlowContext[Input, Output], branches []string, name string, duration time.Duration) {
	until := time.Now().Add(duration)
	runtime.SleepUntil(ctx, branches, name, until)
}

func (runtime *runtime[Input, Output]) SleepUntil(ctx *FlowContext[Input, Output], branches []string, key string, until time.Time) {
	sleepEvent := runtime.getEvent[*SleepEvent](ctx, branches, key)
	if sleepEvent == nil {
		sleepEvent = runtime.addEvent(ctx, &SleepEvent{
			Key:       key,
			Branches:  append([]string(nil), branches...),
			StartedAt: time.Now(),
			Status:    FlowStatusRunning,
			Version:   0,
			Until:     until,
		})
	}
	duration := time.Until(sleepEvent.Until)
	if duration <= 0 {
		if sleepEvent.Status == FlowStatusRunning {
			sleepEvent.Status = FlowStatusFinished
			sleepEvent.EndedAt = time.Now()
			runtime.updateEvent(ctx, sleepEvent)
		}
		return
	}

	signal := sleepEvent.BuildSignal()
	runtime.raiseSignal(ctx, signal)
	panic(signal)
}

func (runtime *runtime[Input, Output]) unmarshal[T any](result json.RawMessage) T {
	var t T
	if err := json.Unmarshal(result, &t); err != nil {
		panic(NewBizError(fmt.Sprintf("fail to unmarshal result %s", err.Error()), err))
	}
	return t
}

func (runtime *runtime[Input, Output]) marshal(data any) json.RawMessage {
	result, err := json.Marshal(data)
	if err != nil {
		panic(NewBizError(fmt.Sprintf("fail to marshal data %s", err.Error()), err))
	}
	return json.RawMessage(result)
}

func (runtime *runtime[Input, Output]) getEvent[T Event](ctx context.Context, branches []string, key string) T {
	event, err := runtime.infrastructure.GetEvent(ctx, branches, key)
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

func (runtime *runtime[Input, Output]) addEvent[T Event](ctx context.Context, event T) T {
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

func (runtime *runtime[Input, Output]) updateEvent[T Event](ctx context.Context, event T) T {
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

func (runtime *runtime[Input, Output]) raiseSignal(ctx context.Context, signal Signal) {
	if err := runtime.infrastructure.RaiseSignal(ctx, signal); err != nil {
		panic(NewInfrastructureError(fmt.Sprintf("fail to raise signal %s", signal.GetKey()), err))
	}
}
