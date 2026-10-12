package flow

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInMemoryInfrastructureEventCAS_BitsUT(t *testing.T) {
	var infrastructure InMemoryInfrastructure
	ctx := context.Background()
	event := &StepResultEvent{
		BaseEvent: BaseEvent{
			Key:      "step",
			Status:   FlowStatusRunning,
			Branches: []string{"parallel", "branch"},
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
	current.Status = FlowStatusFinished
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
	stored, err := infrastructure.GetEvent(ctx, []string{"parallel", "branch"}, "step")
	if err != nil {
		t.Fatalf("get event failed: %v", err)
	}
	if string(stored.(*StepResultEvent).Result) != `"done"` {
		t.Fatalf("stored event was mutated through returned value: %s", stored.(*StepResultEvent).Result)
	}
}

func TestInMemoryInfrastructureResolvesSignals_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure()
	ctx := context.Background()
	added, err := infrastructure.AddEvent(ctx, &WaitForSignalEvent{
		BaseEvent: BaseEvent{
			Key:      "approval",
			Status:   FlowStatusRunning,
			Branches: []string{"group", "branch"},
		},
		Request: json.RawMessage(`"request"`),
	})
	if err != nil {
		t.Fatalf("add wait event failed: %v", err)
	}
	wait := added.(*WaitForSignalEvent)
	signal := NewWaitForSignal(
		wait.Key,
		wait.Branches,
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

	stored, err := infrastructure.GetEvent(ctx, wait.Branches, wait.Key)
	if err != nil {
		t.Fatalf("get resolved event failed: %v", err)
	}
	result := stored.(*WaitForSignalEvent)
	if result.Status != FlowStatusFinished ||
		result.Version != wait.Version+1 ||
		string(result.Response) != `"approved"` {
		t.Fatalf("unexpected resolved event: %+v", result)
	}
}

func TestInMemoryInfrastructureResolvesDueSleep_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure()
	ctx := context.Background()
	added, err := infrastructure.AddEvent(ctx, &SleepEvent{
		BaseEvent: BaseEvent{
			Key:    "sleep",
			Status: FlowStatusRunning,
		},
		Until: time.Now().Add(-time.Second),
	})
	if err != nil {
		t.Fatalf("add sleep event failed: %v", err)
	}
	sleep := added.(*SleepEvent)

	resolved, err := infrastructure.ResolveSignal(ctx, sleep.BuildSignal())
	if err != nil {
		t.Fatalf("resolve sleep failed: %v", err)
	}
	if !resolved {
		t.Fatal("expected due sleep to resolve")
	}
	stored, err := infrastructure.GetEvent(ctx, nil, sleep.Key)
	if err != nil {
		t.Fatalf("get sleep event failed: %v", err)
	}
	if stored.GetStatus() != FlowStatusFinished ||
		stored.GetVersion() != sleep.Version+1 {
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
					Status: FlowStatusFinished,
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

func TestInMemoryInfrastructureSnapshotsStateSignalsAndErrors_BitsUT(t *testing.T) {
	var infrastructure InMemoryInfrastructure
	ctx := context.Background()
	state := State{
		Input:  json.RawMessage(`{"value":1}`),
		Output: json.RawMessage(`"done"`),
		Status: FlowStatusFinished,
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

	signal := NewWaitForSignal("approval", nil, nil, nil, 1)
	if err := infrastructure.RaiseSignal(ctx, signal); err != nil {
		t.Fatalf("raise signal failed: %v", err)
	}
	infrastructure.OnError(context.Canceled)
	if len(infrastructure.Signals()) != 1 || len(infrastructure.Errors()) != 1 {
		t.Fatalf(
			"unexpected snapshots: signals=%d errors=%d",
			len(infrastructure.Signals()),
			len(infrastructure.Errors()),
		)
	}
}
