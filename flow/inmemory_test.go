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

func TestInMemoryInfrastructureEventCAS_BitsUT(t *testing.T) {
	var infrastructure InMemoryInfrastructure[struct{}, struct{}]
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

	duplicate, err := infrastructure.AddEvent(ctx, &SleepSignalEvent{
		BaseEvent: BaseEvent{Key: "step", Status: FlowStatusRunning},
	})
	if err != nil {
		t.Fatalf("add duplicate key failed: %v", err)
	}
	if duplicate != nil {
		t.Fatal("expected an event key to be unique across event types")
	}
}

func TestInMemoryInfrastructureClonesAsyncEvent_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, struct{}]()
	added, err := infrastructure.AddEvent(context.Background(), &AsyncEvent{
		BaseEvent: BaseEvent{
			Key:    "async",
			Status: FlowStatusCompleted,
		},
		Result: json.RawMessage(`"done"`),
	})
	if err != nil {
		t.Fatalf("add async event failed: %v", err)
	}

	asyncEvent := added.(*AsyncEvent)
	asyncEvent.Result[1] = 'X'
	stored, err := infrastructure.GetEvent(context.Background(), "async")
	if err != nil {
		t.Fatalf("get async event failed: %v", err)
	}
	if result := string(stored.(*AsyncEvent).Result); result != `"done"` {
		t.Fatalf("stored async event was mutated through returned value: %s", result)
	}
}

func TestInMemoryInfrastructureCapturesSignalEvents_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, struct{}]()
	ctx := context.Background()
	added, err := infrastructure.AddEvent(ctx, &WaitForSignalEvent{
		BaseEvent: BaseEvent{
			Key:    "approval",
			Status: FlowStatusSuspended,
		},
		Request: json.RawMessage(`"request"`),
	})
	if err != nil {
		t.Fatalf("add wait event failed: %v", err)
	}
	wait := added.(*WaitForSignalEvent)
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one captured signal, got %d", len(signals))
	}
	signal := signals[0].(*WaitForSignalEvent)
	if signal.Key != wait.Key ||
		signal.Version != wait.Version ||
		signal.Status != FlowStatusSuspended ||
		string(signal.Request) != `"request"` {
		t.Fatalf("unexpected signal snapshot: %+v", signal)
	}

	signal.Request[1] = 'X'
	if got := infrastructure.Signals(); string(got[0].(*WaitForSignalEvent).Request) != `"request"` {
		t.Fatalf("stored signal was mutated through snapshot: %+v", got[0])
	}

	duplicate, err := infrastructure.AddEvent(ctx, &WaitForSignalEvent{
		BaseEvent: BaseEvent{Key: "approval", Status: FlowStatusSuspended},
	})
	if err != nil {
		t.Fatalf("add duplicate signal failed: %v", err)
	}
	if duplicate != nil || len(infrastructure.Signals()) != 1 {
		t.Fatal("duplicate event must not emit another signal")
	}

	if _, err := infrastructure.AddEvent(ctx, &CheckpointEvent{
		BaseEvent: BaseEvent{Key: "checkpoint", Status: FlowStatusCompleted},
	}); err != nil {
		t.Fatalf("add checkpoint failed: %v", err)
	}
	if len(infrastructure.Signals()) != 1 {
		t.Fatal("non-signal event must not be captured as a signal")
	}
}

func TestInMemoryInfrastructureCapturesSleepSignal_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, struct{}]()
	ctx := context.Background()

	until := time.Now().Add(time.Hour)
	added, err := infrastructure.AddEvent(ctx, &SleepSignalEvent{
		BaseEvent: BaseEvent{
			Key:    "sleep",
			Status: FlowStatusSuspended,
		},
		Until: until,
	})
	if err != nil {
		t.Fatalf("add sleep signal failed: %v", err)
	}
	sleep := added.(*SleepSignalEvent)
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one sleep signal, got %d", len(signals))
	}
	signal := signals[0].(*SleepSignalEvent)
	if signal.Key != sleep.Key ||
		signal.Version != sleep.Version ||
		signal.Status != FlowStatusSuspended ||
		!signal.Until.Equal(until) {
		t.Fatalf("unexpected sleep signal: %+v", signal)
	}
}

func TestInMemoryInfrastructureAddsEventAtomically_BitsUT(t *testing.T) {
	var infrastructure InMemoryInfrastructure[struct{}, struct{}]
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
	infrastructure := NewInMemoryInfrastructure[struct{}, struct{}]()
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

func TestInMemoryInfrastructureSnapshotsStateAndErrors_BitsUT(t *testing.T) {
	var infrastructure InMemoryInfrastructure[json.RawMessage, json.RawMessage]
	ctx := context.Background()
	state := State[json.RawMessage, json.RawMessage]{
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

	infrastructure.OnError(context.Canceled)
	infrastructure.OnError(nil)
	errs := infrastructure.Errors()
	if len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("unexpected errors snapshot: %v", errs)
	}
}

func TestInMemoryInfrastructureRejectsCanceledContextAndUnsupportedTypes_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, struct{}]()
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
	if _, err := infrastructure.GetState(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled get state, got %v", err)
	}
	if err := infrastructure.SetState(ctx, State[struct{}, struct{}]{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled set state, got %v", err)
	}

	background := context.Background()
	if _, err := infrastructure.AddEvent(background, &unsupportedEvent{}); err == nil {
		t.Fatal("expected unsupported event add to fail")
	}
}

func TestInMemoryInfrastructureLeaseLifecycle(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, struct{}]()
	ctx := context.Background()

	lease, err := infrastructure.ClaimLease(ctx, "branch", time.Hour)
	if err != nil {
		t.Fatalf("claim lease failed: %v", err)
	}
	if lease == nil || lease.Generation != 1 {
		t.Fatalf("unexpected claimed lease: %+v", lease)
	}

	conflict, err := infrastructure.ClaimLease(ctx, "branch", time.Hour)
	if err != nil || conflict != nil {
		t.Fatalf("expected claim conflict, got lease=%+v err=%v", conflict, err)
	}

	stale := *lease
	stale.Generation++
	renewed, err := infrastructure.RenewLease(ctx, stale, time.Hour)
	if err != nil || renewed != nil {
		t.Fatalf("expected renew conflict, got lease=%+v err=%v", renewed, err)
	}

	renewed, err = infrastructure.RenewLease(ctx, *lease, 2*time.Hour)
	if err != nil {
		t.Fatalf("renew lease failed: %v", err)
	}
	if renewed == nil ||
		renewed.Generation != lease.Generation ||
		!renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("unexpected renewed lease: %+v", renewed)
	}

	released, err := infrastructure.ReleaseLease(ctx, stale)
	if err != nil || released {
		t.Fatalf("expected release conflict, got released=%v err=%v", released, err)
	}
	released, err = infrastructure.ReleaseLease(ctx, *renewed)
	if err != nil || !released {
		t.Fatalf("release lease failed: released=%v err=%v", released, err)
	}

	reclaimed, err := infrastructure.ClaimLease(ctx, "branch", time.Hour)
	if err != nil {
		t.Fatalf("reclaim lease failed: %v", err)
	}
	if reclaimed == nil || reclaimed.Generation != 2 {
		t.Fatalf("expected generation 2, got %+v", reclaimed)
	}
}
