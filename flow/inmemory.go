package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

var _ Infrastructure = (*InMemoryInfrastructure)(nil)

type inMemoryEventKey struct {
	branches string
	key      string
}

type InMemoryInfrastructure struct {
	lock    sync.RWMutex
	state   State
	events  map[inMemoryEventKey]Event
	signals []Signal
	errors  []error
}

func NewInMemoryInfrastructure() *InMemoryInfrastructure {
	return &InMemoryInfrastructure{
		events: make(map[inMemoryEventKey]Event),
	}
}

func (infrastructure *InMemoryInfrastructure) AddEvent(ctx context.Context, event Event) (Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stored, err := cloneEvent(event)
	if err != nil {
		return nil, err
	}
	setEventVersion(stored, 1)
	key := newInMemoryEventKey(event.GetBranches(), event.GetKey())

	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	if infrastructure.events[key] != nil {
		return nil, nil
	}
	infrastructure.events[key] = stored
	return cloneEvent(stored)
}

func (infrastructure *InMemoryInfrastructure) GetEvent(ctx context.Context, branches []string, key string) (Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	eventKey := newInMemoryEventKey(branches, key)

	infrastructure.lock.RLock()
	defer infrastructure.lock.RUnlock()
	if infrastructure.events == nil || infrastructure.events[eventKey] == nil {
		return nil, nil
	}
	return cloneEvent(infrastructure.events[eventKey])
}

func (infrastructure *InMemoryInfrastructure) UpdateEvent(ctx context.Context, event Event) (Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := newInMemoryEventKey(event.GetBranches(), event.GetKey())

	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	current := infrastructure.events[key]
	if current == nil || current.GetVersion() != event.GetVersion() {
		return nil, nil
	}
	stored, err := cloneEvent(event)
	if err != nil {
		return nil, err
	}
	setEventVersion(stored, event.GetVersion()+1)
	infrastructure.events[key] = stored
	return cloneEvent(stored)
}

func (infrastructure *InMemoryInfrastructure) RaiseSignal(ctx context.Context, signal Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stored, err := cloneSignal(signal)
	if err != nil {
		return err
	}

	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.signals = append(infrastructure.signals, stored)
	return nil
}

func (infrastructure *InMemoryInfrastructure) ResolveSignal(ctx context.Context, signal Signal) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	branches, ok := signalBranches(signal)
	if !ok {
		return false, nil
	}
	key := newInMemoryEventKey(branches, signal.GetKey())

	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	event := infrastructure.events[key]
	if event == nil ||
		event.GetStatus() != FlowStatusRunning ||
		event.GetVersion() != signal.GetVersion() {
		return false, nil
	}

	switch value := signal.(type) {
	case *WaitForSignal:
		awaited, ok := event.(*WaitForSignalEvent)
		if !ok {
			return false, nil
		}
		resolved, err := cloneEvent(awaited)
		if err != nil {
			return false, err
		}
		result := resolved.(*WaitForSignalEvent)
		result.Response = cloneRawMessage(value.Response)
		result.Status = FlowStatusFinished
		result.EndedAt = time.Now()
		result.Version++
		infrastructure.events[key] = result
	case *SleepSignal:
		sleep, ok := event.(*SleepEvent)
		if !ok || time.Now().Before(value.Until) {
			return false, nil
		}
		resolved, err := cloneEvent(sleep)
		if err != nil {
			return false, err
		}
		result := resolved.(*SleepEvent)
		result.Status = FlowStatusFinished
		result.EndedAt = time.Now()
		result.Version++
		infrastructure.events[key] = result
	default:
		return false, nil
	}
	return true, nil
}

func (infrastructure *InMemoryInfrastructure) GetState(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	infrastructure.lock.RLock()
	defer infrastructure.lock.RUnlock()
	return cloneState(infrastructure.state), nil
}

func (infrastructure *InMemoryInfrastructure) SetState(ctx context.Context, state State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.state = cloneState(state)
	return nil
}

