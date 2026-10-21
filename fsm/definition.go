package fsm

import (
	"context"
	"fmt"
	"reflect"
)

type transitionKey[S State] struct {
	state S
	event string
}

// Definition is an immutable set of states and transition rules.
// It can be shared by concurrent machines. Callbacks must be safe for concurrent
// use if they access data shared across those machines.
// Create definitions with Builder.Build.
type Definition[S State, D any] struct {
	states      []stateDescription[S]
	stateIndex  map[S]int
	events      map[string]eventDescription
	transitions map[transitionKey[S]]rule[S, D]
	rules       []rule[S, D]
}

// Fire computes the transition for an explicit current state without storing
// anything: it resolves the event in from, evaluates the guard, runs the action
// and returns the committed target state. The caller owns the current state and
// is responsible for persisting the result and for any concurrency control
// (for example an optimistic compare-and-swap on the stored state).
//
// This is the stateless core primitive, suited to entities whose authoritative
// state lives outside the process, such as a row in a database. For in-process
// state that a single owner holds in memory, use NewMachine instead.
//
// On any error the returned state equals from, so a failed call never advances
// a persisted entity. Guard rejection returns ErrGuardRejected; a disallowed
// (from, event) pair returns ErrTransitionNotAllowed; all errors are wrapped in
// *TransitionError. Action side effects are not rolled back on error or panic.
//
// ctx must not be nil and is checked before the guard and before the action.
// If Action returns nil, the target state is returned even if ctx was cancelled
// during that action, keeping successful business work and the result aligned.
func (d *Definition[S, D]) Fire[P any](ctx context.Context, from S, data D, event Event[S, P], value P) (S, error) {
	if d == nil {
		return from, ErrInvalidDefinition
	}
	if err := ctx.Err(); err != nil {
		return from, err
	}
	wrap := func(operation string, err error) error {
		return &TransitionError[S]{State: from, Event: event.name, Operation: operation, Err: err}
	}
	registered, exists := d.events[event.name]
	if !exists {
		return from, wrap("lookup", ErrUnknownEvent)
	}
	if registered.payloadType != reflect.TypeFor[P]() {
		return from, wrap("lookup", ErrEventTypeMismatch)
	}
	r, exists := d.transitions[transitionKey[S]{state: from, event: event.name}]
	if !exists {
		return from, wrap("lookup", ErrTransitionNotAllowed)
	}
	boxed := payload[P]{value: value}
	if r.guard != nil {
		allowed, err := r.guard(ctx, data, boxed)
		if err != nil {
			return from, wrap("guard", err)
		}
		if !allowed {
			return from, wrap("guard", ErrGuardRejected)
		}
	}
	if err := ctx.Err(); err != nil {
		return from, err
	}
	if r.action != nil {
		if err := r.action(ctx, data, boxed); err != nil {
			return from, wrap("action", err)
		}
	}
	return r.to, nil
}

// NewMachine binds an initial state and business data to a new machine.
// The state must have been explicitly registered or referenced by a transition.
// Data is retained as supplied, not copied deeply.
func (d *Definition[S, D]) NewMachine(initial S, data D) (*Machine[S, D], error) {
	if d == nil || len(d.states) == 0 {
		return nil, ErrInvalidDefinition
	}
	if !validState(initial) {
		return nil, fmt.Errorf("%w: %v", ErrUnknownState, initial)
	}
	if _, exists := d.stateIndex[initial]; !exists {
		return nil, fmt.Errorf("%w: %v", ErrUnknownState, initial)
	}
	return &Machine[S, D]{
		definition: d,
		state:      initial,
		data:       data,
		gate:       make(chan struct{}, 1),
	}, nil
}
