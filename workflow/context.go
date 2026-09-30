package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type StepContext struct {
	context.Context
	name string
}

func (ctx *StepContext) GetName() string {
	return ctx.name
}

type StepOption[Result any] struct {
	Handler func(ctx *StepContext) (Result, error)
}

type SignalOption struct {
	Meta map[string]any
}

type SleepOption struct {
	Name               string
	InProcessThreshold time.Duration
}

type ParallelOption struct {
	Name string
}

type Branch[Input any, Output any] struct {
	BranchID string
	Handler  func(ctx *WorkflowContext[Input, Output]) error
}

type parallelBranchResult struct {
	branchID           string
	remainingBranchIDs []string
	parentLease        *ExecutionLease
	err                error
	recovered          recoveredPanic
}

type WorkflowContext[Input any, Output any] struct {
	context.Context
	input        Input
	output       Output
	runtime      *workflowRuntime[Input, Output]
	currentEvent Event
	branchIds    []string
	lease        *ExecutionLease
}

func (ctx *WorkflowContext[Input, Output]) branch(
	branchID string,
	lease *ExecutionLease,
) *WorkflowContext[Input, Output] {
	newCtx := &WorkflowContext[Input, Output]{
		Context:      ctx.Context,
		input:        ctx.input,
		runtime:      ctx.runtime,
		currentEvent: ctx.currentEvent,
		branchIds:    append([]string(nil), ctx.branchIds...),
		lease:        lease,
	}
	newCtx.branchIds = append(newCtx.branchIds, branchID)
	return newCtx
}

func (ctx *WorkflowContext[Input, Output]) branchGroupID(name string) string {
	if len(ctx.branchIds) == 0 {
		return name
	}
	return name + ":" + strings.Join(ctx.branchIds, ".")
}

func (ctx *WorkflowContext[Input, Output]) BuildBaseEvent(name string) *EventBase {
	evtBase := &EventBase{
		Name:      name,
		BranchIds: append([]string(nil), ctx.branchIds...),
		Ts:        time.Now(),
	}
	if ctx.currentEvent != nil {
		previous := IdentityOf(ctx.currentEvent)
		evtBase.Previous = &previous
	}
	return evtBase
}

func (ctx *WorkflowContext[Input, Output]) requireLease() (ExecutionLease, error) {
	if ctx.lease == nil {
		return ExecutionLease{}, ErrContinuationLeaseInvalid
	}
	return *ctx.lease, nil
}

func (ctx *WorkflowContext[Input, Output]) appendEvent(event Event) error {
	lease, err := ctx.requireLease()
	if err != nil {
		return err
	}
	return ctx.runtime.AppendEvent(lease, event)
}

