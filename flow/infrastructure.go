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
}

type eventAppender interface {
	AppendEvent(ctx context.Context, event Event, expectedIndex int) (bool, error)
}

type Infrastructure interface {
	eventAppender
	GetState(ctx context.Context) (State, error)
	SetState(ctx context.Context, state State) error
	GetEvents(ctx context.Context) ([]Event, error)
	ProcessSignal(ctx context.Context, signal Signal)
}
