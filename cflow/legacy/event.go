package cflow

import (
	"encoding/json"
	"fmt"
	"time"
)

var _ ResultEvent = (*StepResultEvent)(nil)
var _ Signal = (*WaitForSignalEvent)(nil)
var _ ResultEvent = (*CheckpointEvent)(nil)
var _ Signal = (*SleepSignalEvent)(nil)
var _ ResultEvent = (*LoopEvent)(nil)

type Event interface {
	GetEventType() string
	GetKey() string
	GetStatus() FlowStatus
	GetVersion() int
	GetError() string
	SetError(err error)
}

type ResultEvent interface {
	Event
	GetResult() json.RawMessage
	SetResult(result json.RawMessage)
	SetStatus(status FlowStatus)
}

type Signal interface {
	Event
	ResolveSignal(signal Signal) error
}

type BaseEvent struct {
	Key       string
	StartedAt time.Time
	EndedAt   time.Time
	Status    FlowStatus
	Version   int
	Error     string
}

func (e *BaseEvent) SetError(err error) {
	e.SetStatus(FlowStatusFailed)
	if err != nil {
		e.Error = err.Error()
	}
}

func (e *BaseEvent) SetStatus(status FlowStatus) {
	e.Status = status
	switch status {
	case FlowStatusCompleted, FlowStatusFailed:
		e.EndedAt = time.Now()
	default:
		e.EndedAt = time.Time{}
	}
}

func (e *BaseEvent) GetStatus() FlowStatus {
	return e.Status
}

func (e *BaseEvent) GetVersion() int {
	return e.Version
}

func (e *BaseEvent) GetError() string {
	return e.Error
}

func (e *BaseEvent) GetKey() string {
	return e.Key
}

func (e *BaseEvent) GetStartedAt() time.Time {
	return e.StartedAt
}

func (e *BaseEvent) GetEndedAt() time.Time {
	return e.EndedAt
}

type StepResultEvent struct {
	BaseEvent
	Result json.RawMessage
}

func (e *StepResultEvent) GetEventType() string {
	return "STEP_RESULT"
}

func (e *StepResultEvent) GetResult() json.RawMessage {
	return e.Result
}

func (e *StepResultEvent) SetResult(result json.RawMessage) {
	e.Result = result
}

type WaitForSignalEvent struct {
	BaseEvent
	Request  json.RawMessage
	Response json.RawMessage
}

func (e *WaitForSignalEvent) ResolveSignal(signal Signal) error {
	if s, ok := signal.(*WaitForSignalEvent); ok {
		e.Response = s.Response
		e.EndedAt = time.Now()
		e.Status = FlowStatusCompleted
		return nil
	}
	return fmt.Errorf("unsupported signal type %T", signal)
}

func (e *WaitForSignalEvent) GetEventType() string {
	return "WAIT_FOR_SIGNAL"
}

type CheckpointEvent struct {
	BaseEvent
	Data json.RawMessage
}

func (e *CheckpointEvent) GetEventType() string {
	return "CHECKPOINT"
}

func (e *CheckpointEvent) GetResult() json.RawMessage {
	return e.Data
}

func (e *CheckpointEvent) SetResult(result json.RawMessage) {
	e.Data = result
}

type SleepSignalEvent struct {
	BaseEvent
	Until time.Time
}

func (e *SleepSignalEvent) GetEventType() string {
	return "SLEEP"
}

func (e *SleepSignalEvent) ResolveSignal(signal Signal) error {
	if _, ok := signal.(*SleepSignalEvent); !ok {
		return fmt.Errorf("unsupported signal type %T", signal)
	}
	if e.Status != FlowStatusSuspended && e.Status != FlowStatusRunning {
		return NewConflictError(e)
	}
	if time.Now().Before(e.Until) {
		return NewConflictError(e)
	}
	e.SetStatus(FlowStatusCompleted)
	return nil
}

type LoopEvent struct {
	BaseEvent
	Result  json.RawMessage
	Attempt int
}

func (e *LoopEvent) GetEventType() string {
	return "LOOP"
}

func (e *LoopEvent) GetResult() json.RawMessage {
	return e.Result
}

func (e *LoopEvent) SetResult(result json.RawMessage) {
	e.Result = result
}
