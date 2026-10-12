package flow

import (
	"context"
	"encoding/json"
)

type FlowStatus string

const (
	FlowStatusRunning  FlowStatus = "RUNNING"
	FlowStatusFinished FlowStatus = "FINISHED"
	FlowStatusFailed   FlowStatus = "FAILED"
)

type State struct {
	Input  json.RawMessage
	Output json.RawMessage
	Status FlowStatus
	Errors []string
}

type Infrastructure interface {
	AddEvent(ctx context.Context, event Event) (Event, error)
	GetEvent(ctx context.Context, branches []string, key string) (Event, error)
	UpdateEvent(ctx context.Context, event Event) (Event, error)
	RaiseSignal(ctx context.Context, signal Signal) error
	ResolveSignal(ctx context.Context, signal Signal) (bool, error)
	GetState(ctx context.Context) (State, error)
	SetState(ctx context.Context, state State) error
	OnError(err error)
}
