package flow

import (
	"errors"
)

var (
	ErrContextDone          = errors.New("flow context done")
	ErrWaitForEventNotFound = errors.New("wait for event not found")
	ErrBranchKeyRequired    = errors.New("parallel branch key is required")
	ErrDuplicateBranchKey   = errors.New("parallel branch key must be unique")
	ErrConflict             = errors.New("conflict")
)

func IsConflictError(err error) bool {
	return errors.Is(err, ErrConflict)
}

type BizError struct {
	Message string
	Cause   error
}

func (err *BizError) Error() string {
	if err == nil {
		return ""
	}
	if err.Message != "" {
		return err.Message
	}
	if err.Cause != nil {
		return err.Cause.Error()
	}
	return "business error"
}

func (err *BizError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func NewBizError(message string, causes ...error) *BizError {
	return &BizError{
		Message: message,
		Cause:   errors.Join(causes...),
	}
}

type SystemError struct {
	Message string
	Cause   error
}

func (err *SystemError) Error() string {
	if err == nil {
		return ""
	}
	if err.Message != "" {
		return err.Message
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

func NewSystemError(message string, causes ...error) *SystemError {
	return &SystemError{
		Message: message,
		Cause:   errors.Join(causes...),
	}
}

type InfrastructureError struct {
	Message string
	Cause   error
}

func (err *InfrastructureError) Error() string {
	if err == nil {
		return ""
	}
	if err.Message != "" {
		return err.Message
	}
	if err.Cause != nil {
		return err.Cause.Error()
	}
	return "infrastructure error"
}

func (err *InfrastructureError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func NewInfrastructureError(message string, causes ...error) *InfrastructureError {
	return &InfrastructureError{
		Message: message,
		Cause:   errors.Join(causes...),
	}
}

type commonValue[Value any] struct {
	Value Value
}
