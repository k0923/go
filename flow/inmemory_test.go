package flow

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type unsupportedEvent struct {
	BaseEvent
}

func (event *unsupportedEvent) GetEventType() string {
	return "UNSUPPORTED"
}

type unsupportedSignal struct {
	key     string
	version int
}

func (signal *unsupportedSignal) GetKey() string {
	return signal.key
}

func (signal *unsupportedSignal) GetVersion() int {
	return signal.version
}

func TestInMemoryInfrastructureEventCAS_BitsUT(t *testing.T) {
	var infrastructure InMemoryInfrastructure
	ctx := context.Background()
	event := &StepResultEvent{
		BaseEvent: BaseEvent{
			Key:    "step",
			Status: FlowStatusRunning,
		},
	}

	added, err := infrastructure.AddEvent(ctx, event)
	if err != nil {
		t.Fatalf("add event failed: %v", err)
	}
	current := added.(*StepResultEvent)
	if current.Version != 1 {
		t.Fatalf("expected version 1 after add, got %d", current.Version)
	}

	stale := *current
	current.Status = FlowStatusCompleted
	current.Result = json.RawMessage(`"done"`)
	updated, err := infrastructure.UpdateEvent(ctx, current)
	if err != nil {
		t.Fatalf("update event failed: %v", err)
	}
	finished := updated.(*StepResultEvent)
	if finished.Version != 2 {
		t.Fatalf("expected version 2 after update, got %d", finished.Version)
	}

	stale.Status = FlowStatusFailed
	rejected, err := infrastructure.UpdateEvent(ctx, &stale)
	if err != nil {
		t.Fatalf("stale update returned an error: %v", err)
	}
	if rejected != nil {
		t.Fatal("expected stale update to be rejected")
	}

	finished.Result[1] = 'X'
	stored, err := infrastructure.GetEvent(ctx, "step")
	if err != nil {
		t.Fatalf("get event failed: %v", err)
	}
	if string(stored.(*StepResultEvent).Result) != `"done"` {
		t.Fatalf("stored event was mutated through returned value: %s", stored.(*StepResultEvent).Result)
	}

	duplicate, err := infrastructure.AddEvent(ctx, &SleepEvent{
		BaseEvent: BaseEvent{Key: "step", Status: FlowStatusRunning},
	})
	if err != nil {
		t.Fatalf("add duplicate key failed: %v", err)
	}
	if duplicate != nil {
		t.Fatal("expected an event key to be unique across event types")
	}
}

func TestInMemoryInfrastructureResolvesSignals_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure()
	ctx := context.Background()
	added, err := infrastructure.AddEvent(ctx, &WaitForSignalEvent{
		BaseEvent: BaseEvent{
			Key:    "approval",
			Status: FlowStatusRunning,
		},
		Request: json.RawMessage(`"request"`),
	})
	if err != nil {
		t.Fatalf("add wait event failed: %v", err)
	}
	wait := added.(*WaitForSignalEvent)
	signal := NewWaitForSignal(
		wait.Key,
		wait.Request,
		json.RawMessage(`"approved"`),
		wait.Version,
	)

	resolved, err := infrastructure.ResolveSignal(ctx, signal)
	if err != nil {
		t.Fatalf("resolve signal failed: %v", err)
	}
	if !resolved {
		t.Fatal("expected signal to resolve")
	}
	resolvedAgain, err := infrastructure.ResolveSignal(ctx, signal)
	if err != nil {
		t.Fatalf("resolve duplicate signal failed: %v", err)
	}
	if resolvedAgain {
		t.Fatal("expected duplicate signal to be rejected")
	}

	stored, err := infrastructure.GetEvent(ctx, wait.Key)
	if err != nil {
		t.Fatalf("get resolved event failed: %v", err)
	}
	result := stored.(*WaitForSignalEvent)
	if result.Status != FlowStatusCompleted ||
		result.Version != wait.Version+1 ||
		string(result.Response) != `"approved"` {
		t.Fatalf("unexpected resolved event: %+v", result)
	}

	signal.(*WaitForSignal).Response[1] = 'X'
	stored, err = infrastructure.GetEvent(ctx, wait.Key)
	if err != nil {
		t.Fatalf("get resolved event after signal mutation failed: %v", err)
	}
	if string(stored.(*WaitForSignalEvent).Response) != `"approved"` {
		t.Fatalf("stored response was mutated through signal: %s", stored.(*WaitForSignalEvent).Response)
	}
}

