// Package fsm implements typed finite state machines.
//
// Build a reusable, immutable Definition with NewBuilder and trigger typed
// Events through it. Definition.Fire is the stateless core: the caller supplies
// the current state and persists the returned one, which fits entities whose
// state lives in external storage. For in-process state held by a single owner,
// Definition.NewMachine returns a Machine that keeps the state in memory.
package fsm

// State is the constraint for business-specific state types.
// Use a distinct named type for each business domain, rather than a type alias.
// Values must be comparable and reflexive (s == s); NaN states are rejected.
type State interface {
	comparable
	FSMState()
}
