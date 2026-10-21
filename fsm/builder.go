package fsm

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

// Builder configures a Definition. It is not safe for concurrent mutation.
// Build snapshots its configuration; subsequent edits do not change earlier
// definitions. Captured callback data remains the caller's responsibility.
type Builder[S State, D any] struct {
	states []stateDescription[S]
	rules  []*rule[S, D]
}

type stateDescription[S State] struct {
	value       S
	description string
}

type eventDescription struct {
	name        string
	description string
	payloadType reflect.Type
}

type rule[S State, D any] struct {
	event       eventDescription
	from        []S
	to          S
	hasTarget   bool
	description string
	guard       func(context.Context, D, any) (bool, error)
	action      func(context.Context, D, any) error
}

// NewBuilder creates a builder whose states are S and business data is D.
func NewBuilder[S State, D any]() *Builder[S, D] {
	return &Builder[S, D]{}
}

// State registers a state and its optional description, including isolated
// states. From and To automatically register states without descriptions.
// Repeated explicit declarations are rejected by Build.
func (b *Builder[S, D]) State(state S, description ...string) *Builder[S, D] {
	b.states = append(b.states, stateDescription[S]{state, strings.Join(description, "\n")})
	return b
}

// On begins a transition for event. P is inferred from the event, so Guard and
// Action receive its exact payload type.
func (b *Builder[S, D]) On[P any](event Event[S, P]) *TransitionBuilder[S, D, P] {
	r := &rule[S, D]{
		event: eventDescription{
			name:        event.name,
			description: event.description,
			payloadType: reflect.TypeFor[P](),
		},
	}
	b.rules = append(b.rules, r)
	return &TransitionBuilder[S, D, P]{rule: r}
}

// TransitionBuilder configures one event's transition from one or more states.
// Repeated calls to a setter replace its earlier value.
type TransitionBuilder[S State, D, P any] struct {
	rule *rule[S, D]
}

// From sets the allowed source states. At least one source is required.
func (b *TransitionBuilder[S, D, P]) From(states ...S) *TransitionBuilder[S, D, P] {
	b.rule.from = append([]S(nil), states...)
	return b
}

// To sets the target state, which may be equal to a source state.
func (b *TransitionBuilder[S, D, P]) To(state S) *TransitionBuilder[S, D, P] {
	b.rule.to, b.rule.hasTarget = state, true
	return b
}

// Description sets a human-readable explanation displayed on the diagram edge.
func (b *TransitionBuilder[S, D, P]) Description(description string) *TransitionBuilder[S, D, P] {
	b.rule.description = description
	return b
}

// Guard sets a predicate evaluated before Action. False rejects the transition.
// A nil predicate removes the guard. Guards should not mutate business data.
func (b *TransitionBuilder[S, D, P]) Guard(fn func(context.Context, D, P) (bool, error)) *TransitionBuilder[S, D, P] {
	b.rule.guard = nil
	if fn != nil {
		b.rule.guard = func(ctx context.Context, data D, value any) (bool, error) {
			return fn(ctx, data, value.(payload[P]).value)
		}
	}
	return b
}

// Action sets the business operation executed before committing the target
// state. A nil action removes the callback. External side effects are not
// rolled back on error or panic.
func (b *TransitionBuilder[S, D, P]) Action(fn func(context.Context, D, P) error) *TransitionBuilder[S, D, P] {
	b.rule.action = nil
	if fn != nil {
		b.rule.action = func(ctx context.Context, data D, value any) error {
			return fn(ctx, data, value.(payload[P]).value)
		}
	}
	return b
}

// Build validates and snapshots the configuration. It rejects missing sources
// or targets, duplicate (source, event) pairs, empty event names, incompatible
// declarations of the same event name, and invalid state values.
func (b *Builder[S, D]) Build() (*Definition[S, D], error) {
	d := &Definition[S, D]{
		stateIndex:  make(map[S]int),
		events:      make(map[string]eventDescription),
		transitions: make(map[transitionKey[S]]rule[S, D]),
	}
	addState := func(state S, description string) error {
		if !validState(state) {
			return fmt.Errorf("%w: non-comparable or non-reflexive state %v", ErrInvalidDefinition, state)
		}
		if _, exists := d.stateIndex[state]; !exists {
			d.stateIndex[state] = len(d.states)
			d.states = append(d.states, stateDescription[S]{state, description})
		}
		return nil
	}
	for _, state := range b.states {
		before := len(d.states)
		if err := addState(state.value, state.description); err != nil {
			return nil, err
		}
		if len(d.states) == before {
			return nil, fmt.Errorf("%w: duplicate state %v", ErrInvalidDefinition, state.value)
		}
	}
	for i, configured := range b.rules {
		r := *configured
		r.from = append([]S(nil), configured.from...)
		if strings.TrimSpace(r.event.name) == "" {
			return nil, fmt.Errorf("%w: rule %d has an empty event name", ErrInvalidDefinition, i)
		}
		if len(r.from) == 0 || !r.hasTarget {
			return nil, fmt.Errorf("%w: event %q requires From and To", ErrInvalidDefinition, r.event.name)
		}
		if previous, exists := d.events[r.event.name]; exists && previous != r.event {
			return nil, fmt.Errorf("%w: conflicting declarations for event %q", ErrInvalidDefinition, r.event.name)
		}
		d.events[r.event.name] = r.event
		for _, from := range r.from {
			if err := addState(from, ""); err != nil {
				return nil, err
			}
			key := transitionKey[S]{state: from, event: r.event.name}
			if _, exists := d.transitions[key]; exists {
				return nil, fmt.Errorf("%w: duplicate transition from %v on %q", ErrInvalidDefinition, from, r.event.name)
			}
			d.transitions[key] = r
		}
		if err := addState(r.to, ""); err != nil {
			return nil, err
		}
		d.rules = append(d.rules, r)
	}
	if len(d.states) == 0 {
		return nil, fmt.Errorf("%w: at least one state is required", ErrInvalidDefinition)
	}
	return d, nil
}

func validState[S State](state S) bool {
	value := reflect.ValueOf(state)
	return (!value.IsValid() || value.Comparable()) && state == state
}