func (infrastructure *InMemoryInfrastructure) OnError(err error) {
	if err == nil {
		return
	}
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.errors = append(infrastructure.errors, err)
}

func (infrastructure *InMemoryInfrastructure) Signals() []Signal {
	infrastructure.lock.RLock()
	defer infrastructure.lock.RUnlock()
	signals := make([]Signal, 0, len(infrastructure.signals))
	for _, signal := range infrastructure.signals {
		cloned, err := cloneSignal(signal)
		if err == nil {
			signals = append(signals, cloned)
		}
	}
	return signals
}

func (infrastructure *InMemoryInfrastructure) Errors() []error {
	infrastructure.lock.RLock()
	defer infrastructure.lock.RUnlock()
	return append([]error(nil), infrastructure.errors...)
}

func (infrastructure *InMemoryInfrastructure) initialize() {
	if infrastructure.events == nil {
		infrastructure.events = make(map[inMemoryEventKey]Event)
	}
}

func newInMemoryEventKey(branches []string, key string) inMemoryEventKey {
	data, _ := json.Marshal(branches)
	return inMemoryEventKey{
		branches: string(data),
		key:      key,
	}
}

func cloneState(state State) State {
	state.Input = cloneRawMessage(state.Input)
	state.Output = cloneRawMessage(state.Output)
	state.Errors = append([]string(nil), state.Errors...)
	return state
}

func cloneBaseEvent(event BaseEvent) BaseEvent {
	event.Branches = append([]string(nil), event.Branches...)
	return event
}

func cloneEvent(event Event) (Event, error) {
	switch value := event.(type) {
	case *StepResultEvent:
		result := *value
		result.BaseEvent = cloneBaseEvent(value.BaseEvent)
		result.Result = cloneRawMessage(value.Result)
		return &result, nil
	case *BranchResultEvent:
		result := *value
		result.BaseEvent = cloneBaseEvent(value.BaseEvent)
		return &result, nil
	case *WaitForSignalEvent:
		result := *value
		result.BaseEvent = cloneBaseEvent(value.BaseEvent)
		result.Request = cloneRawMessage(value.Request)
		result.Response = cloneRawMessage(value.Response)
		return &result, nil
	case *CheckpointEvent:
		result := *value
		result.BaseEvent = cloneBaseEvent(value.BaseEvent)
		result.Data = cloneRawMessage(value.Data)
		return &result, nil
	case *SleepEvent:
		result := *value
		result.BaseEvent = cloneBaseEvent(value.BaseEvent)
		return &result, nil
	default:
		return nil, fmt.Errorf("unsupported event type %T", event)
	}
}

func setEventVersion(event Event, version int) {
	switch value := event.(type) {
	case *StepResultEvent:
		value.Version = version
	case *BranchResultEvent:
		value.Version = version
	case *WaitForSignalEvent:
		value.Version = version
	case *CheckpointEvent:
		value.Version = version
	case *SleepEvent:
		value.Version = version
	}
}

func cloneSignal(signal Signal) (Signal, error) {
	switch value := signal.(type) {
	case *WaitForSignal:
		result := *value
		result.Branches = append([]string(nil), value.Branches...)
		result.Request = cloneRawMessage(value.Request)
		result.Response = cloneRawMessage(value.Response)
		return &result, nil
	case *SleepSignal:
		result := *value
		result.Branches = append([]string(nil), value.Branches...)
		return &result, nil
	case BranchSignal:
		result := make(BranchSignal, len(value))
		for key, branchResult := range value {
			result[key] = branchResult
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported signal type %T", signal)
	}
}

func signalBranches(signal Signal) ([]string, bool) {
	switch value := signal.(type) {
	case *WaitForSignal:
		return value.Branches, true
	case *SleepSignal:
		return value.Branches, true
	default:
		return nil, false
	}
}

func cloneRawMessage(message json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), message...)
}
