package fsm

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidDefinition    = errors.New("fsm: invalid definition")
	ErrUnknownState         = errors.New("fsm: unknown state")
	ErrUnknownEvent         = errors.New("fsm: unknown event")
	ErrEventTypeMismatch    = errors.New("fsm: event payload type mismatch")
	ErrTransitionNotAllowed = errors.New("fsm: transition not allowed")
	ErrGuardRejected        = errors.New("fsm: guard rejected transition")
	ErrUninitializedMachine = errors.New("fsm: uninitialized machine")
)

// TransitionError identifies the event, source state and failing operation.
// Operation is "lookup", "guard", or "action".
// Unwrap preserves both sentinel errors and errors returned by callbacks.
type TransitionError[S State] struct {
	State     S
	Event     string
	Operation string
	Err       error
}

func (e *TransitionError[S]) Error() string {
	return fmt.Sprintf("fsm: %s event %q from %v: %v", e.Operation, e.Event, e.State, e.Err)
}

func (e *TransitionError[S]) Unwrap() error { return e.Err }
