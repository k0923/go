package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

var _ Infrastructure = (*DebugInfrastructure)(nil)

type DebugInfrastructure struct {
	lock             sync.RWMutex
	events           map[string]Event
	leases           map[string]ExecutionLease
	leaseGenerations map[string]int64
	errors           []error
}

func NewDebugInfrastructure() *DebugInfrastructure {
	return &DebugInfrastructure{
		events:           make(map[string]Event),
		leases:           make(map[string]ExecutionLease),
		leaseGenerations: make(map[string]int64),
	}
}

func (infrastructure *DebugInfrastructure) ClaimLease(
	ctx context.Context,
	key string,
	ttl time.Duration,
) (*ExecutionLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("lease ttl must be positive")
	}

	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()

	now := time.Now()
	if lease, exists := infrastructure.leases[key]; exists && lease.ExpiresAt.After(now) {
		return nil, nil
	}

	generation := infrastructure.leaseGenerations[key] + 1
	lease := ExecutionLease{
		Key:        key,
		Generation: generation,
		ExpiresAt:  now.Add(ttl),
	}
	infrastructure.leases[key] = lease
	infrastructure.leaseGenerations[key] = generation
	return cloneLease(lease), nil
}

func (infrastructure *DebugInfrastructure) RenewLease(
	ctx context.Context,
	lease ExecutionLease,
	ttl time.Duration,
) (*ExecutionLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("lease ttl must be positive")
	}

	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()

	now := time.Now()
	current, exists := infrastructure.leases[lease.Key]
	if !exists ||
		current.Generation != lease.Generation ||
		!current.ExpiresAt.After(now) {
		return nil, nil
	}
	current.ExpiresAt = now.Add(ttl)
	infrastructure.leases[lease.Key] = current
	return cloneLease(current), nil
}

func (infrastructure *DebugInfrastructure) ReleaseLease(
	ctx context.Context,
	lease ExecutionLease,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()

	current, exists := infrastructure.leases[lease.Key]
	if !exists || current.Generation != lease.Generation {
		return false, nil
	}
	delete(infrastructure.leases, lease.Key)
	return true, nil
}

func (infrastructure *DebugInfrastructure) AddEvent(ctx context.Context, event Event) (Event, error) {
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
	return cloneEvent(stored)
}

func (infrastructure *DebugInfrastructure) GetEvent(ctx context.Context, key string) (Event, error) {
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

func (infrastructure *DebugInfrastructure) UpdateEvent(ctx context.Context, event Event) (Event, error) {
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

func (infrastructure *DebugInfrastructure) OnError(err error) {
	if err == nil {
		return
	}
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.errors = append(infrastructure.errors, err)
}

func (infrastructure *DebugInfrastructure) Errors() []error {
	infrastructure.lock.RLock()
	defer infrastructure.lock.RUnlock()
	return append([]error(nil), infrastructure.errors...)
}

func (infrastructure *DebugInfrastructure) initialize() {
	if infrastructure.events == nil {
		infrastructure.events = make(map[string]Event)
	}
	if infrastructure.leases == nil {
		infrastructure.leases = make(map[string]ExecutionLease)
	}
	if infrastructure.leaseGenerations == nil {
		infrastructure.leaseGenerations = make(map[string]int64)
	}
}

func cloneLease(lease ExecutionLease) *ExecutionLease {
	result := lease
	return &result
}

func cloneEvent(event Event) (Event, error) {
	switch value := event.(type) {
	case *StepResultEvent:
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
	case *LoopEvent:
		result := *value
		result.Result = cloneRawMessage(value.Result)
		return &result, nil
	default:
		return nil, fmt.Errorf("unsupported event type %T", event)
	}
}

func setEventVersion(event Event, version int) {
	switch value := event.(type) {
	case *StepResultEvent:
		value.Version = version
	case *WaitForSignalEvent:
		value.Version = version
	case *CheckpointEvent:
		value.Version = version
	case *SleepSignalEvent:
		value.Version = version
	case *LoopEvent:
		value.Version = version
	}
}

func cloneRawMessage(message json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), message...)
}
