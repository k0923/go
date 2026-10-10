package flow

import (
	"encoding/json"
	"time"
)

type Event interface {
	GetEventType() string
	GetName() string
	GetBranches() []string
}

type BaseEvent struct {
	Name     string
	Ts       time.Time
	Branches []string
}

func (e *BaseEvent) GetName() string {
	return e.Name
}

func (e *BaseEvent) GetBranches() []string {
	return e.Branches
}

type StepStartEvent struct {
	BaseEvent
}

func (e *StepStartEvent) GetEventType() string {
	return "STEP_START"
}

type StepEndEvent struct {
	BaseEvent
	Result json.RawMessage
}

func (e *StepEndEvent) GetEventType() string {
	return "STEP_END"
}

type StepFailedEvent struct {
	BaseEvent
	Error string
}

func (e *StepFailedEvent) GetEventType() string {
	return "STEP_FAILED"
}

type WaitForEvent struct {
	BaseEvent
	Request json.RawMessage
}

func (e *WaitForEvent) GetEventType() string {
	return "WAIT_FOR"
}

type WaitForResultEvent struct {
	BaseEvent
	Response json.RawMessage
}

func (e *WaitForResultEvent) GetEventType() string {
	return "WAIT_FOR_RESULT"
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
