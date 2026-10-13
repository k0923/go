package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

var _ Infrastructure[any, any] = (*InMemoryInfrastructure[any, any])(nil)

type InMemoryInfrastructure[Input any, Output any] struct {
	lock    sync.RWMutex
	state   State[Input, Output]
	events  map[string]Event
	signals []Signal
	errors  []error
}

func NewInMemoryInfrastructure[Input any, Output any]() *InMemoryInfrastructure[Input, Output] {
	return &InMemoryInfrastructure[Input, Output]{
		events: make(map[string]Event),
	}
}

func (infrastructure *InMemoryInfrastructure[Input, Output]) AddEvent(ctx context.Context, event Event) (Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stored, err := cloneEvent(event)
	if err != nil {
		return nil, err
	}
	setEventVersion(stored, 1)
	key := event.GetKey()

	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	if infrastructure.events[key] != nil {
		return nil, nil
	}
	infrastructure.events[key] = stored
	if signal, ok := stored.(Signal); ok {
		infrastructure.signals = append(infrastructure.signals, signal)
	}
	return cloneEvent(stored)
}

func (infrastructure *InMemoryInfrastructure[Input, Output]) GetEvent(ctx context.Context, key string) (Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	infrastructure.lock.RLock()
	defer infrastructure.lock.RUnlock()
	if infrastructure.events == nil || infrastructure.events[key] == nil {
		return nil, nil
	}
	return cloneEvent(infrastructure.events[key])
}

func (infrastructure *InMemoryInfrastructure[Input, Output]) UpdateEvent(ctx context.Context, event Event) (Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := event.GetKey()

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

func (infrastructure *InMemoryInfrastructure[Input, Output]) GetState(ctx context.Context) (State[Input, Output], error) {
	if err := ctx.Err(); err != nil {
		return State[Input, Output]{}, err
	}
	infrastructure.lock.RLock()
	defer infrastructure.lock.RUnlock()
	return cloneState(infrastructure.state), nil
}

func (infrastructure *InMemoryInfrastructure[Input, Output]) SetState(ctx context.Context, state State[Input, Output]) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.state = cloneState(state)
	return nil
}

func (infrastructure *InMemoryInfrastructure[Input, Output]) OnError(err error) {
	if err == nil {
		return
	}
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.errors = append(infrastructure.errors, err)
}

func (infrastructure *InMemoryInfrastructure[Input, Output]) Signals() []Signal {
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

func (infrastructure *InMemoryInfrastructure[Input, Output]) Errors() []error {
	infrastructure.lock.RLock()
	defer infrastructure.lock.RUnlock()
	return append([]error(nil), infrastructure.errors...)
}

func (infrastructure *InMemoryInfrastructure[Input, Output]) initialize() {
	if infrastructure.events == nil {
		infrastructure.events = make(map[string]Event)
	}
}

func cloneState[Input any, Output any](state State[Input, Output]) State[Input, Output] {
	if value, ok := any(state.Input).(json.RawMessage); ok {
		state.Input = any(cloneRawMessage(value)).(Input)
	}
	if value, ok := any(state.Output).(json.RawMessage); ok {
		state.Output = any(cloneRawMessage(value)).(Output)
	}
	state.Errors = append([]string(nil), state.Errors...)
	return state
}

func cloneEvent(event Event) (Event, error) {
	switch value := event.(type) {
	case *StepResultEvent:
		result := *value
		result.Result = cloneRawMessage(value.Result)
		return &result, nil
	case *AsyncEvent:
		result := *value
		result.Result = cloneRawMessage(value.Result)
		return &result, nil
	case *WaitForSignalEvent:
		result := *value
		result.Request = cloneRawMessage(value.Request)
		result.Response = cloneRawMessage(value.Response)
		return &result, nil
	case *CheckpointEvent:
		result := *value
		result.Data = cloneRawMessage(value.Data)
		return &result, nil
	case *SleepSignalEvent:
		result := *value
		return &result, nil
	default:
		return nil, fmt.Errorf("unsupported event type %T", event)
	}
}

func setEventVersion(event Event, version int) {
	switch value := event.(type) {
	case *StepResultEvent:
		value.Version = version
	case *AsyncEvent:
		value.Version = version
	case *WaitForSignalEvent:
		value.Version = version
	case *CheckpointEvent:
		value.Version = version
	case *SleepSignalEvent:
		value.Version = version
	}
}

func cloneSignal(signal Signal) (Signal, error) {
	event, err := cloneEvent(signal)
	if err != nil {
		return nil, err
	}
	result, ok := event.(Signal)
	if !ok {
		return nil, fmt.Errorf("event %T does not implement Signal", event)
	}
	return result, nil
}

func cloneRawMessage(message json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), message...)
}