func (ctx *WorkflowContext[Input, Output]) Step[Result any](
	name string,
	option StepOption[Result],
) (Result, error) {
	var resultValue Result
	if name == "" {
		return resultValue, ErrStepNameRequired
	}
	if err := ctx.runtime.claimStepName(name, ctx.branchIds); err != nil {
		return resultValue, err
	}
	handler := option.Handler
	stepCtx := &StepContext{
		Context: ctx.Context,
		name:    name,
	}

	stepResultEvent, index := ctx.runtime.FindEvent(func(event *StepFinishEvent, index int) bool {
		return event.Name == name &&
			slices.Equal(event.BranchIds, ctx.branchIds)
	})
	if index >= 0 {
		ctx.currentEvent = stepResultEvent
		err := json.Unmarshal(stepResultEvent.Result, &resultValue)
		return resultValue, err
	}

	runStep := func(
		startedAt time.Time,
		previousAttempts []StepAttempt,
		failedBase *EventBase,
	) (Result, error) {
		if _, err := ctx.requireLease(); err != nil {
			return resultValue, err
		}
		result, err := handler(stepCtx)
		finishedAt := time.Now()
		if err != nil {
			evtBase := ctx.BuildBaseEvent(name)
			if failedBase != nil {
				evtBase.Previous = failedBase.Previous
			}
			evtBase.Ts = finishedAt
			stepFailedEvent := &StepFailedEvent{
				EventBase: *evtBase,
				Error:     newStepError(err),
				Attempts: append(previousAttempts, StepAttempt{
					StartedAt:  startedAt,
					FinishedAt: finishedAt,
					Error:      newStepError(err),
				}),
			}
			if saveErr := ctx.appendEvent(stepFailedEvent); saveErr != nil {
				return resultValue, errors.Join(err, saveErr)
			}
			ctx.currentEvent = stepFailedEvent
			return resultValue, err
		}

		resultBytes, err := json.Marshal(result)
		if err != nil {
			evtBase := ctx.BuildBaseEvent(name)
			if failedBase != nil {
				evtBase.Previous = failedBase.Previous
			}
			evtBase.Ts = finishedAt
			stepFailedEvent := &StepFailedEvent{
				EventBase: *evtBase,
				Error:     newStepError(err),
				Attempts: append(previousAttempts, StepAttempt{
					StartedAt:  startedAt,
					FinishedAt: finishedAt,
					Error:      newStepError(err),
				}),
			}
			if saveErr := ctx.appendEvent(stepFailedEvent); saveErr != nil {
				return resultValue, errors.Join(err, saveErr)
			}
			ctx.currentEvent = stepFailedEvent
			return resultValue, err
		}

		evtBase := ctx.BuildBaseEvent(name)
		evtBase.Ts = finishedAt
		stepFinishEvent := &StepFinishEvent{
			EventBase: *evtBase,
			Result:    resultBytes,
		}
		if err := ctx.appendEvent(stepFinishEvent); err != nil {
			return resultValue, err
		}
		ctx.currentEvent = stepFinishEvent
		return result, nil
	}

	stepFailedEvent, index := ctx.runtime.FindEvent(func(event *StepFailedEvent, index int) bool {
		return event.Name == name &&
			slices.Equal(event.BranchIds, ctx.branchIds)
	})
	if index >= 0 {
		attempts := append([]StepAttempt(nil), stepFailedEvent.Attempts...)
		ctx.currentEvent = stepFailedEvent
		return runStep(time.Now(), attempts, stepFailedEvent.GetBase())
	}

	stepStartedEvent, index := ctx.runtime.FindEvent(func(event *StepStartEvent, index int) bool {
		return event.Name == name &&
			slices.Equal(event.BranchIds, ctx.branchIds)
	})
	if index >= 0 {
		ctx.currentEvent = stepStartedEvent
		return runStep(stepStartedEvent.Ts, nil, nil)
	}

	stepStartEvent := &StepStartEvent{
		EventBase: *ctx.BuildBaseEvent(name),
	}
	if err := ctx.appendEvent(stepStartEvent); err != nil {
		return resultValue, err
	}
	ctx.currentEvent = stepStartEvent
	return runStep(stepStartEvent.Ts, nil, nil)
}

func (ctx *WorkflowContext[Input, Output]) WaitForEvent[Payload any](
	name string,
	option SignalOption,
) (Payload, error) {
	if name == "" {
		var payload Payload
		return payload, ErrSignalNameRequired
	}
	resolvedEvt, index := ctx.runtime.FindEvent(func(evt *SignalResolvedEvent, index int) bool {
		return evt.Name == name &&
			slices.Equal(evt.BranchIds, ctx.branchIds)
	})
	var payload Payload
	if index >= 0 {
		ctx.currentEvent = resolvedEvt
		err := json.Unmarshal(resolvedEvt.Result, &payload)
		return payload, err
	}

	awaitedEvt, index := ctx.runtime.FindEvent(func(evt *SignalAwaitedEvent, index int) bool {
		return evt.Name == name &&
			slices.Equal(evt.BranchIds, ctx.branchIds)
	})
	if index < 0 {
		evtBase := ctx.BuildBaseEvent(name)
		awaitedEvt = &SignalAwaitedEvent{
			EventBase: *evtBase,
		}
	}
	lease, err := ctx.requireLease()
	if err != nil {
		return payload, err
	}
	identity := IdentityOf(awaitedEvt)
	if err := ctx.runtime.SuspendContinuation(lease, awaitedEvt, Suspension{
		Kind:     SuspensionKindSignal,
		Event:    identity,
		Metadata: option.Meta,
	}); err != nil {
		return payload, err
	}
	ctx.currentEvent = awaitedEvt
	ctx.lease = nil
	panic(&awaitSignal{event: awaitedEvt})
}

