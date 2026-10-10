package flow

import (
	"encoding/json"
	"time"
)

type Signal interface {
	innerSignal() bool
}

type WaitForSignal struct {
	Name     string
	Branches []string
	Request  json.RawMessage
	Response json.RawMessage
}

func (s WaitForSignal) innerSignal() bool {
	return false
}

type SleepSignal struct {
	Name     string
	Duration time.Duration
	Branches []string
}

func (s SleepSignal) innerSignal() bool {
	return false
}

type conflictEventSignal struct {
	Event Event
}

func (s conflictEventSignal) innerSignal() bool {
	return true
}
