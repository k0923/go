package flow

import "context"

type FlowStatus string

const (
	FlowStatusRunning   FlowStatus = "RUNNING"
	FlowStatusCompleted FlowStatus = "COMPLETED"
	FlowStatusFailed    FlowStatus = "FAILED"
	FlowStatusSuspended FlowStatus = "SUSPENDED"
)

type State[Input any, Output any] struct {
	Input  Input
	Output Output
	Status FlowStatus
	Errors []string
}

type Infrastructure[Input any, Output any] interface {
	LeaseInfrastructure
	AddEvent(ctx context.Context, event Event) (Event, error)
	GetEvent(ctx context.Context, key string) (Event, error)
	UpdateEvent(ctx context.Context, event Event) (Event, error)
	GetState(ctx context.Context) (State[Input, Output], error)
	SetState(ctx context.Context, state State[Input, Output]) error
	OnError(err error)
}
