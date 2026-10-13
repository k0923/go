package flow

// import (
// 	"encoding/json"
// 	"time"
// )

// var _ Signal = (*WaitForSignal)(nil)
// var _ Signal = (*SleepSignal)(nil)

// type Signal interface {
// 	GetKey() string
// 	GetVersion() int
// }

// func NewWaitForSignal(key string, request json.RawMessage, response json.RawMessage, version int) Signal {
// 	return &WaitForSignal{
// 		Key:      key,
// 		Version:  version,
// 		Request:  request,
// 		Response: response,
// 	}
// }

// func NewSleepSignal(key string, until time.Time, version int) Signal {
// 	return &SleepSignal{
// 		Key:     key,
// 		Until:   until,
// 		Version: version,
// 	}
// }

// type WaitForSignal struct {
// 	Event    *WaitForSignalEvent
// 	Key      string
// 	Version  int
// 	Request  json.RawMessage
// 	Response json.RawMessage
// }

// func (s *WaitForSignal) GetKey() string {
// 	return s.Key
// }

// func (s *WaitForSignal) GetVersion() int {
// 	return s.Version
// }

// type SleepSignal struct {
// 	Key      string
// 	Until    time.Time
// 	Version  int
// }

// func (s *SleepSignal) GetKey() string {
// 	return s.Key
// }

// func (s *SleepSignal) GetVersion() int {
// 	return s.Version
// }
