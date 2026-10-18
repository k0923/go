package dflow

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidDefinition = errors.New("invalid DAG definition")
	ErrInvalidReference  = errors.New("invalid output reference")
	ErrMissingValue      = errors.New("referenced value is missing")
	ErrConflict          = errors.New("run state conflict")
	ErrRunMismatch       = errors.New("infrastructure is bound to a different definition or input")
	ErrWaitNotFound      = errors.New("wait is not registered")
	ErrInvalidSuspension = errors.New("invalid node suspension")
)

// ValidationError identifies a definition error before any node is executed.
type ValidationError struct {
	NodeID string
	Detail string
	Cause  error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("dflow definition node %q: %s: %v", e.NodeID, e.Detail, e.Cause)
}
func (e *ValidationError) Unwrap() error { return e.Cause }

// NodeError preserves the node and stage, including the underlying context error.
type NodeError struct {
	NodeID string
	Stage  string
	Cause  error
}

func (e *NodeError) Error() string {
	return fmt.Sprintf("dflow node %q %s: %v", e.NodeID, e.Stage, e.Cause)
}
func (e *NodeError) Unwrap() error { return e.Cause }

// InfrastructureError means execution/persistence is uncertain, not a business failure.
type InfrastructureError struct {
	Op    string
	Cause error
}

func (e *InfrastructureError) Error() string {
	return fmt.Sprintf("dflow infrastructure %s: %v", e.Op, e.Cause)
}
func (e *InfrastructureError) Unwrap() error { return e.Cause }
