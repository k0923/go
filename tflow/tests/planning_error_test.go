package tflow_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/k0923/go/tflow"
)

type failingPlanner struct {
	progressed    bool
	failure       error
	panicOnNext   bool
	invalidOnNext bool
}

func (p *failingPlanner) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	if !p.progressed {
		next := *p
		next.progressed = true
		return &batch{tasks: []tflow.SubTask{&rowTask{}}, updated: &next, more: true}, nil
	}
	if p.panicOnNext {
		panic("broken definition")
	}
	if p.invalidOnNext {
		return nil, nil
	}
	return nil, p.failure
}

func TestPlanningFailureClassification(t *testing.T) {
	base := errors.New("source problem")
	for _, test := range []struct {
		name     string
		task     *failingPlanner
		terminal bool
	}{
		{"temporary", &failingPlanner{failure: base}, false},
		{"permanent", &failingPlanner{failure: tflow.Permanent(base)}, true},
		{"panic", &failingPlanner{panicOnNext: true}, true},
		{"invalid_plan", &failingPlanner{invalidOnNext: true}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newStore()
			e := newEngine(t, s, test.task, tflow.Options{})
			m, err := e.submit(ctx, tflow.SubmitOptions{ConcurrencyLimit: 1})
			if err != nil || m.Subs.Running != 1 {
				t.Fatalf("submit: %+v %v", m, err)
			}
			sub := list(t, s, tflow.SubRunning)[0]
			r, err := e.Report(ctx, tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded})
			if test.terminal {
				if err != nil || r.Main.Status != tflow.MainFailed || r.Main.Error == "" || len(commandsOf(t, s, tflow.MainTerminated)) != 1 {
					t.Fatalf("permanent failure not closed: %+v %v", r, err)
				}
			} else {
				if !errors.Is(err, base) || r.Main.Status != tflow.MainRunning || len(commandsOf(t, s, tflow.MainTerminated)) != 0 {
					t.Fatalf("temporary failure terminalized: %+v %v", r, err)
				}
			}
		})
	}
}

func TestPermanentInitialPlanDoesNotCreateMain(t *testing.T) {
	base := errors.New("invalid source")
	s := newStore()
	e := newEngine(t, s, &failingPlanner{progressed: true, failure: tflow.Permanent(base)}, tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); !errors.Is(err, tflow.ErrPermanent) || !errors.Is(err, base) {
		t.Fatalf("classification: %v", err)
	}
	if _, err := s.GetMain(ctx); !errors.Is(err, tflow.ErrNotFound) {
		t.Fatal("invalid initial task committed")
	}
	if tflow.Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must be nil")
	}
}

type racingPlannerState struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}
type racingPlanner struct {
	position int
	state    *racingPlannerState
}

func (p *racingPlanner) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	if p.position == 0 {
		return &batch{tasks: []tflow.SubTask{&rowTask{row: 0}, &rowTask{row: 1}},
			updated: &racingPlanner{position: 2, state: p.state}, more: true}, nil
	}
	if p.state.calls.Add(1) == 1 {
		close(p.state.entered)
		<-p.state.release
		return nil, tflow.Permanent(errors.New("failure against stale snapshot"))
	}
	return &batch{tasks: []tflow.SubTask{&rowTask{row: 2}}, updated: p}, nil
}

func TestPermanentPlanFailureCannotFailNewerMainVersion(t *testing.T) {
	s := newStore()
	state := &racingPlannerState{entered: make(chan struct{}), release: make(chan struct{})}
	e := newEngine(t, s, &racingPlanner{state: state}, tflow.Options{})
	if _, err := e.submit(ctx, spec(2)); err != nil {
		t.Fatal(err)
	}
	sub := list(t, s, tflow.SubRunning)[0]
	done := make(chan error, 1)
	go func() {
		_, err := e.Report(ctx, tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded})
		done <- err
	}()
	<-state.entered
	err := s.WithTx(ctx, func(tx tflow.Tx) error {
		m, err := tx.Main()
		if err != nil {
			return err
		}
		version := m.Version
		m.Version++
		return tx.SaveMain(version, m.MainState)
	})
	close(state.release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m, _ := s.GetMain(ctx)
	if m.Status != tflow.MainRunning || m.DispatchCursor != 3 || len(commandsOf(t, s, tflow.MainTerminated)) != 0 {
		t.Fatalf("stale permanent error failed newer state: %+v", m)
	}
}

type repeatedKeyPlanner struct{}

func (p *repeatedKeyPlanner) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return &batch{tasks: []tflow.SubTask{&rowTask{}}, updated: p, more: true}, nil
}

func TestDuplicateSubKeyAcrossPagesFailsPlanningWithoutPartialPage(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, &repeatedKeyPlanner{}, tflow.Options{})
	if _, err := e.submit(ctx, tflow.SubmitOptions{ConcurrencyLimit: 1}); err != nil {
		t.Fatal(err)
	}
	r := report(t, e, list(t, s, tflow.SubRunning)[0], tflow.SubSucceeded)
	if r.Main.Status != tflow.MainFailed || r.Main.DispatchCursor != 1 || r.Main.Subs.Succeeded != 1 ||
		len(commandsOf(t, s, tflow.MainTerminated)) != 1 {
		t.Fatalf("invalid repeated page did not stop or partially committed: %+v", r.Main)
	}
}
