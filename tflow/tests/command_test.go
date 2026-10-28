package tflow_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/k0923/go/tflow"
)

func commandsOf(t *testing.T, s tflow.Store, kind tflow.CommandKind) []tflow.Command {
	t.Helper()
	pending, err := s.PendingCommands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result []tflow.Command
	for _, item := range pending {
		if item.Command.Kind == kind {
			result = append(result, item.Command)
		}
	}
	return result
}

func TestHandleChecksQueuedIdentityAtConsumption(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(1), tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	sub := list(t, s, tflow.SubRunning)[0]
	if err := e.Resume(ctx, sub.Execution("job"), event{id: "response"}); err != nil {
		t.Fatal(err)
	}
	queued := append(commandsOf(t, s, tflow.StartSub), commandsOf(t, s, tflow.ResumeSub)...)
	if _, err := e.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	oldCancels := commandsOf(t, s, tflow.CancelSub)
	if _, err := e.Rerun(ctx, tflow.RerunCanceled); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	x := executor{
		start: func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error) {
			calls.Add(1)
			return tflow.DispatchResult{Status: tflow.Accepted}, nil
		},
		resume: func(context.Context, tflow.SubTask, tflow.Execution, tflow.Event) (tflow.DispatchResult, error) {
			calls.Add(1)
			return tflow.DispatchResult{Status: tflow.Waiting}, nil
		},
		cancel: func(_ context.Context, task tflow.SubTask, ref tflow.Execution) error {
			if ref.Attempt != 1 {
				t.Fatalf("cancel targeted rerun: %+v", ref)
			}
			calls.Add(1)
			return nil
		},
	}
	for _, command := range queued {
		if err := e.Handle(ctx, command, x); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("old queued execution called executor")
	}
	// The adapter helper also uses the same receiving boundary.
	adapter, _ := tflow.NewExecutorDispatcher(e.Engine, x)
	if err := adapter.Dispatch(ctx, queued[0]); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("ExecutorDispatcher bypassed identity checks")
	}
	for _, command := range oldCancels {
		if err := e.Handle(ctx, command, x); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("historical cancellation calls=%d", calls.Load())
	}
}

func TestHandleValidatesCommandAndContext(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(1), tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	command := commandsOf(t, s, tflow.StartSub)[0]
	if err := e.Handle(ctx, command, nil); !errors.Is(err, tflow.ErrExecutorUnavailable) {
		t.Fatalf("missing executor: %v", err)
	}
	bad := command
	bad.MainID = "another-main"
	if err := e.Handle(ctx, bad, nil); err == nil {
		t.Fatal("invalid command scope accepted")
	}
	bad = command
	bad.ID = "different-command"
	if err := e.Handle(ctx, bad, nil); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatalf("invalid identity: %v", err)
	}
	sub := list(t, s, tflow.SubRunning)[0]
	if err := e.Resume(ctx, sub.Execution("job"), event{id: "a"}); err != nil {
		t.Fatal(err)
	}
	bad = commandsOf(t, s, tflow.ResumeSub)[0]
	bad.Event = event{id: "changed"}
	if err := e.Handle(ctx, bad, nil); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatalf("changed event: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.Handle(canceled, command, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("context: %v", err)
	}
}