func TestInMemoryInfrastructureUsesStoredSleepDeadline_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure()
	ctx := context.Background()

	future, err := infrastructure.AddEvent(ctx, &SleepEvent{
		BaseEvent: BaseEvent{
			Key:    "future-sleep",
			Status: FlowStatusRunning,
		},
		Until: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("add future sleep event failed: %v", err)
	}
	futureSleep := future.(*SleepEvent)
	resolved, err := infrastructure.ResolveSignal(
		ctx,
		NewSleepSignal(futureSleep.Key, time.Now().Add(-time.Hour), futureSleep.Version),
	)
	if err != nil {
		t.Fatalf("resolve future sleep failed: %v", err)
	}
	if resolved {
		t.Fatal("expected persisted future deadline to prevent early resolution")
	}

	due, err := infrastructure.AddEvent(ctx, &SleepEvent{
		BaseEvent: BaseEvent{
			Key:    "due-sleep",
			Status: FlowStatusRunning,
		},
		Until: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("add due sleep event failed: %v", err)
	}
	dueSleep := due.(*SleepEvent)
	resolved, err = infrastructure.ResolveSignal(
		ctx,
		NewSleepSignal(dueSleep.Key, time.Now().Add(time.Hour), dueSleep.Version),
	)
	if err != nil {
		t.Fatalf("resolve sleep failed: %v", err)
	}
	if !resolved {
		t.Fatal("expected due sleep to resolve")
	}
	stored, err := infrastructure.GetEvent(ctx, dueSleep.Key)
	if err != nil {
		t.Fatalf("get sleep event failed: %v", err)
	}
	if stored.GetStatus() != FlowStatusCompleted ||
		stored.GetVersion() != dueSleep.Version+1 {
		t.Fatalf("unexpected sleep event: %+v", stored)
	}
}

func TestInMemoryInfrastructureAddsEventAtomically_BitsUT(t *testing.T) {
	var infrastructure InMemoryInfrastructure
	ctx := context.Background()
	const workers = 32
	var accepted atomic.Int64
	var waitGroup sync.WaitGroup
	errors := make(chan error, workers)

	waitGroup.Add(workers)
	for range workers {
		go func() {
			defer waitGroup.Done()
			result, err := infrastructure.AddEvent(ctx, &CheckpointEvent{
				BaseEvent: BaseEvent{
					Key:    "checkpoint",
					Status: FlowStatusCompleted,
				},
				Data: json.RawMessage(`"value"`),
			})
			if err != nil {
				errors <- err
				return
			}
			if result != nil {
				accepted.Add(1)
			}
		}()
	}
	waitGroup.Wait()
	close(errors)
	for err := range errors {
		t.Fatalf("concurrent add failed: %v", err)
	}
	if accepted.Load() != 1 {
		t.Fatalf("expected one accepted add, got %d", accepted.Load())
	}
}

func TestInMemoryInfrastructureUpdatesEventAtomically_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure()
	ctx := context.Background()
	added, err := infrastructure.AddEvent(ctx, &CheckpointEvent{
		BaseEvent: BaseEvent{
			Key:    "checkpoint",
			Status: FlowStatusRunning,
		},
	})
	if err != nil {
		t.Fatalf("add event failed: %v", err)
	}

	const workers = 32
	var accepted atomic.Int64
	var waitGroup sync.WaitGroup
	errs := make(chan error, workers)
	waitGroup.Add(workers)
	for i := range workers {
		go func() {
			defer waitGroup.Done()
			candidate := *(added.(*CheckpointEvent))
			candidate.Data = json.RawMessage(string(rune('a' + i%26)))
			result, updateErr := infrastructure.UpdateEvent(ctx, &candidate)
			if updateErr != nil {
				errs <- updateErr
				return
			}
			if result != nil {
				accepted.Add(1)
			}
		}()
	}
	waitGroup.Wait()
	close(errs)
	for updateErr := range errs {
		t.Fatalf("concurrent update failed: %v", updateErr)
	}
	if accepted.Load() != 1 {
		t.Fatalf("expected one accepted update, got %d", accepted.Load())
	}
}

