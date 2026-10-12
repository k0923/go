package flow

import (
	"encoding/json"
	"time"
)

var _ Signal = (*WaitForSignal)(nil)
var _ Signal = (*SleepSignal)(nil)
var _ Signal = (*BranchSignal)(nil)

type Signal interface {
	GetKey() string
	GetVersion() int
}

func NewWaitForSignal(key string, request json.RawMessage, response json.RawMessage, version int) Signal {
	return &WaitForSignal{
		Key:      key,
		Version:  version,
		Request:  request,
		Response: response,
	}
}

func NewSleepSignal(key string, until time.Time, version int) Signal {
	return &SleepSignal{
		Key:     key,
		Until:   until,
		Version: version,
	}
}

type WaitForSignal struct {
	Key      string
	Branches []string
	Version  int
	Request  json.RawMessage
	Response json.RawMessage
}

func (s *WaitForSignal) GetKey() string {
	return s.Key
}

func (s *WaitForSignal) GetVersion() int {
	return s.Version
}

type SleepSignal struct {
	Key      string
	Until    time.Time
	Branches []string
	Version  int
}

func (s *SleepSignal) GetKey() string {
	return s.Key
}

func (s *SleepSignal) GetVersion() int {
	return s.Version
}

type BranchSignal map[string]any

func (s BranchSignal) GetKey() string {
	return "branch"
}

func (s BranchSignal) GetVersion() int {
	return 0
}
