package tflow_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type tolerantMain struct{ *source }

func (*tolerantMain) DecideMain(_ context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
	if view.ReadyToComplete() {
		return tflow.MainDecision{Action: tflow.Succeed, Reason: "all results reviewed"}, nil
	}
	return tflow.MainDecision{Action: tflow.Continue}, nil
}

func TestTaskDeciderCanSucceedWithFailedUnitsAfterRecovery(t *testing.T) {
	repository := memory.New()
	store := repository.ForMain("job")
	task := &tolerantMain{source: newSource(2)}
	first := newEngine(t, store, task, tflow.Options{
		MainDecider: tflow.MainDeciderFunc(func(context.Context, tflow.MainDecisionView) (tflow.MainDecision, error) {
			t.Fatal("task decider should take precedence")
			return tflow.MainDecision{}, nil
		}),
	})
	if _, err := first.submit(ctx, spec(2)); err != nil {
		t.Fatal(err)
	}
	subs := list(t, store)
	report(t, first, subs[0], tflow.SubFailed)
	recovered := newEngine(t, repository.ForMain("job"), nil, tflow.Options{})
	final := report(t, recovered, subs[1], tflow.SubSucceeded)
	if final.Main.Status != tflow.MainSucceeded || final.Main.Subs.Failed != 1 || final.Main.Error != "all results reviewed" {
		t.Fatalf("business completion rule lost: %+v", final.Main)
	}
	notices := commandsOf(t, store, tflow.MainTerminated)
	if len(notices) != 1 || notices[0].Main.Subs != final.Main.Subs {
		t.Fatal("historical notification did not capture the consistent summary")
	}
}

func TestConfiguredDeciderIsPersistedAcrossEngines(t *testing.T) {
	repository := newRepository()
	s := repository.ForMain("job")
	decider := tflow.MainDeciderFunc(func(_ context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
		if view.ReadyToComplete() {
			return tflow.MainDecision{Action: tflow.Succeed}, nil
		}
		return tflow.MainDecision{Action: tflow.Continue}, nil
	})
	first := newEngine(t, s, newSource(1), tflow.Options{MainDecider: decider})
	if _, err := first.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	recovered := newEngine(t, repository.ForMain("job"), nil, tflow.Options{})
	final := report(t, recovered, list(t, s)[0], tflow.SubFailed)
	if final.Main.Status != tflow.MainSucceeded {
		t.Fatalf("recovery replaced persisted business decision: %+v", final.Main)
	}
	var nilDecider tflow.MainDeciderFunc
	if _, err := tflow.New(s, tflow.Options{MainDecider: nilDecider}); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal("typed nil decider accepted")
	}
}

type validationMain struct{ validated bool }

func (*validationMain) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return tflow.Page{Items: []tflow.SubTask{unit("row")}}, nil
}

func (m *validationMain) DecideMain(_ context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
	if m.validated && view.ReadyToComplete() {
		return tflow.MainDecision{Action: tflow.Succeed, Reason: "validated"}, nil
	}
	return tflow.MainDecision{Action: tflow.Continue}, nil
}

