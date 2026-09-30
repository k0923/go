package workflow

import (
	"encoding/json"
	"slices"
	"time"
)

type Validator[T any] func(data T) error

type Workflow[Input any, Output any] struct {
	ID             string
	Handler        func(ctx *WorkflowContext[Input, Output]) (Output, error)
	InputValidator Validator[Input]
}

type WorkflowOption[Input any, Output any] struct {
	RunID          string
	Input          Input
	Store          WorkflowStore[Input, Output]
	LeaseDuration  time.Duration
	InputValidator Validator[Input]
	Workflow       Workflow[Input, Output]
}

type StartOption[Input any, Output any] = WorkflowOption[Input, Output]

type ResumeRunOption[Input any, Output any] struct {
	RunID         string
	ID            string
	Store         WorkflowStore[Input, Output]
	Callback      *SignalCallback
	BranchIDs     []string
	LeaseDuration time.Duration
	Workflow      Workflow[Input, Output]
}

type ResumeOption[Input any, Output any] = ResumeRunOption[Input, Output]

type SignalCallback struct {
	Name      string          `json:"name"`
	BranchIDs []string        `json:"branch_ids,omitempty"`
	Result    json.RawMessage `json:"result"`
}

func RunWorkflow[Input any, Output any](option WorkflowOption[Input, Output]) (err error) {
	handler := option.Workflow.Handler
	if option.Workflow.Handler != nil {
		handler = option.Workflow.Handler
	}
	validator := option.InputValidator
	if option.Workflow.InputValidator != nil {
		validator = option.Workflow.InputValidator
	}
	if validator != nil {
		if err := validator(option.Input); err != nil {
			return err
		}
	}
	runtime, err := newRuntime(
		option.RunID,
		option.Input,
		option.Store,
		option.LeaseDuration,
	)
	if err != nil {
		return err
	}
	return executeWorkflow(runtime, handler)
}

func ResumeRun[Input any, Output any](option ResumeRunOption[Input, Output]) error {
	runID := option.RunID
	if runID == "" {
		runID = option.ID
	}
	ownerID := newRuntimeID()
	var lease ExecutionLease
	var err error
	if option.Callback != nil {
		events, err := option.Store.GetEvents(runID)
		if err != nil {
			return err
		}
		awaitedEvent := findAwaitedSignal(events, option.Callback)
		if awaitedEvent == nil {
			return ErrAwaitedSignalNotFound
		}
		awaitedIdentity := IdentityOf(awaitedEvent)
		resolvedEvent := &SignalResolvedEvent{
			EventBase: EventBase{
				Name:      option.Callback.Name,
				BranchIds: append([]string(nil), awaitedEvent.BranchIds...),
				Previous:  &awaitedIdentity,
				Ts:        time.Now(),
			},
			Result: option.Callback.Result,
		}
		lease, err = option.Store.ResolveSignal(
			runID,
			awaitedIdentity,
			resolvedEvent,
			ownerID,
			option.LeaseDuration,
		)
	} else if option.BranchIDs != nil {
		lease, err = option.Store.ClaimContinuation(
			ContinuationKey{
				RunID:     runID,
				BranchIDs: append([]string(nil), option.BranchIDs...),
			},
			ownerID,
			option.LeaseDuration,
		)
	} else {
		lease, err = option.Store.ClaimNextContinuation(
			runID,
			ownerID,
			option.LeaseDuration,
		)
	}
	if err != nil {
		return err
	}
	runtime, err := resumeRuntime(runID, option.Store, lease, option.LeaseDuration)
	if err != nil {
		_ = option.Store.ReleaseContinuation(lease)
		return err
	}

	handler := option.Workflow.Handler
	if option.Workflow.Handler != nil {
		handler = option.Workflow.Handler
	}
	return executeWorkflow(runtime, handler)
}

func findAwaitedSignal(events []Event, callback *SignalCallback) *SignalAwaitedEvent {
	for _, event := range events {
		awaited, ok := event.(*SignalAwaitedEvent)
		if ok &&
			awaited.Name == callback.Name &&
			(callback.BranchIDs == nil || slices.Equal(awaited.BranchIds, callback.BranchIDs)) {
			return awaited
		}
	}
	return nil
}
