package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type eventCollector struct {
	mu     sync.Mutex
	events []Event
}

type unformattableEventError struct{}

func (*unformattableEventError) Error() string { panic("broken Error method") }

func (c *eventCollector) listen(_ context.Context, event Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *eventCollector) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

func eventsOfType(events []Event, eventType EventType) []Event {
	var result []Event
	for _, event := range events {
		if event.Type == eventType {
			result = append(result, event)
		}
	}
	return result
}

func TestEngineEvents(t *testing.T) {
	t.Run("normal execution and replay have distinct exactly counted events", func(t *testing.T) {
		engine := testEngine(t, NewMemoryInfrastructure(), Options{})
		collector := &eventCollector{}
		unsubscribe := engine.Subscribe(collector.listen)
		defer unsubscribe()
		flow := definition(FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
			return f.Run("action", Pure(func(n int) int { return n + 1 }), input)
		}))
		for i := 0; i < 2; i++ {
			result, err := engine.Drive(context.Background(), flow, 1)
			if err != nil || result.Output != 2 {
				t.Fatalf("%+v %v", result, err)
			}
		}
		events := collector.snapshot()
		var types []EventType
		seen := make(map[string]bool)
		for i, event := range events {
			types = append(types, event.Type)
			if seen[event.ID] || event.ID == "" {
				t.Fatalf("duplicate ID: %+v", event)
			}
			seen[event.ID] = true
			if event.SchemaVersion != 1 || event.EngineID == "" || event.ExecutionID == "" ||
				event.WorkflowName != "test" || event.WorkflowVersion != "1" || event.Sequence != uint64(i+1) {
				t.Fatalf("incomplete event: %+v", event)
			}
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip Event
			if err = json.Unmarshal(raw, &roundTrip); err != nil {
				t.Fatal(err)
			}
			reencoded, err := json.Marshal(roundTrip)
			if err != nil || !bytes.Equal(raw, reencoded) {
				t.Fatalf("JSON changed: %s %s %v", raw, reencoded, err)
			}
		}
		want := []EventType{DriveStarted, ActionStarted, ActionFinished, DriveFinished, DriveStarted, ActionReplayed, ActionFinished, DriveFinished}
		if !reflect.DeepEqual(types, want) {
			t.Fatalf("types=%v", types)
		}
		starts := eventsOfType(events, DriveStarted)
		if starts[0].ExecutionID == starts[1].ExecutionID {
			t.Fatal("drive identities reused")
		}
		finished := eventsOfType(events, ActionFinished)
		if finished[0].Status != Completed || finished[0].RecordVersion != 2 || finished[0].Replayed ||
			!finished[1].Replayed || finished[1].Error != nil {
			t.Fatalf("%+v", finished)
		}
	})
	t.Run("rejected and cancelled drives still finish", func(t *testing.T) {
		engine := testEngine(t, NewMemoryInfrastructure(), Options{})
		collector := &eventCollector{}
		engine.Subscribe(collector.listen)
		_, err := engine.Drive(context.Background(), Workflow[int, int]{}, 0)
		if !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = engine.Drive(ctx, definition(FlowFunc[int, int](func(*FlowContext, int) (int, error) { t.Fatal("executed"); return 0, nil })), 0)
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		events := collector.snapshot()
		if len(events) != 4 || len(eventsOfType(events, DriveStarted)) != 2 || len(eventsOfType(events, DriveFinished)) != 2 {
			t.Fatal(events)
		}
		finished := eventsOfType(events, DriveFinished)
		if finished[0].Error.Category != "invalid" || finished[1].Error.Category != "cancelled" {
			t.Fatal(finished)
		}
	})
	t.Run("transient permanent panic and timeout outcomes", func(t *testing.T) {
		for _, scenario := range []string{"transient", "permanent", "panic", "timeout"} {
			t.Run(scenario, func(t *testing.T) {
				engine := testEngine(t, NewMemoryInfrastructure(), Options{})
				collector := &eventCollector{}
				engine.Subscribe(collector.listen)
				cause := errors.New("action failed")
				flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
					return f.Run("failure", AsAction(func(ctx context.Context, _ int) (int, error) {
						switch scenario {
						case "permanent":
							return 0, Permanent(cause)
						case "panic":
							panic(cause)
						case "timeout":
							<-ctx.Done()
							return 0, ctx.Err()
						default:
							return 0, cause
						}
					}), in, ActionOptions{Timeout: 10 * time.Millisecond})
				}))
				_, err := engine.Drive(context.Background(), flow, 0)
				if err == nil {
					t.Fatal("error lost")
				}
				events := collector.snapshot()
				if len(events) != 4 || len(eventsOfType(events, ActionStarted)) != 1 || len(eventsOfType(events, ActionFinished)) != 1 {
					t.Fatal(events)
				}
				finished := eventsOfType(events, ActionFinished)[0]
				wantCategory := map[string]string{"transient": "execution", "permanent": "permanent", "panic": "panic", "timeout": "timeout"}[scenario]
				if finished.Error == nil || finished.Error.Category != wantCategory {
					t.Fatal(finished)
				}
				if scenario == "permanent" {
					if finished.Status != Failed {
						t.Fatal(finished)
					}
					_, _ = engine.Drive(context.Background(), flow, 0)
					replay := eventsOfType(collector.snapshot(), ActionReplayed)
					if len(replay) != 1 || replay[0].Status != Failed || replay[0].Error.Category != "replay" {
						t.Fatal(replay)
					}
				} else if finished.Status != Running {
					t.Fatal(finished)
				}
			})
		}
	})
	t.Run("subscription panic and payload mutation do not affect other listeners", func(t *testing.T) {
		engine := testEngine(t, NewMemoryInfrastructure(), Options{Observer: func(context.Context, Observation) { panic("observer") }})
		engine.Subscribe(func(_ context.Context, event Event) {
			if len(event.Pending) > 0 {
				event.Pending[0].Key = "mutated"
			}
			if event.Error != nil {
				event.Error.Message = "mutated"
			}
			panic("listener")
		})
		collector := &eventCollector{}
		engine.Subscribe(collector.listen)
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			_, err := f.WaitFor[string]("wait", in)
			return 0, err
		}))
		result, err := engine.Drive(context.Background(), flow, 0)
		if err != nil || result.Status != Suspended || result.Pending[0].Key != "wait" {
			t.Fatalf("%+v %v", result, err)
		}
		finished := eventsOfType(collector.snapshot(), DriveFinished)
		if len(finished) != 1 || finished[0].Pending[0].Key != "wait" {
			t.Fatal(finished)
		}
		_, err = engine.Drive(context.Background(), Workflow[int, int]{}, 0)
		if err == nil {
			t.Fatal("error lost")
		}
		finished = eventsOfType(collector.snapshot(), DriveFinished)
		if finished[1].Error.Message == "mutated" {
			t.Fatal("mutable error shared")
		}
	})
	t.Run("claim conflicts and read failures finish without action start", func(t *testing.T) {
		for _, scenario := range []string{"conflict", "read"} {
			store := NewMemoryInfrastructure()
			var infrastructure Infrastructure = store
			if scenario == "conflict" {
				if _, err := store.Claim(context.Background(), "action", time.Hour); err != nil {
					t.Fatal(err)
				}
			} else {
				infrastructure = &failingInfrastructure{Infrastructure: store, getError: errors.New("read failed")}
			}
			engine := testEngine(t, infrastructure, Options{})
			collector := &eventCollector{}
			engine.Subscribe(collector.listen)
			flow := definition(FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
				return f.Run("action", Pure(func(n int) int { return n }), input)
			}))
			if _, err := engine.Drive(context.Background(), flow, 0); err == nil {
				t.Fatal("failure lost")
			}
			events := collector.snapshot()
			if len(eventsOfType(events, ActionStarted)) != 0 || len(eventsOfType(events, ActionFinished)) != 1 {
				t.Fatal(events)
			}
			finished := eventsOfType(events, ActionFinished)[0]
			if finished.Status != Running || finished.Error == nil {
				t.Fatal(finished)
			}
			wantCategory, wantOperation := "conflict", "Claim"
			if scenario == "read" {
				wantCategory, wantOperation = "infrastructure", "Get"
			}
			if finished.Error.Category != wantCategory || finished.Error.Operation != wantOperation {
				t.Fatal(finished)
			}
		}
	})
	t.Run("broken error formatter does not drop events", func(t *testing.T) {
		engine := testEngine(t, NewMemoryInfrastructure(), Options{})
		collector := &eventCollector{}
		engine.Subscribe(collector.listen)
		cause := &unformattableEventError{}
		flow := definition(FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
			return f.Run("action", AsAction(func(context.Context, int) (int, error) { return 0, cause }), input)
		}))
		_, err := engine.Drive(context.Background(), flow, 0)
		if !errors.Is(err, cause) {
			t.Fatal("original error lost")
		}
		events := collector.snapshot()
		if len(events) != 4 {
			t.Fatalf("events dropped: %d", len(events))
		}
		for _, eventType := range []EventType{ActionFinished, DriveFinished} {
			event := eventsOfType(events, eventType)[0]
			if event.Error == nil || event.Error.Message != "error description unavailable" {
				t.Fatal("fallback diagnostic missing")
			}
		}
	})
	t.Run("old observer phases unchanged", func(t *testing.T) {
		var phases []string
		engine := testEngine(t, NewMemoryInfrastructure(), Options{Observer: func(_ context.Context, o Observation) { phases = append(phases, o.Phase) }})
		collector := &eventCollector{}
		engine.Subscribe(collector.listen)
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("action", Pure(func(n int) int { return n }), in)
		}))
		for i := 0; i < 2; i++ {
			if _, err := engine.Drive(context.Background(), flow, 0); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{"drive_started", "action_started", "action_finished", "drive_finished", "drive_started", "action_replayed", "action_finished", "drive_finished"}
		if !reflect.DeepEqual(phases, want) {
			t.Fatal(phases)
		}
		if len(collector.snapshot()) != 8 {
			t.Fatal("new subscriptions not delivered")
		}
	})
}

