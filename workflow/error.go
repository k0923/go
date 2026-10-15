package workflow

import (
	"fmt"
)

func NewConflictError(event Event) *ConflictError {
	return &ConflictError{
		Event: event,
	}
}

type ConflictError struct {
	Event Event
}

func (err *ConflictError) Error() string {
	return fmt.Sprintf("conflict: %s", err.Event.GetKey())
}

func NewContextError(event Event, err error) *ContextError {
	return &ContextError{
		Event: event,
		Cause: err,
	}
}

type ContextError struct {
	Event Event
	Cause error
}

func (err *ContextError) Error() string {
	if err == nil {
		return ""
	}
	message := "context error"
	if err.Event != nil {
		message = fmt.Sprintf("%s: %s", message, err.Event.GetKey())
	}
	if err.Cause != nil {
		return fmt.Sprintf("%s: %s", message, err.Cause.Error())
	}
	return message
}

func (err *ContextError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

type BizError struct {
	Event Event
	Cause error
}

func (err *BizError) Error() string {
	if err == nil {
		return ""
	}
	if err.Event != nil {
		return fmt.Sprintf("biz error: %s", err.Event.GetKey())
	}
	if err.Cause != nil {
		return err.Cause.Error()
	}
	return "biz error"
}

func NewBizError(event Event, err error) *BizError {
	return &BizError{
		Event: event,
		Cause: err,
	}
}

type LegacyBizError struct {
	Event Event
	Cause error
}

func (err *LegacyBizError) Error() string {
	if err == nil {
		return ""
	}
	if err.Event != nil {
		return fmt.Sprintf("legacy biz error: %s", err.Event.GetKey())
	}
	if err.Cause != nil {
		return err.Cause.Error()
	}
	return "legacy biz error"
}

func NewLegacyBizError(event Event, err error) *LegacyBizError {
	return &LegacyBizError{
		Event: event,
		Cause: err,
	}
}

type SystemError struct {
	Event Event
	Cause error
}

func (err *SystemError) Error() string {
	if err == nil {
		return ""
	}
	if err.Event != nil {
		return fmt.Sprintf("system error: %s", err.Event.GetKey())
	}
	if err.Cause != nil {
		return err.Cause.Error()
	}
	return "system error"
}

func (err *SystemError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func NewSystemError(event Event, err error) *SystemError {
	return &SystemError{
		Event: event,
		Cause: err,
	}
}

type InfrastructureMethod string

const (
	InfrastructureMethodAddEvent      InfrastructureMethod = "AddEvent"
	InfrastructureMethodGetEvent      InfrastructureMethod = "GetEvent"
	InfrastructureMethodUpdateEvent   InfrastructureMethod = "UpdateEvent"
	InfrastructureMethodSuspend       InfrastructureMethod = "Suspend"
	InfrastructureMethodResolveSignal InfrastructureMethod = "ResolveSignal"
	InfrastructureMethodGetState      InfrastructureMethod = "GetState"
	InfrastructureMethodSetState      InfrastructureMethod = "SetState"
	InfrastructureMethodClaimLease    InfrastructureMethod = "ClaimLease"
	InfrastructureMethodRenewLease    InfrastructureMethod = "RenewLease"
	InfrastructureMethodReleaseLease  InfrastructureMethod = "ReleaseLease"
)

type InfrastructureError struct {
	Event  Event
	Cause  error
	Method InfrastructureMethod
}

func (err *InfrastructureError) Error() string {
	if err == nil {
		return ""
	}
	message := "infrastructure error"
	if err.Method != "" {
		message = fmt.Sprintf("%s: %s", message, err.Method)
	}
	if err.Event != nil {
		message = fmt.Sprintf("%s %s", message, err.Event.GetKey())
	}
	if err.Cause != nil {
		return fmt.Sprintf("%s: %s", message, err.Cause.Error())
	}
	return message
}

func NewInfrastructureError(event Event, err error, method InfrastructureMethod) *InfrastructureError {
	return &InfrastructureError{
		Event:  event,
		Cause:  err,
		Method: method,
	}
}

type MarshalError struct {
	Event Event
	Cause error
}

func (err *MarshalError) Error() string {
	if err == nil {
		return ""
	}
	if err.Event != nil {
		return fmt.Sprintf("marshal error: %s", err.Event.GetKey())
	}
	if err.Cause != nil {
		return err.Cause.Error()
	}
	return "marshal error"
}

func (err *MarshalError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func NewMarshalError(event Event, err error) *MarshalError {
	return &MarshalError{
		Event: event,
		Cause: err,
	}
}

type UnMarshalError struct {
	Event Event
	Cause error
}

func (err *UnMarshalError) Error() string {
	if err == nil {
		return ""
	}
	if err.Event != nil {
		return fmt.Sprintf("unmarshal error: %s", err.Event.GetKey())
	}
	if err.Cause != nil {
		return err.Cause.Error()
	}
	return "unmarshal error"
}

func (err *UnMarshalError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func NewUnMarshalError(event Event, err error) *UnMarshalError {
	return &UnMarshalError{
		Event: event,
		Cause: err,
	}
}

func NewUnknownError(cause any) *UnknownError {
	return &UnknownError{
		Cause: cause,
	}
}

type UnknownError struct {
	Cause any
}

func (err *UnknownError) Error() string {
	return fmt.Sprintf("unknown error: %v", err.Cause)
}
