package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

func TestWaitAnySignals(t *testing.T) {
	ctx := context.Background()
	newTimedEngine := func(t *testing.T, now *time.Time) (*memory.Infrastructure, *Engine) {
		store := memory.New()
		store.SetClock(func() time.Time { return *now })
		return store, testEngine(t, store, Options{Now: func() time.Time { return *now }})
	}
	definitionWithTimeout := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, input int) (waitOutcome, error) {
		return waitAnyOutcome(f, "signals",
			OnSignal[string]("callback", input),
			OnSleep("timeout", time.Hour),
		)
	}))

	t.Run("external response wins and cancels timer", func(t *testing.T) {
		now := time.Now()
		store, engine := newTimedEngine(t, &now)
		waiting, err := engine.Drive(ctx, definitionWithTimeout, 1)
		if err != nil || waiting.Status != Suspended || len(waiting.Pending) != 2 {
			t.Fatalf("%+v %v", waiting, err)
		}
		if waiting.Pending[0].Kind != SignalKind || waiting.Pending[1].Kind != SleepKind ||
			!waiting.Pending[1].Until.Equal(now.Add(time.Hour)) {
			t.Fatal(waiting.Pending)
		}
		now = now.Add(time.Minute)
		if accepted, err := engine.ResolveSignal(ctx, "signals|callback", 1, "answer"); !accepted || err != nil {
			t.Fatalf("%t %v", accepted, err)
		}
		timer, _ := store.Get(ctx, "signals|timeout")
		if timer.Status != Cancelled {
			t.Fatal(timer)
		}
		now = now.Add(2 * time.Hour)
		result, err := engine.Drive(ctx, definitionWithTimeout, 1)
		if err != nil || result.Output.Kind != SignalKind || result.Output.Response != "answer" {
			t.Fatalf("%+v %v", result, err)
		}
		messages, _ := store.Pending(ctx, now)
		for _, message := range messages {
			if message.Key == timer.Key && message.Purpose == memory.WakeNotification {
				t.Fatal("cancelled timer still scheduled")
			}
		}
	})
	t.Run("timeout wins with no response payload and replays", func(t *testing.T) {
		now := time.Now()
		store, engine := newTimedEngine(t, &now)
		first, err := engine.Drive(ctx, definitionWithTimeout, 1)
		if err != nil || first.Status != Suspended {
			t.Fatalf("%+v %v", first, err)
		}
		deadline := first.Pending[1].Until
		now = now.Add(time.Minute)
		again, err := engine.Drive(ctx, definitionWithTimeout, 1)
		if err != nil || !again.Pending[1].Until.Equal(deadline) {
			t.Fatalf("%+v %v", again, err)
		}
		now = deadline
		for i := 0; i < 2; i++ {
			result, err := engine.Drive(ctx, definitionWithTimeout, 1)
			if err != nil || result.Status != Completed || result.Output.Key != "signals|timeout" ||
				result.Output.Kind != SleepKind || result.Output.Response != "" {
				t.Fatalf("%+v %v", result, err)
			}
		}
		callback, _ := store.Get(ctx, "signals|callback")
		if callback.Status != Cancelled {
			t.Fatal(callback)
		}
		if accepted, err := engine.ResolveSignal(ctx, callback.Key, 1, "late"); accepted || err != nil {
			t.Fatalf("%t %v", accepted, err)
		}
	})
	t.Run("callback after deadline cannot steal timer winner", func(t *testing.T) {
		now := time.Now()
		_, engine := newTimedEngine(t, &now)
		_, err := engine.Drive(ctx, definitionWithTimeout, 1)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Hour)
		accepted, err := engine.ResolveSignal(ctx, "signals|callback", 1, "late")
		if accepted || err != nil {
			t.Fatalf("%t %v", accepted, err)
		}
		result, err := engine.Drive(ctx, definitionWithTimeout, 1)
		if err != nil || result.Output.Kind != SleepKind {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("earliest due deadline wins regardless of declaration order", func(t *testing.T) {
		now := time.Now()
		_, engine := newTimedEngine(t, &now)
		deadline := now.Add(30 * time.Minute)
		flow := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, _ int) (waitOutcome, error) {
			return waitAnyOutcome(f, "timers", OnSleep("late", time.Hour), OnSleepUntil("early", deadline))
		}))
		_, err := engine.Drive(ctx, flow, 0)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Hour)
		result, err := engine.Drive(ctx, flow, 0)
		if err != nil || result.Output.Key != "timers|early" || result.Output.Kind != SleepKind {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("immediate sleep completes registration atomically", func(t *testing.T) {
		now := time.Now()
		store, engine := newTimedEngine(t, &now)
		flow := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, in int) (waitOutcome, error) {
			return waitAnyOutcome(f, "signals", OnSignal[string]("callback", in), OnSleep("now", 0))
		}))
		result, err := engine.Drive(ctx, flow, 1)
		if err != nil || result.Status != Completed || result.Output.Kind != SleepKind {
			t.Fatalf("%+v %v", result, err)
		}
		callback, _ := store.Get(ctx, "signals|callback")
		if callback.Status != Cancelled {
			t.Fatal(callback)
		}
	})
	t.Run("failed external signal does not fail healthy sleep", func(t *testing.T) {
		now := time.Now()
		_, engine := newTimedEngine(t, &now)
		_, err := engine.Drive(ctx, definitionWithTimeout, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.FailWait(ctx, "signals|callback", 1, errors.New("unavailable")); err != nil {
			t.Fatal(err)
		}
		pending, err := engine.Drive(ctx, definitionWithTimeout, 1)
		if err != nil || len(pending.Pending) != 1 || pending.Pending[0].Kind != SleepKind {
			t.Fatalf("%+v %v", pending, err)
		}
		now = now.Add(time.Hour)
		result, err := engine.Drive(ctx, definitionWithTimeout, 1)
		if err != nil || result.Output.Kind != SleepKind || result.Output.Failures != 1 {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("custom kind rejected explicitly by reference store", func(t *testing.T) {
		store := memory.New()
		engine := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, in int) (waitOutcome, error) {
			return waitAnyOutcome(f, "signals", OnKind[string](Kind("CUSTOM"), "custom", in))
		}))
		if _, err := engine.Drive(ctx, flow, 1); !errors.Is(err, ErrSignalUnsupported) {
			t.Fatal(err)
		}
		messages, _ := store.Pending(ctx, time.Now())
		if len(messages) != 0 {
			t.Fatal("unsupported signal was partially registered")
		}
	})
}