func TestEngineSubscribe(t *testing.T) {
	t.Run("filter and reentrant idempotent unsubscribe", func(t *testing.T) {
		engine := testEngine(t, NewMemoryInfrastructure(), Options{})
		engine.Subscribe(nil)()
		count := 0
		var unsubscribe func()
		unsubscribe = engine.Subscribe(func(context.Context, Event) { count++; unsubscribe(); unsubscribe() }, DriveStarted)
		flow := definition(FlowFunc[int, int](func(_ *FlowContext, in int) (int, error) { return in, nil }))
		for i := 0; i < 2; i++ {
			if _, err := engine.Drive(context.Background(), flow, 0); err != nil {
				t.Fatal(err)
			}
		}
		if count != 1 {
			t.Fatal(count)
		}
	})
	t.Run("parallel branches and subscription changes", func(t *testing.T) {
		engine := testEngine(t, NewMemoryInfrastructure(), Options{})
		collector := &eventCollector{}
		engine.Subscribe(collector.listen)
		var ephemeral atomic.Int32
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				unsubscribe := engine.Subscribe(func(context.Context, Event) { ephemeral.Add(1) })
				unsubscribe()
			}
		}()
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			f.Go("a", Pure(func(n int) int { return n + 1 }), in)
			f.Go("b", Pure(func(n int) int { return n + 2 }), in)
			return in, nil
		}))
		if _, err := engine.Drive(context.Background(), flow, 0); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		events := collector.snapshot()
		if len(events) != 6 || len(eventsOfType(events, ActionStarted)) != 2 || len(eventsOfType(events, ActionFinished)) != 2 {
			t.Fatal(events)
		}
		ids := make(map[string]bool)
		for _, event := range events {
			if ids[event.ID] {
				t.Fatal("duplicate delivery")
			}
			ids[event.ID] = true
		}
	})
}