func (ctx *WorkflowContext[Input, Output]) Sleep(
	duration time.Duration,
	option SleepOption,
) error {
	return ctx.SleepUntil(time.Now().Add(duration), option)
}

func (ctx *WorkflowContext[Input, Output]) SleepUntil(
	until time.Time,
	option SleepOption,
) error {
	if option.Name == "" {
		return ErrSleepNameRequired
	}
	if option.InProcessThreshold < 0 {
		return ErrSleepThresholdNegative
	}

	scheduledEvent, index := ctx.runtime.FindEvent(func(event *SleepScheduledEvent, index int) bool {
		return event.Name == option.Name &&
			slices.Equal(event.BranchIds, ctx.branchIds)
	})
	if index < 0 {
		evtBase := ctx.BuildBaseEvent(option.Name)
		scheduledEvent = &SleepScheduledEvent{
			EventBase:          *evtBase,
			Until:              until,
			InProcessThreshold: option.InProcessThreshold,
			Suspended:          until.Sub(evtBase.Ts) > option.InProcessThreshold,
		}
	}
	ctx.currentEvent = scheduledEvent

	remaining := time.Until(scheduledEvent.Until)
	if remaining <= 0 {
		if index < 0 {
			return ctx.appendEvent(scheduledEvent)
		}
		return nil
	}
	if remaining > scheduledEvent.InProcessThreshold {
		lease, err := ctx.requireLease()
		if err != nil {
			return err
		}
		if err := ctx.runtime.SuspendContinuation(lease, scheduledEvent, Suspension{
			Kind:   SuspensionKindSleep,
			Event:  IdentityOf(scheduledEvent),
			WakeAt: scheduledEvent.Until,
		}); err != nil {
			return err
		}
		ctx.lease = nil
		panic(&sleepSignal{event: scheduledEvent})
	}
	if index < 0 {
		if err := ctx.appendEvent(scheduledEvent); err != nil {
			return err
		}
	}

	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (ctx *WorkflowContext[Input, Output]) Parallel(
	option ParallelOption,
	branches ...Branch[Input, Output],
) error {
	if option.Name == "" {
		return ErrParallelNameRequired
	}

	branchesByID := make(map[string]Branch[Input, Output], len(branches))
	branchIDs := make([]string, 0, len(branches))
	for _, branch := range branches {
		if branch.BranchID == "" {
			return ErrBranchIDRequired
		}
		if _, exists := branchesByID[branch.BranchID]; exists {
			return fmt.Errorf("%w: %s", ErrDuplicateBranchID, branch.BranchID)
		}
		branchesByID[branch.BranchID] = branch
		branchIDs = append(branchIDs, branch.BranchID)
	}

	parallelCompletedEvent, index := ctx.runtime.FindEvent(func(event *ParallelCompletedEvent, index int) bool {
		return event.Name == option.Name &&
			slices.Equal(event.BranchIds, ctx.branchIds)
	})
	if index >= 0 {
		ctx.currentEvent = parallelCompletedEvent
		return nil
	}

	parallelStartedEvent, index := ctx.runtime.FindEvent(func(event *ParallelStartedEvent, index int) bool {
		return event.Name == option.Name &&
			slices.Equal(event.BranchIds, ctx.branchIds)
	})
	startedExists := index >= 0
	if startedExists && !sameBranchIDs(parallelStartedEvent.Branches, branchIDs) {
		return fmt.Errorf("%w: %s", ErrBranchDefinitionMismatch, ctx.branchGroupID(option.Name))
	}
	if !startedExists {
		parallelStartedEvent = &ParallelStartedEvent{
			EventBase: *ctx.BuildBaseEvent(option.Name),
			Branches:  append([]string(nil), branchIDs...),
		}
	}
	ctx.currentEvent = parallelStartedEvent
	parallelCompletedEvent = &ParallelCompletedEvent{
		EventBase: *ctx.BuildBaseEvent(option.Name),
	}
	branchGroupID := ctx.branchGroupID(option.Name)

	branchLeases := make(map[string]ExecutionLease, len(branches))
	if !startedExists {
		lease, err := ctx.requireLease()
		if err != nil {
			return err
		}
		forked, err := ctx.runtime.ForkParallel(
			lease,
			branchGroupID,
			parallelStartedEvent,
			branchIDs,
		)
		if err != nil {
			return err
		}
		if len(branchIDs) == 0 {
			if err := ctx.appendEvent(parallelCompletedEvent); err != nil {
				return err
			}
			ctx.currentEvent = parallelCompletedEvent
			return nil
		}
		ctx.lease = nil
		for _, branchLease := range forked.BranchLeases {
			branchID := branchLease.Key.BranchIDs[len(branchLease.Key.BranchIDs)-1]
			branchLeases[branchID] = branchLease
		}
	}

	branchesToRun := make([]Branch[Input, Output], 0, len(branches))
	for _, branch := range branches {
		if !startedExists {
			branchesToRun = append(branchesToRun, branch)
			continue
		}
		if ctx.runtime.targetsChild(ctx.branchIds, branch.BranchID) {
			branchesToRun = append(branchesToRun, branch)
		}
	}
	if len(branchesToRun) == 0 {
		panic(&branchSignal{})
	}

	runBranch := func(branch Branch[Input, Output]) (result parallelBranchResult) {
		result.branchID = branch.BranchID
		var branchLease *ExecutionLease
		if lease, exists := branchLeases[branch.BranchID]; exists {
			leaseCopy := lease
			branchLease = &leaseCopy
		} else if ctx.runtime.initialLeaseTargets(ctx.branchIds, branch.BranchID) {
			leaseCopy := ctx.runtime.initialLease
			branchLease = &leaseCopy
		}
		branchCtx := ctx.branch(branch.BranchID, branchLease)
		defer func() {
			result.recovered = classifyPanic(recover())
			if result.recovered.value != nil || result.err != nil {
				if branchCtx.lease != nil {
					result.err = errors.Join(
						result.err,
						ctx.runtime.ReleaseContinuation(*branchCtx.lease),
					)
					branchCtx.lease = nil
				}
				return
			}
			if branchCtx.lease == nil {
				return
			}
			branchCompletedEvent := &BranchCompletedEvent{
				EventBase: *branchCtx.BuildBaseEvent(option.Name),
				BranchID:  branch.BranchID,
			}
			completed, err := ctx.runtime.CompleteContinuation(
				*branchCtx.lease,
				branchGroupID,
				branchCompletedEvent,
				parallelCompletedEvent,
			)
			branchCtx.lease = nil
			result.remainingBranchIDs = completed.RemainingBranchIDs
			result.parentLease = completed.ParentLease
			result.err = errors.Join(result.err, err)
		}()
		result.err = branch.Handler(branchCtx)
		return result
	}

	resultCh := make(chan parallelBranchResult, len(branchesToRun))
	for _, branch := range branchesToRun {
		go func(branch Branch[Input, Output]) {
			resultCh <- runBranch(branch)
		}(branch)
	}

	results := make(map[string]parallelBranchResult, len(branchesToRun))
	for range branchesToRun {
		result := <-resultCh
		results[result.branchID] = result
	}

	var errs []error
	for _, branch := range branchesToRun {
		result := results[branch.BranchID]
		if result.recovered.value != nil && result.recovered.signal == nil {
			panic(result.recovered.value)
		}
		if result.err != nil {
			errs = append(errs, result.err)
		}
		if result.parentLease != nil {
			leaseCopy := *result.parentLease
			ctx.lease = &leaseCopy
			ctx.currentEvent = parallelCompletedEvent
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if ctx.lease == nil {
		panic(&branchSignal{})
	}
	return nil
}

func newStepError(err error) StepError {
	return StepError{
		Name:    fmt.Sprintf("%T", err),
		Message: err.Error(),
	}
}

func (ctx *WorkflowContext[Input, Output]) Approve() {
}