func TestBusinessCanDelayCompletionUntilVersionedValidation(t *testing.T) {
	repository := newRepository()
	s := repository.ForMain("job")
	e := newEngine(t, s, &validationMain{}, tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	r := report(t, e, list(t, s)[0], tflow.SubSucceeded)
	if r.Main.Status != tflow.MainRunning || !r.Main.ReadyToComplete() || len(commandsOf(t, s, tflow.MainTerminated)) != 0 {
		t.Fatal("Continue was overridden after all units finished")
	}
	if err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := e.GetMain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.UpdateTask(ctx, m.Version, &validationMain{validated: true}); err != nil {
		t.Fatal(err)
	}
	recovered := newEngine(t, repository.ForMain("job"), nil, tflow.Options{})
	if _, err := recovered.Drain(ctx, 100); err != nil {
		t.Fatal(err)
	}
	m, _ = e.GetMain(ctx)
	if m.Status != tflow.MainSucceeded {
		t.Fatalf("versioned validation did not converge: %+v", m)
	}
}

func TestDeciderCannotMutatePersistedSchedulingFacts(t *testing.T) {
	s := newStore()
	decider := tflow.MainDeciderFunc(func(ctx context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
		state, summary := view.State(), view.Summary()
		state.Status, state.DispatchExhausted, state.Version = tflow.MainFailed, true, 1000
		summary.Running, summary.Failed = 0, 1000
		if view.State() == state || view.Summary() == summary {
			t.Fatal("decision accessors exposed mutable state")
		}
		return tflow.DefaultMainDecider{}.DecideMain(ctx, view)
	})
	e := newEngine(t, s, newSource(1), tflow.Options{MainDecider: decider})
	m, err := e.submit(ctx, spec(1))
	if err != nil || m.Status != tflow.MainRunning || m.Subs.Running != 1 || m.Subs.Failed != 0 {
		t.Fatalf("decision changed persisted facts: %+v %v", m, err)
	}
	final := report(t, e, list(t, s)[0], tflow.SubSucceeded)
	if final.Main.Status != tflow.MainSucceeded || final.Main.Subs.Failed != 0 {
		t.Fatalf("decision mutation leaked into completion: %+v", final.Main)
	}
}

func TestInvalidOrFailedDecisionDoesNotCommitATerminalState(t *testing.T) {
	failure := errors.New("business decision unavailable")
	for _, scenario := range []string{"early_success", "unknown", "error", "panic"} {
		t.Run(scenario, func(t *testing.T) {
			s := newStore()
			decider := tflow.MainDeciderFunc(func(context.Context, tflow.MainDecisionView) (tflow.MainDecision, error) {
				switch scenario {
				case "early_success":
					return tflow.MainDecision{Action: tflow.Succeed}, nil
				case "unknown":
					return tflow.MainDecision{Action: "UNKNOWN"}, nil
				case "error":
					return tflow.MainDecision{}, failure
				default:
					panic("business decision panic")
				}
			})
			e := newEngine(t, s, newSource(2), tflow.Options{MainDecider: decider})
			m, err := e.submit(ctx, spec(1))
			var committed *tflow.CommittedError
			if err == nil || !errors.As(err, &committed) || m.Status != tflow.MainRunning || m.Subs.Running != 1 {
				t.Fatalf("invalid decision changed state: %+v %v", m, err)
			}
			if scenario == "error" && !errors.Is(err, failure) {
				t.Fatal("decision error identity lost")
			}
			if len(commandsOf(t, s, tflow.MainTerminated)) != 0 || len(commandsOf(t, s, tflow.ReconcileMain)) == 0 {
				t.Fatal("invalid decision lost recovery or emitted terminal notice")
			}
		})
	}
}

func TestEarlyCancelDecisionCancelsPendingAndRunningTogether(t *testing.T) {
	s := newStore()
	decider := tflow.MainDeciderFunc(func(_ context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
		if view.Summary().Failed > 0 {
			return tflow.MainDecision{Action: tflow.Cancel, Reason: "business canceled batch"}, nil
		}
		return tflow.MainDecision{Action: tflow.Continue}, nil
	})
	e := newEngine(t, s, newSource(10), tflow.Options{MainDecider: decider})
	input := spec(2)
	input.PlanningPolicy = tflow.BufferPolicy{TargetPending: 3}
	if _, err := e.submit(ctx, input); err != nil {
		t.Fatal(err)
	}
	r := report(t, e, list(t, s, tflow.SubRunning)[0], tflow.SubFailed)
	if r.Main.Status != tflow.MainCanceled || r.Main.Subs.Running != 0 || r.Main.Subs.Pending != 0 ||
		r.Main.Subs.Canceled != 4 || r.Main.Subs.Failed != 1 || r.Main.Error != "business canceled batch" {
		t.Fatalf("early cancel did not atomically stop work: %+v", r.Main)
	}
	if len(commandsOf(t, s, tflow.CancelSub)) != 1 || len(commandsOf(t, s, tflow.MainTerminated)) != 1 {
		t.Fatal("early cancel lost historical cancel or notice")
	}
}

func TestStaleDecisionCannotFailNewerVersion(t *testing.T) {
	s := newStore()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	decider := tflow.MainDeciderFunc(func(ctx context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
		if view.Summary().Failed > 0 && view.Summary().Running > 0 {
			once.Do(func() { close(entered); <-release })
			return tflow.MainDecision{Action: tflow.Fail, Reason: "old early failure"}, nil
		}
		if view.ReadyToComplete() {
			return tflow.MainDecision{Action: tflow.Succeed, Reason: "new completed state"}, nil
		}
		return tflow.DefaultMainDecider{}.DecideMain(ctx, view)
	})
	e := newEngine(t, s, newSource(2), tflow.Options{MainDecider: decider})
	if _, err := e.submit(ctx, spec(2)); err != nil {
		t.Fatal(err)
	}
	subs := list(t, s)
	done := make(chan error, 1)
	go func() {
		_, err := e.Report(ctx, tflow.Report{Execution: subs[0].Execution("job"), Status: tflow.SubFailed})
		done <- err
	}()
	<-entered
	_, err := e.Report(ctx, tflow.Report{Execution: subs[1].Execution("job"), Status: tflow.SubSucceeded})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m, _ := e.GetMain(ctx)
	if m.Status != tflow.MainSucceeded || m.Error != "new completed state" || len(commandsOf(t, s, tflow.MainTerminated)) != 1 {
		t.Fatalf("stale decision won: %+v", m)
	}
}

type expectedTotalMain struct {
	*source
	expected int64
}

func (m *expectedTotalMain) Plan(ctx context.Context, req tflow.PlanRequest) (tflow.Plan, error) {
	plan, err := m.source.Plan(ctx, req)
	if err != nil {
		return nil, err
	}
	page := plan.(*batch)
	if next, ok := page.updated.(*source); ok {
		page.updated = &expectedTotalMain{source: next, expected: m.expected}
	}
	return page, nil
}

func (m *expectedTotalMain) DecideMain(ctx context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
	if view.State().DispatchExhausted && view.State().DispatchCursor != m.expected {
		return tflow.MainDecision{Action: tflow.Fail, Reason: "unexpected discovered count"}, nil
	}
	return tflow.DefaultMainDecider{}.DecideMain(ctx, view)
}

func TestBusinessOwnsExpectedTotalValidation(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, &expectedTotalMain{source: newSource(2), expected: 5}, tflow.Options{})
	m, err := e.submit(ctx, spec(3))
	if err != nil || m.Status != tflow.MainFailed || m.DispatchCursor != 2 || m.Error != "unexpected discovered count" {
		t.Fatalf("expected count rule not applied: %+v %v", m, err)
	}
}
