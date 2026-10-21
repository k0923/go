package fsm

import "strings"

// Event binds an event name to both its state machine's state type S and its
// payload type P. The S parameter ties an event to one Definition's state type,
// so an event created for one machine cannot be passed to a builder, machine or
// Fire call of another; that mismatch is a compile error rather than a runtime
// surprise. Its zero value is invalid.
//
// Within a Definition, a name must have one payload type and description. Events
// recreated with the same S, name and P can trigger the same transitions.
type Event[S State, P any] struct {
	name        string
	description string
}

// NewEvent creates an event bound to state type S and payload type P. Both type
// arguments are explicit, for example NewEvent[OrderState, Payment]("pay").
// Optional description paragraphs are joined by newlines. Empty or
// whitespace-only names are rejected by Builder.Build. Use struct{} as P for
// events without a payload.
func NewEvent[S State, P any](name string, description ...string) Event[S, P] {
	return Event[S, P]{name: name, description: strings.Join(description, "\n")}
}

// Name returns the event's identity within a Definition.
func (e Event[S, P]) Name() string { return e.name }

// Description returns the event's optional human-readable description.
func (e Event[S, P]) Description() string { return e.description }

// Boxing preserves P even when it is an interface holding nil.
type payload[P any] struct {
	value P
}
