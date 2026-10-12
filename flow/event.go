package flow

import (
	"encoding/json"
	"time"
)

var _ Event = (*StepResultEvent)(nil)
var _ Event = (*BranchResultEvent)(nil)
var _ Event = (*WaitForSignalEvent)(nil)
var _ Event = (*CheckpointEvent)(nil)
var _ Event = (*SleepEvent)(nil)

type Event interface {
	GetEventType() string
	GetKey() string
	GetBranches() []string
	GetStatus() FlowStatus
	GetVersion() int
}

type BaseEvent struct {
	Key       string
	StartedAt time.Time
	EndedAt   time.Time
	Status    FlowStatus
	Version   int
	Branches  []string
}

func (e *BaseEvent) GetStatus() FlowStatus {
	return e.Status
}

func (e *BaseEvent) GetVersion() int {
	return e.Version
}

func (e *BaseEvent) GetKey() string {
	return e.Key
}

func (e *BaseEvent) GetBranches() []string {
	return e.Branches
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
	Error  string
}

func (e *StepResultEvent) GetEventType() string {
	return "STEP_RESULT"
}

type BranchResultEvent struct {
	BaseEvent
}

func (e *BranchResultEvent) GetEventType() string {
	return "BRANCH_RESULT"
}

type WaitForSignalEvent struct {
	BaseEvent
	Request  json.RawMessage
	Response json.RawMessage
}

func (e *WaitForSignalEvent) GetEventType() string {
	return "WAIT_FOR_SIGNAL"
}

func (e *WaitForSignalEvent) BuildSignal() Signal {
	return NewWaitForSignal(e.Key, e.Branches, e.Request, e.Response, e.Version)
}

type CheckpointEvent struct {
	BaseEvent
	Data json.RawMessage
}

func (e *CheckpointEvent) GetEventType() string {
	return "CHECKPOINT"
}

type SleepEvent struct {
	BaseEvent
	Until time.Time
}

func (e *SleepEvent) GetEventType() string {
	return "SLEEP"
}

func (e *SleepEvent) BuildSignal() Signal {
	return NewSleepSignal(e.Key, e.Until, e.Branches, e.Version)
}
