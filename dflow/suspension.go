package dflow

// Suspension carries one pending operation's dispatch and resume context.
// Type supports application routing; storage owns encoding and reconstruction.
type Suspension interface {
	Type() string
}

// ValueSuspension is a generic payload for application-defined operations.
type ValueSuspension struct {
	Kind string
	Data any
}

func (s *ValueSuspension) Type() string { return s.Kind }
