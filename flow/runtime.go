package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

type runtime[Input any, Output any] struct {
	infrastructure eventAppender
	event          *eventTree
	lock           sync.Mutex
	signalChan     chan Signal
}

func (runtime *runtime[Input, Output]) Parallel(ctx context.Context, input Input, branches []string, name string, branchList ...Branch[Input, Output]) {
	if len(branchList) == 0 {
		return
	}
	branchKeys := make(map[string]struct{}, len(branchList))
	for _, branch := range branchList {
		if branch.Key == "" {
			panic(NewBizError("parallel branch key is required", ErrBranchKeyRequired))
		}
		if _, ok := branchKeys[branch.Key]; ok {
			panic(NewBizError(fmt.Sprintf("parallel branch key %s must be unique", branch.Key), ErrDuplicateBranchKey))
		}
		branchKeys[branch.Key] = struct{}{}
	}
	type branchResult struct {
		index  int
		signal Signal
		err    any
	}

	results := make(chan branchResult, len(branchList))
	for index, branch := range branchList {
		go func(index int, branch Branch[Input, Output]) {
			result := branchResult{
				index: index,
			}
			defer func() {
				if recovered := recover(); recovered != nil {
					if signal, ok := recovered.(Signal); ok {
						result.signal = signal
					} else {
						result.err = recovered
					}
				}
				results <- result
			}()
			newBranches := append(append([]string(nil), branches...), name, branch.Key)
			ctx := newBranchContext(ctx, input, runtime, newBranches)
			branch.Handler(ctx)
		}(index, branch)
	}

	branchResults := make([]branchResult, len(branchList))
	for range branchList {
		result := <-results
		branchResults[result.index] = result
	}
	for _, result := range branchResults {
		if result.err != nil {
			panic(result.err)
		}
	}
	for _, result := range branchResults {
		if result.signal != nil {
			panic(result.signal)
		}
	}
}

func (runtime *runtime[Input, Output]) WaitFor[Request any, Response any](ctx *FlowContext[Input, Output], branches []string, name string, option WaitForOption[Request]) Response {
	waitForResultEvent := runtime.getEvent[*WaitForResultEvent](branches, name)
	if waitForResultEvent != nil {
		return runtime.unmarshal[Response](waitForResultEvent.Response)
	}
	waitForEvent := runtime.getEvent[*WaitForEvent](branches, name)
	if waitForEvent == nil {
		waitForEvent = &WaitForEvent{
			Name:     name,
			Ts:       time.Now(),
			Request:  runtime.marshal(option.Request),
			Branches: append([]string(nil), branches...),
		}
		runtime.appendEvent(ctx, waitForEvent)
	}
	runtime.raiseSignal(WaitForSignal{
		Name:     name,
		Branches: waitForEvent.Branches,
		Request:  runtime.marshal(option.Request),
	})
	var defaultResponse Response
	return defaultResponse
}

func (runtime *runtime[Input, Output]) Now(ctx *FlowContext[Input, Output], branches []string, name string) time.Time {
	checkpointEvent := runtime.getEvent[*CheckpointEvent](branches, name)
	if checkpointEvent != nil {
		return runtime.unmarshal[time.Time](checkpointEvent.Data)
	}
	now := time.Now()
	checkpointEvent = &CheckpointEvent{
		Data:     runtime.marshal(now),
		Name:     name,
		Ts:       now,
		Branches: append([]string(nil), branches...),
	}
	runtime.appendEvent(ctx, checkpointEvent)
	return now
}

func (runtime *runtime[Input, Output]) UUID(ctx *FlowContext[Input, Output], branches []string, name string) string {
	checkpointEvent := runtime.getEvent[*CheckpointEvent](branches, name)
	if checkpointEvent != nil {
		return runtime.unmarshal[string](checkpointEvent.Data)
	}
	now := time.Now()
	value := uuid.NewString()
	checkpointEvent = &CheckpointEvent{
		Data:     runtime.marshal(value),
		Name:     name,
		Ts:       now,
		Branches: append([]string(nil), branches...),
	}
	runtime.appendEvent(ctx, checkpointEvent)
	return value
}

func (runtime *runtime[Input, Output]) Step[Result any](ctx *FlowContext[Input, Output], branches []string, name string, handler func(ctx StepContext) (Result, error)) Result {
	stepEndEvent := runtime.getEvent[*StepEndEvent](branches, name)
	if stepEndEvent != nil {
		return runtime.unmarshal[Result](stepEndEvent.Result)
	}
	stepFailedEvent := runtime.getEvent[*StepFailedEvent](branches, name)
	if stepFailedEvent != nil {
		panic(NewBizError(fmt.Sprintf("step %s failed: %s", name, stepFailedEvent.Error)))
	}

	stepStartEvent := runtime.getEvent[*StepStartEvent](branches, name)
	if stepStartEvent != nil {
		runtime.raiseSignal(conflictEventSignal{
			Event: stepStartEvent,
		})
	}

	startEvent := &StepStartEvent{
		Name:     name,
		Ts:       time.Now(),
		Branches: append([]string(nil), branches...),
	}

	runtime.appendEvent(ctx, startEvent)

	stepContext := StepContext{
		Context: ctx,
		name:    name,
	}

	result, err := handler(stepContext)
	if err == nil {
		stepEndEvent := &StepEndEvent{
			Name:     name,
			Ts:       time.Now(),
			Result:   runtime.marshal(result),
			Branches: append([]string(nil), branches...),
		}
		runtime.appendEvent(ctx, stepEndEvent)
		return result
	}
	stepFailEvent := &StepFailedEvent{
		Error:    err.Error(),
		Name:     name,
		Ts:       time.Now(),
		Branches: append([]string(nil), branches...),
	}
	runtime.appendEvent(ctx, stepFailEvent)
	panic(NewBizError(fmt.Sprintf("step %s failed", name), err))
}

