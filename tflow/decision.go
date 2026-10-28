package tflow

import (
	"context"
	"fmt"
)

type MainAction string

const (
	Continue MainAction = "CONTINUE"
	Succeed  MainAction = "SUCCEED"
	Fail     MainAction = "FAIL"
	Cancel   MainAction = "CANCEL"
)

// MainDecision specifies business lifecycle intent. Succeed requires exhausted
// planning and all units terminal; Fail/Cancel can stop outstanding work.
type MainDecision struct {
	Action MainAction
	Reason string
}

// MainDecisionView exposes scheduling facts for a single bound main task.
// MainProgress implements it with value snapshots; task identity and business
// decision inputs belong to the persisted MainTask.
type MainDecisionView interface {
	State() MainState
	Summary() SubSummary
	ReadyToComplete() bool
}

// MainDecider can be implemented by MainTask itself (preferred for business
// rules), or persisted in MainConfig. It runs outside transactions, must not
// mutate the task/view or perform side effects, and may run more than once.
// Changes to facts used by a decision must advance the same main Version.
type MainDecider interface {
	DecideMain(context.Context, MainDecisionView) (MainDecision, error)
}

type MainDeciderFunc func(context.Context, MainDecisionView) (MainDecision, error)

func (f MainDeciderFunc) DecideMain(ctx context.Context, view MainDecisionView) (MainDecision, error) {
	return f(ctx, view)
}

// DefaultMainDecider preserves fail-before-cancel-before-success completion.
// Custom business deciders may tolerate failures or stop early.
type DefaultMainDecider struct{}

func (DefaultMainDecider) DecideMain(_ context.Context, view MainDecisionView) (MainDecision, error) {
	if !view.ReadyToComplete() {
		return MainDecision{Action: Continue}, nil
	}
	summary := view.Summary()
	if summary.Failed > 0 {
		return MainDecision{Action: Fail}, nil
	}
	if summary.Canceled > 0 {
		return MainDecision{Action: Cancel}, nil
	}
	return MainDecision{Action: Succeed}, nil
}

var _ MainDecisionView = MainProgress{}

func (e *Engine) decideMain(ctx context.Context, m MainRecord) (decision MainDecision, err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("decide main %s panicked: %v", e.MainID(), value)
		}
	}()
	decider := m.MainDecider
	if taskDecider, ok := m.Task.(MainDecider); ok {
		decider = taskDecider
	}
	if IsNil(decider) {
		return MainDecision{}, fmt.Errorf("%w: missing MainDecider", ErrInvalid)
	}
	decision, err = decider.DecideMain(ctx, m.MainProgress)
	if err != nil {
		return MainDecision{}, fmt.Errorf("decide main %s: %w", e.MainID(), err)
	}
	switch decision.Action {
	case Continue, Fail, Cancel:
	case Succeed:
		if !m.ReadyToComplete() {
			return MainDecision{}, fmt.Errorf("%w: Succeed requires exhausted planning and all units terminal", ErrInvalid)
		}
	default:
		return MainDecision{}, fmt.Errorf("%w: unknown main action %q", ErrInvalid, decision.Action)
	}
	return decision, nil
}

func (e *Engine) commitDecision(ctx context.Context, m MainRecord, decision MainDecision) error {
	status := MainFailed
	switch decision.Action {
	case Succeed:
		status = MainSucceeded
	case Cancel:
		status = MainCanceled
	}
	if m.ReadyToComplete() {
		return e.complete(ctx, m.Version, status, decision.Reason)
	}
	_, err := e.stop(ctx, status, decision.Reason, m.Version)
	return err
}