func TestInMemoryInfrastructureSnapshotsStateSignalsAndErrors_BitsUT(t *testing.T) {
	var infrastructure InMemoryInfrastructure
	ctx := context.Background()
	state := State{
		Input:  json.RawMessage(`{"value":1}`),
		Output: json.RawMessage(`"done"`),
		Status: FlowStatusCompleted,
		Errors: []string{"first"},
	}
	if err := infrastructure.SetState(ctx, state); err != nil {
		t.Fatalf("set state failed: %v", err)
	}
	state.Input[0] = '['
	stored, err := infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get state failed: %v", err)
	}
	if string(stored.Input) != `{"value":1}` {
		t.Fatalf("stored state was mutated through input: %s", stored.Input)
	}

	signal := NewWaitForSignal(
		"approval",
		json.RawMessage(`"request"`),
		json.RawMessage(`"response"`),
		1,
	)
	if err := infrastructure.RaiseSignal(ctx, signal); err != nil {
		t.Fatalf("raise signal failed: %v", err)
	}
	signal.(*WaitForSignal).Request[1] = 'X'
	signals := infrastructure.Signals()
	if len(signals) != 1 ||
		string(signals[0].(*WaitForSignal).Request) != `"request"` {
		t.Fatalf("unexpected signal snapshot: %+v", signals)
	}
	signals[0].(*WaitForSignal).Response[1] = 'X'
	if got := infrastructure.Signals(); string(got[0].(*WaitForSignal).Response) != `"response"` {
		t.Fatalf("stored signal was mutated through snapshot: %+v", got[0])
	}

	infrastructure.OnError(context.Canceled)
	infrastructure.OnError(nil)
	errs := infrastructure.Errors()
	if len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("unexpected errors snapshot: %v", errs)
	}
}

func TestInMemoryInfrastructureRejectsCanceledContextAndUnsupportedTypes_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := infrastructure.AddEvent(ctx, &CheckpointEvent{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled add, got %v", err)
	}
	if _, err := infrastructure.GetEvent(ctx, "event"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled get, got %v", err)
	}
	if _, err := infrastructure.UpdateEvent(ctx, &CheckpointEvent{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled update, got %v", err)
	}
	if err := infrastructure.RaiseSignal(ctx, NewSleepSignal("sleep", time.Now(), 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled raise, got %v", err)
	}
	if _, err := infrastructure.ResolveSignal(ctx, NewSleepSignal("sleep", time.Now(), 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled resolve, got %v", err)
	}
	if _, err := infrastructure.GetState(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled get state, got %v", err)
	}
	if err := infrastructure.SetState(ctx, State{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled set state, got %v", err)
	}

	background := context.Background()
	if _, err := infrastructure.AddEvent(background, &unsupportedEvent{}); err == nil {
		t.Fatal("expected unsupported event add to fail")
	}
	if err := infrastructure.RaiseSignal(background, &unsupportedSignal{}); err == nil {
		t.Fatal("expected unsupported signal raise to fail")
	}
	if resolved, err := infrastructure.ResolveSignal(background, &unsupportedSignal{}); err != nil || resolved {
		t.Fatalf("expected unsupported signal resolution to be ignored, got resolved=%v err=%v", resolved, err)
	}
}
