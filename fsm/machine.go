package fsm

import (
	"context"
	"sync"
)

// Machine holds one business object's state in memory, layered over the
// stateless Definition.Fire primitive. Use it when the current state has no
// authoritative home outside the process and a single owner drives it, such as
// a connection, session or device handshake. For entities whose state is loaded
// from and saved to external storage, call Definition.Fire directly instead.
//
// Concurrent Fire calls on one machine are serialized; State can be called from
// a callback. The serialization only protects this one in-memory instance and
// is no substitute for storage-level concurrency control across processes.
// Do not copy a Machine or synchronously Fire it again from its own callback.
// Create machines with Definition.NewMachine; the zero value cannot execute.
type Machine[S State, D any] struct {
	definition *Definition[S, D]
	data       D
	gate       chan struct{}
	mu         sync.RWMutex
	state      S
}

// State returns the last committed state, including while a callback is running.
func (m *Machine[S, D]) State() S {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Fire resolves the event in the current state and commits the target state on
// success by delegating to Definition.Fire. Errors and panics leave the machine
// state unchanged; mutations to business data are not rolled back.
//
// ctx must not be nil. Cancellation interrupts waiting for another transition
// and is checked before callbacks. Callbacks must cooperate with cancellation.
// Event descriptions do not participate in lookup; the name and P do.
func (m *Machine[S, D]) Fire[P any](ctx context.Context, event Event[S, P], value P) error {
	if m == nil || m.definition == nil {
		return ErrUninitializedMachine
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.gate <- struct{}{}:
		defer func() { <-m.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	next, err := m.definition.Fire(ctx, m.State(), m.data, event, value)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.state = next
	m.mu.Unlock()
	return nil
}