func (runtime *runtime[Input, Output]) Sleep(ctx *FlowContext[Input, Output], branches []string, name string, duration time.Duration) {
	until := time.Now().Add(duration)
	runtime.SleepUntil(ctx, branches, name, until)
}

func (runtime *runtime[Input, Output]) SleepUntil(ctx *FlowContext[Input, Output], branches []string, name string, until time.Time) {
	sleepEvent := runtime.getEvent[*SleepEvent](branches, name)
	if sleepEvent == nil {
		sleepEvent = &SleepEvent{
			Name:     name,
			Ts:       time.Now(),
			Until:    until,
			Branches: append([]string(nil), branches...),
		}
		runtime.appendEvent(ctx, sleepEvent)
	}
	duration := time.Until(sleepEvent.Until)
	if duration <= 0 {
		return
	}
	runtime.raiseSignal(SleepSignal{
		Name:     name,
		Duration: duration,
		Branches: sleepEvent.Branches,
	})
}

func (runtime *runtime[Input, Output]) Close() {
	close(runtime.signalChan)
}

func (runtime *runtime[Input, Output]) GetSignal() chan Signal {
	return runtime.signalChan
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

func (runtime *runtime[Input, Output]) getEvent[T Event](branches []string, name string) T {
	runtime.lock.Lock()
	defer runtime.lock.Unlock()
	return runtime.event.GetEvent[T](branches, name)
}

func (runtime *runtime[Input, Output]) AppendEvent(ctx context.Context, event Event) (bool, error) {
	runtime.lock.Lock()
	defer runtime.lock.Unlock()
	expectedIndex := runtime.event.EventCount(event.GetBranches())
	accepted, err := runtime.infrastructure.AppendEvent(ctx, event, expectedIndex)
	if err != nil || !accepted {
		return accepted, err
	}
	runtime.event.AddEvent(event)
	return true, nil
}

func (runtime *runtime[Input, Output]) appendEvent(ctx context.Context, event Event) {
	accepted, err := runtime.AppendEvent(ctx, event)
	if err != nil {
		panic(NewInfrastructureError(fmt.Sprintf("fail to append step event %s", event.GetName()), err))
	}
	if !accepted {
		runtime.raiseSignal(conflictEventSignal{
			Event: event,
		})
	}
}

func (runtime *runtime[Input, Output]) raiseSignal(signal Signal) {
	runtime.signalChan <- signal
	panic(signal)
}

type eventTree struct {
	children map[string]*eventTree
	events   map[string]Event
}

func (tree *eventTree) AddEvent(event Event) error {
	current := tree
	for _, branch := range event.GetBranches() {
		if current.children == nil {
			current.children = make(map[string]*eventTree)
		}
		if current.children[branch] == nil {
			current.children[branch] = &eventTree{}
		}
		current = current.children[branch]
	}
	if current.events == nil {
		current.events = make(map[string]Event)
	}
	eventKey := fmt.Sprintf("%s:%s", event.GetEventType(), event.GetName())
	if _, ok := current.events[eventKey]; ok {
		return fmt.Errorf("event %s:%s already exists", event.GetEventType(), event.GetName())
	}
	current.events[eventKey] = event
	return nil
}

func (tree *eventTree) EventCount(branches []string) int {
	current := tree.GetTree(branches)
	if current == nil {
		return 0
	}
	return len(current.events)
}

func (tree *eventTree) GetTree(branches []string) *eventTree {
	current := tree
	for _, branch := range branches {
		if current == nil {
			return nil
		}
		current = current.children[branch]
	}
	return current
}

func (tree *eventTree) GetEvent[T Event](branches []string, name string) T {
	var defaultValue T
	current := tree.GetTree(branches)
	if current == nil {
		return defaultValue
	}
	for _, event := range current.events {
		if event.GetName() != name {
			continue
		}
		if matched, ok := event.(T); ok {
			return matched
		}
	}
	return defaultValue
}

func newEventTree(events []Event) (*eventTree, error) {
	tree := &eventTree{}
	for _, event := range events {
		if err := tree.AddEvent(event); err != nil {
			return nil, err
		}
	}
	return tree, nil
}

func newRuntime[Input any, Output any](infrastructure eventAppender, events []Event) (*runtime[Input, Output], error) {
	tree, err := newEventTree(events)
	if err != nil {
		return nil, err
	}
	runtime := &runtime[Input, Output]{
		infrastructure: infrastructure,
		event:          tree,
		signalChan:     make(chan Signal, 1),
	}
	return runtime, nil
}