func TestEngineWaitEvents(t *testing.T) {
	t.Run("signal resolve emits once and repeated drives are distinct", func(t *testing.T) {
		store := NewMemoryInfrastructure()
		engine := testEngine(t, store, Options{})
		collector := &eventCollector{}
		engine.Subscribe(collector.listen)
		flow := definition(FlowFunc[int, string](func(f *FlowContext, in int) (string, error) { return f.WaitFor[string]("wait", in) }))
		for i := 0; i < 2; i++ {
			result, err := engine.Drive(context.Background(), flow, 0)
			if err != nil || result.Status != Suspended {
				t.Fatalf("%+v %v", result, err)
			}
		}
		record, _ := store.Get(context.Background(), "wait")
		for i := 0; i < 2; i++ {
			if _, err := engine.ResolveSignal(context.Background(), "wait", record.Version, "done"); err != nil {
				t.Fatal(err)
			}
		}
		result, err := engine.Drive(context.Background(), flow, 0)
		if err != nil || result.Output != "done" {
			t.Fatalf("%+v %v", result, err)
		}
		events := collector.snapshot()
		resolved := eventsOfType(events, WaitResolved)
		if len(resolved) != 1 || resolved[0].ExecutionID != "" || resolved[0].RecordVersion != 2 {
			t.Fatal(resolved)
		}
		if len(eventsOfType(events, WaitSuspended)) != 2 || len(eventsOfType(events, DriveFinished)) != 3 {
			t.Fatal(events)
		}
		if _, err := engine.ResolveSignal(context.Background(), "missing", 1, "done"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if len(eventsOfType(collector.snapshot(), WaitResolved)) != 1 {
			t.Fatal("rejected resolve emitted completion")
		}
	})
	t.Run("sleep and immediate sleep resolve exactly once", func(t *testing.T) {
		for _, interval := range []time.Duration{0, time.Hour} {
			now := time.Now()
			store := NewMemoryInfrastructure()
			store.now = func() time.Time { return now }
			engine := testEngine(t, store, Options{Now: func() time.Time { return now }})
			collector := &eventCollector{}
			engine.Subscribe(collector.listen)
			flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) { return in, f.Sleep("sleep", interval) }))
			for i := 0; i < 3; i++ {
				result, err := engine.Drive(context.Background(), flow, 0)
				if err != nil {
					t.Fatal(err)
				}
				if interval > 0 && i < 2 && result.Status != Suspended {
					t.Fatal(result)
				}
				if i == 1 {
					now = now.Add(time.Hour)
				}
			}
			events := collector.snapshot()
			wantSuspended := 0
			if interval > 0 {
				wantSuspended = 2
			}
			if len(eventsOfType(events, WaitSuspended)) != wantSuspended || len(eventsOfType(events, WaitResolved)) != 1 {
				t.Fatal(events)
			}
		}
	})
}
