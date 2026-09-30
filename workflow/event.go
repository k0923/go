package workflow

import (
	"encoding/json"
	"time"
)

var _ Event = (*StepStartEvent)(nil)
var _ Event = (*StepFinishEvent)(nil)
var _ Event = (*StepFailedEvent)(nil)
var _ Event = (*SignalAwaitedEvent)(nil)
var _ Event = (*SignalResolvedEvent)(nil)
var _ Event = (*SleepScheduledEvent)(nil)
var _ Event = (*ParallelStartedEvent)(nil)
var _ Event = (*BranchCompletedEvent)(nil)
var _ Event = (*ParallelCompletedEvent)(nil)

const (
	EventTypeStepStart         = "STEP_START"
	EventTypeStepFinish        = "STEP_FINISH"
	EventTypeStepFailed        = "STEP_FAILED"
	EventTypeSignalAwaited     = "SIGNAL_AWAITED"
	EventTypeSignalResolved    = "SIGNAL_RESOLVED"
	EventTypeSleepScheduled    = "SLEEP_SCHEDULED"
	EventTypeParallelStarted   = "PARALLEL_STARTED"
	EventTypeBranchCompleted   = "BRANCH_COMPLETED"
	EventTypeParallelCompleted = "PARALLEL_COMPLETED"
)

type EventIdentity struct {
	Name      string   `json:"name"`
	BranchIds []string `json:"branchIds,omitempty"`
	Type      string   `json:"type"`
}

func (identity EventIdentity) Equal(other EventIdentity) bool {
	if identity.Name != other.Name ||
		identity.Type != other.Type ||
		len(identity.BranchIds) != len(other.BranchIds) {
		return false
	}
	for index := range identity.BranchIds {
		if identity.BranchIds[index] != other.BranchIds[index] {
			return false
		}
	}
	return true
}

type EventBase struct {
	Name      string         `json:"name"`
	BranchIds []string       `json:"branchIds,omitempty"`
	Previous  *EventIdentity `json:"previous,omitempty"`
	Ts        time.Time      `json:"ts"`
}

func (e *EventBase) GetBase() *EventBase {
	return e
}

type Event interface {
	GetType() string
	GetBase() *EventBase
}

func IdentityOf(event Event) EventIdentity {
	return EventIdentity{
		Name:      event.GetBase().Name,
		BranchIds: append([]string(nil), event.GetBase().BranchIds...),
		Type:      event.GetType(),
	}
}

func sameEventIdentity(left Event, right Event) bool {
	return IdentityOf(left).Equal(IdentityOf(right))
}

type StepStartEvent struct {
	EventBase
}

func (e *StepStartEvent) GetType() string {
	return EventTypeStepStart
}

type StepFinishEvent struct {
	EventBase
	Result json.RawMessage `json:"result"`
}

func (e *StepFinishEvent) GetType() string {
	return EventTypeStepFinish
}

type StepError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

type StepAttempt struct {
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Result     any       `json:"result"`
	Error      StepError `json:"error"`
}

type StepFailedEvent struct {
	EventBase
	Error    StepError     `json:"error"`
	Attempts []StepAttempt `json:"attempts"`
}

func (e *StepFailedEvent) GetType() string {
	return EventTypeStepFailed
}

type SignalAwaitedEvent struct {
	EventBase
}

func (e *SignalAwaitedEvent) GetType() string {
	return EventTypeSignalAwaited
}

type SignalResolvedEvent struct {
	EventBase
	Result json.RawMessage `json:"result"`
}

func (e *SignalResolvedEvent) GetType() string {
	return EventTypeSignalResolved
}

type SleepScheduledEvent struct {
	EventBase
	Until              time.Time     `json:"until"`
	InProcessThreshold time.Duration `json:"inProcessThreshold"`
	Suspended          bool          `json:"suspended"`
}

func (e *SleepScheduledEvent) GetType() string {
	return EventTypeSleepScheduled
}

type ParallelStartedEvent struct {
	EventBase
	Branches []string `json:"branches"`
}

func (e *ParallelStartedEvent) GetType() string {
	return EventTypeParallelStarted
}

type BranchCompletedEvent struct {
	EventBase
	BranchID string `json:"branchId"`
}

func (e *BranchCompletedEvent) GetType() string {
	return EventTypeBranchCompleted
}

type ParallelCompletedEvent struct {
	EventBase
}

func (e *ParallelCompletedEvent) GetType() string {
	return EventTypeParallelCompleted
}
