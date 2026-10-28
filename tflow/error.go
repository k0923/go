package tflow

import (
	"errors"
	"fmt"
)

var ErrPermanent = errors.New("permanent planning failure")

// PlanError identifies a rejected protocol field while preserving errors.Is.
// Index is the zero-based task index, or -1 for a page-wide error.
type PlanError struct {
	MainID string
	Field  string
	Index  int
	Reason string
	Cause  error
}

func (e *PlanError) Error() string {
	field := e.Field
	if e.Index >= 0 {
		field = fmt.Sprintf("tasks[%d].%s", e.Index, field)
	}
	if e.MainID == "" {
		return fmt.Sprintf("tflow plan %s: %s: %v", field, e.Reason, e.Cause)
	}
	return fmt.Sprintf("tflow main %q plan %s: %s: %v", e.MainID, field, e.Reason, e.Cause)
}

func (e *PlanError) Unwrap() error { return e.Cause }

func invalidPlan(field string, index int, cause error, reason string) error {
	return &PlanError{Field: field, Index: index, Reason: reason, Cause: cause}
}

type permanentError struct{ cause error }

func (e *permanentError) Error() string        { return e.cause.Error() }
func (e *permanentError) Unwrap() error        { return e.cause }
func (e *permanentError) Is(target error) bool { return target == ErrPermanent }

// Permanent marks a planning error as non-retryable. Use for invalid definitions
// or source configuration, not temporary IO failures. Permanent(nil) is nil.
// Permanent 将规划错误标记为不可重试。用于无效定义或配置错误；
// 临时 IO 故障不要包装。Permanent(nil) 返回 nil。
func Permanent(err error) error {
	if err == nil || errors.Is(err, ErrPermanent) {
		return err
	}
	return &permanentError{cause: err}
}

func planPanic(value any) error {
	return Permanent(fmt.Errorf("planning panicked: %v", value))
}