func TestTerminationNotificationsAreDurableAndHistorical(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(1), tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	first := list(t, s, tflow.SubRunning)[0]
	r := report(t, e, first, tflow.SubFailed)
	old := commandsOf(t, s, tflow.MainTerminated)
	if len(old) != 1 || old[0].Main.Version != r.Main.Version {
		t.Fatalf("terminal event: %+v", old)
	}
	if _, err := e.Rerun(ctx, tflow.RerunFailed); err != nil {
		t.Fatal(err)
	}
	current := list(t, s, tflow.SubRunning)[0]
	report(t, e, current, tflow.SubSucceeded)
	events := commandsOf(t, s, tflow.MainTerminated)
	if len(events) != 2 || events[0].ID == events[1].ID {
		t.Fatalf("rerun reused terminal identity: %+v", events)
	}
	var seen []tflow.MainTermination
	consumer := newEngine(t, s, nil, tflow.Options{
		TerminationHandler: tflow.TerminationHandlerFunc(func(_ context.Context, event tflow.MainTermination) error {
			seen = append(seen, event)
			return nil
		}),
	})
	if err := consumer.Handle(ctx, old[0], nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Main.Status != tflow.MainFailed || seen[0].Main.Subs.Failed != 1 ||
		seen[0].Main.Version != old[0].Main.Version || seen[0].ID != old[0].ID {
		t.Fatalf("historical event replaced with current state: %+v", seen)
	}
}

func TestObserverFailureRetriesWithoutChangingTerminalState(t *testing.T) {
	s := newStore()
	failure := errors.New("observer unavailable")
	var fail atomic.Bool
	fail.Store(true)
	var ids []string
	e := newEngine(t, s, newSource(1), tflow.Options{
		TerminationHandler: tflow.TerminationHandlerFunc(func(_ context.Context, event tflow.MainTermination) error {
			ids = append(ids, event.ID)
			if fail.Load() {
				return failure
			}
			return nil
		}),
	})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	report(t, e, list(t, s, tflow.SubRunning)[0], tflow.SubSucceeded)
	d, _ := tflow.NewExecutorDispatcher(e.Engine, execution{})
	if _, err := e.DispatchPending(ctx, 100, d); !errors.Is(err, failure) {
		t.Fatalf("observer failure: %v", err)
	}
	m, _ := s.GetMain(ctx)
	if m.Status != tflow.MainSucceeded || len(commandsOf(t, s, tflow.MainTerminated)) != 1 {
		t.Fatal("observer failure changed terminal state or lost event")
	}
	fail.Store(false)
	deliveryClock.advance()
	if _, err := e.DispatchPending(ctx, 100, d); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != ids[1] || len(commandsOf(t, s, tflow.MainTerminated)) != 0 {
		t.Fatalf("observer retry: %v", ids)
	}
}

type rejectingTerminalStore struct {
	tflow.Store
	reject atomic.Bool
}
type rejectingTerminalTx struct {
	tflow.Tx
	reject *atomic.Bool
}

func (tx rejectingTerminalTx) PutCommand(c tflow.Command) error {
	if c.Kind == tflow.MainTerminated && tx.reject.Load() {
		return errors.New("terminal event write failed")
	}
	return tx.Tx.PutCommand(c)
}
func (s *rejectingTerminalStore) WithTx(ctx context.Context, fn func(tflow.Tx) error) error {
	return s.Store.WithTx(ctx, func(tx tflow.Tx) error { return fn(rejectingTerminalTx{Tx: tx, reject: &s.reject}) })
}

func TestTerminalStateAndNotificationCommitTogether(t *testing.T) {
	for _, operation := range []string{"complete", "cancel", "fail"} {
		t.Run(operation, func(t *testing.T) {
			s := &rejectingTerminalStore{Store: newStore()}
			e := newEngine(t, s, newSource(1), tflow.Options{})
			if _, err := e.submit(ctx, spec(1)); err != nil {
				t.Fatal(err)
			}
			s.reject.Store(true)
			var err error
			switch operation {
			case "complete":
				sub := list(t, s, tflow.SubRunning)[0]
				_, err = e.Report(ctx, tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded})
			case "cancel":
				_, err = e.Cancel(ctx)
			case "fail":
				_, err = e.Fail(ctx, "failed")
			}
			if err == nil {
				t.Fatal("event write failure was ignored")
			}
			m, _ := s.GetMain(ctx)
			if m.Status != tflow.MainRunning || len(commandsOf(t, s, tflow.MainTerminated)) != 0 {
				t.Fatal("partial terminal transition committed")
			}
			if operation != "complete" && m.Subs.Running != 1 {
				t.Fatal("cancellation subs committed without terminal event")
			}
			s.reject.Store(false)
			switch operation {
			case "complete":
				err = e.Reconcile(ctx)
			case "cancel":
				_, err = e.Cancel(ctx)
			case "fail":
				_, err = e.Fail(ctx, "failed")
			}
			if err != nil || len(commandsOf(t, s, tflow.MainTerminated)) != 1 {
				t.Fatalf("terminal transition retry: %v", err)
			}
		})
	}
}

func TestConcurrentFinalReportsProduceOneTermination(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(1), tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	sub := list(t, s, tflow.SubRunning)[0]
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Report(ctx, tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(commandsOf(t, s, tflow.MainTerminated)) != 1 {
		t.Fatal("duplicate terminal events")
	}
}

func TestDeciderDistinguishesDrainedCompletionFromEarlyFailure(t *testing.T) {
	s := newStore()
	var policyCalls atomic.Int64
	e := newEngine(t, s, newSource(1), tflow.Options{
		MainDecider: tflow.MainDeciderFunc(func(ctx context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
			if view.ReadyToComplete() {
				return tflow.DefaultMainDecider{}.DecideMain(ctx, view)
			}
			policyCalls.Add(1)
			if view.Summary().Failed > 0 {
				return tflow.MainDecision{Action: tflow.Fail, Reason: "circuit-breaker"}, nil
			}
			return tflow.MainDecision{Action: tflow.Continue}, nil
		}),
	})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	policyCalls.Store(0)
	r := report(t, e, list(t, s, tflow.SubRunning)[0], tflow.SubFailed)
	if r.Main.Status != tflow.MainFailed || r.Main.Error != "" || policyCalls.Load() != 0 {
		t.Fatalf("final result took circuit-breaker path: %+v calls=%d", r.Main, policyCalls.Load())
	}
}

func TestObserverPanicRetainsNotification(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(1), tflow.Options{
		TerminationHandler: tflow.TerminationHandlerFunc(func(context.Context, tflow.MainTermination) error {
			panic("observer failure")
		}),
	})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	report(t, e, list(t, s, tflow.SubRunning)[0], tflow.SubSucceeded)
	d, _ := tflow.NewExecutorDispatcher(e.Engine, execution{})
	if _, err := e.DispatchPending(ctx, 100, d); err == nil {
		t.Fatal("observer panic was swallowed")
	}
	if len(commandsOf(t, s, tflow.MainTerminated)) != 1 {
		t.Fatal("observer panic discarded notification")
	}
}
