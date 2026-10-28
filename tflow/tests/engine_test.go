package tflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

var ctx = context.Background()

type testDeliveryClock struct{ seconds atomic.Int64 }

func (c *testDeliveryClock) Now() time.Time { return time.Unix(1000000000+c.seconds.Load(), 0) }
func (c *testDeliveryClock) advance()       { c.seconds.Add(120) }

var deliveryClock = &testDeliveryClock{}

type source struct {
	total    int64
	position int64
	state    *sourceState
}

type sourceState struct {
	mu          sync.Mutex
	cursors     []int64
	limits      []int
	counts      []int
	unavailable *atomic.Bool
	failure     error
}

func newSource(total int64) *source { return &source{total: total, state: &sourceState{}} }

type rowTask struct{ row int64 }

func (r *rowTask) Key() string { return fmt.Sprint(r.row) }

type batch struct {
	tasks   []tflow.SubTask
	updated tflow.MainTask
	more    bool
}

func (p *batch) Tasks() []tflow.SubTask { return p.tasks }
func (p *batch) HasMore() bool          { return p.more }

// The test adapter understands fixture plans; tflow never interprets progress.
func newRepository() *memory.Repository {
	return memory.New(memory.WithClock(deliveryClock), memory.WithPlanCommit(func(task tflow.MainTask, plan tflow.Plan) (tflow.MainTask, error) {
		if page, ok := plan.(*batch); ok && !tflow.IsNil(page.updated) {
			return page.updated, nil
		}
		return task, nil
	}))
}

func newStore() *memory.Store { return newRepository().ForMain("job") }

type event struct{ id, kind string }

func (e event) Key() string { return e.id }

func (s *source) Plan(_ context.Context, req tflow.PlanRequest) (tflow.Plan, error) {
	if s.position > 0 && s.state.unavailable != nil && s.state.unavailable.Load() {
		return nil, s.state.failure
	}
	s.state.mu.Lock()
	s.state.cursors = append(s.state.cursors, s.position)
	s.state.limits = append(s.state.limits, req.Limit())
	s.state.mu.Unlock()
	end := min(s.total, s.position+int64(req.Limit()))
	next := *s
	next.position = end
	result := &batch{updated: &next, more: end < s.total}
	for i := s.position; i < end; i++ {
		result.tasks = append(result.tasks, &rowTask{row: i})
	}
	s.state.mu.Lock()
	s.state.counts = append(s.state.counts, len(result.tasks))
	s.state.mu.Unlock()
	return result, nil
}

type testEngine struct {
	*tflow.Engine
	initialTask tflow.MainTask
}

func (e *testEngine) submit(ctx context.Context, options tflow.SubmitOptions) (tflow.MainProgress, error) {
	return e.Engine.Submit(ctx, e.initialTask, options)
}

func newEngine(t *testing.T, s tflow.Store, task tflow.MainTask, opts tflow.Options) *testEngine {
	t.Helper()
	e, err := tflow.New(s, opts)
	if err != nil {
		t.Fatal(err)
	}
	return &testEngine{Engine: e, initialTask: task}
}

func spec(concurrency int64) tflow.SubmitOptions {
	return tflow.SubmitOptions{ConcurrencyLimit: concurrency}
}

func list(t *testing.T, s tflow.Store, statuses ...tflow.SubStatus) []tflow.SubRecord {
	t.Helper()
	subs, err := s.FindSubs(ctx, tflow.SubQuery{Statuses: statuses})
	if err != nil {
		t.Fatal(err)
	}
	return subs
}

func report(t *testing.T, e *testEngine, sub tflow.SubRecord, status tflow.SubStatus) tflow.ReportReceipt {
	t.Helper()
	r, err := e.Report(ctx, tflow.Report{Execution: sub.Execution("job"), Status: status})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func finishAll(t *testing.T, e *testEngine, s tflow.Store) tflow.MainProgress {
	t.Helper()
	for i := 0; i < 1000; i++ {
		m, err := s.GetMain(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if m.Status.Terminal() {
			return m.MainProgress
		}
		subs := list(t, s, tflow.SubRunning)
		if len(subs) == 0 {
			t.Fatal("running main has no executable subs")
		}
		for _, sub := range subs {
			report(t, e, sub, tflow.SubSucceeded)
		}
	}
	t.Fatal("main did not converge")
	return tflow.MainProgress{}
}

func TestSubmitWindowPendingAndLazyRefill(t *testing.T) {
	s, planner := newStore(), newSource(7)
	d := &memory.FakeDispatcher{}
	d.Handler = func(_ context.Context, c tflow.Command) error {
		if c.Kind == tflow.MainTerminated {
			return nil
		}
		sub, err := s.GetSub(ctx, c.Sub.Key)
		if err != nil || sub.Status != tflow.SubRunning {
			return fmt.Errorf("dispatch before commit: %v", err)
		}
		return nil
	}
	e := newEngine(t, s, planner, tflow.Options{PlanningPolicy: tflow.BufferPolicy{TargetPending: 4, MaxPlanSize: 5}, Dispatcher: d})
	m, err := e.submit(ctx, spec(2))
	if err != nil {
		t.Fatal(err)
	}
	if m.Subs.Running != 2 || m.DispatchCursor != 5 || len(list(t, s, tflow.SubPending)) != 3 || len(d.Commands()) != 2 {
		t.Fatalf("initial window: %+v", m)
	}
	first := list(t, s, tflow.SubRunning)[0]
	r := report(t, e, first, tflow.SubSucceeded)
	if r.Main.Subs.Succeeded != 1 || r.Main.Subs.Running != 2 || r.Main.DispatchCursor != 5 {
		t.Fatalf("pending activation: %+v", r.Main)
	}
	if len(planner.state.cursors) != 1 {
		t.Fatal("planned before consuming pending")
	}
	m = finishAll(t, e, s)
	if m.Status != tflow.MainSucceeded || m.Subs.Succeeded != 7 || m.DispatchCursor != 7 || !m.DispatchExhausted {
		t.Fatalf("lazy completion: %+v", m)
	}
}

func TestHasMoreAndEmptyTasks(t *testing.T) {
	t.Run("paged_source", func(t *testing.T) {
		s := newStore()
		e := newEngine(t, s, newSource(5), tflow.Options{PlanningPolicy: tflow.FillPolicy{}})
		input := spec(2)
		m, err := e.submit(ctx, input)
		if err != nil || m.DispatchCursor != 2 || m.DispatchExhausted {
			t.Fatalf("dynamic initial: %+v %v", m, err)
		}
		m = finishAll(t, e, s)
		if m.DispatchCursor != 5 || m.Subs.Succeeded != 5 || !m.DispatchExhausted {
			t.Fatalf("dynamic final: %+v", m)
		}
	})
	t.Run("empty", func(t *testing.T) {
		s := newStore()
		e := newEngine(t, s, newSource(0), tflow.Options{})
		m, err := e.submit(ctx, spec(3))
		if err != nil || m.Status != tflow.MainSucceeded || m.DispatchCursor != 0 {
			t.Fatalf("empty: %+v %v", m, err)
		}
	})
}

func TestDuplicateTerminalAndCompletionPriority(t *testing.T) {
	for _, test := range []struct {
		name     string
		statuses []tflow.SubStatus
		want     tflow.MainStatus
	}{
		{"success", []tflow.SubStatus{tflow.SubSucceeded, tflow.SubSucceeded}, tflow.MainSucceeded},
		{"cancel", []tflow.SubStatus{tflow.SubSucceeded, tflow.SubCanceled}, tflow.MainCanceled},
		{"failure_wins", []tflow.SubStatus{tflow.SubCanceled, tflow.SubFailed}, tflow.MainFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newStore()
			e := newEngine(t, s, newSource(2), tflow.Options{})
			if _, err := e.submit(ctx, spec(2)); err != nil {
				t.Fatal(err)
			}
			subs := list(t, s, tflow.SubRunning)
			report(t, e, subs[0], test.statuses[0])
			r := report(t, e, subs[0], test.statuses[0])
			if r.Changed || r.Main.Subs.TerminalCount() != 1 {
				t.Fatalf("duplicate counted: %+v", r)
			}
			r = report(t, e, subs[1], test.statuses[1])
			if r.Main.Status != test.want || r.Main.Subs.Running != 0 || r.Main.Subs.TerminalCount() != 2 {
				t.Fatalf("completion: %+v", r.Main)
			}
			if err := e.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			current, err := e.GetMain(ctx)
			if err != nil || current != r.Main || len(commandsOf(t, s, tflow.MainTerminated)) != 1 {
				t.Fatalf("repeated reconciliation changed terminal state: %+v %v", current, err)
			}
		})
	}
}

func TestConcurrentReportersAcrossEngines(t *testing.T) {
	s, planner := newStore(), newSource(30)
	first, second := newEngine(t, s, planner, tflow.Options{}), newEngine(t, s, planner, tflow.Options{})
	if _, err := first.submit(ctx, spec(3)); err != nil {
		t.Fatal(err)
	}
	subs := list(t, s, tflow.SubRunning)
	var wg sync.WaitGroup
	errs := make(chan error, 90)
	for i := 0; i < 90; i++ {
		sub := subs[i%len(subs)]
		e := first
		if i%2 == 0 {
			e = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Report(ctx, tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	m, err := s.GetMain(ctx)
	if err != nil || m.Subs.Succeeded != 3 || m.Subs.Running != 3 || m.DispatchCursor != 6 {
		t.Fatalf("concurrent refill: %+v %v", m, err)
	}
	final := finishAll(t, first, s)
	if final.Subs.Succeeded != 30 {
		t.Fatalf("count drift: %+v", final)
	}
}

type conflictStore struct {
	tflow.Store
	abortNext atomic.Bool
}

func (s *conflictStore) WithTx(ctx context.Context, fn func(tflow.Tx) error) error {
	return s.Store.WithTx(ctx, func(tx tflow.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if s.abortNext.CompareAndSwap(true, false) {
			return tflow.ErrConflict
		}
		return nil
	})
}

func TestReporterRetriesTransactionCASConflict(t *testing.T) {
	s := &conflictStore{Store: newStore()}
	e := newEngine(t, s, newSource(2), tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	sub := list(t, s, tflow.SubRunning)[0]
	s.abortNext.Store(true)
	r := report(t, e, sub, tflow.SubSucceeded)
	if !r.Changed || r.Main.Subs.Succeeded != 1 || r.Main.Subs.Running != 1 {
		t.Fatalf("CAS retry: %+v", r)
	}
}

func TestCancelAndRerunAttempts(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(7), tflow.Options{PlanningPolicy: tflow.BufferPolicy{TargetPending: 4, MaxPlanSize: 5}})
	if _, err := e.submit(ctx, spec(2)); err != nil {
		t.Fatal(err)
	}
	old := list(t, s, tflow.SubRunning)[0]
	stop, err := e.Cancel(ctx)
	if err != nil || stop.Main.Status != tflow.MainCanceled || stop.Main.Subs.Canceled != 5 || len(stop.SubTasks) != 5 {
		t.Fatalf("cancel: %+v %v", stop, err)
	}
	if len(list(t, s, tflow.SubRunning, tflow.SubPending)) != 0 {
		t.Fatal("cancel left active subs")
	}
	rerun, err := e.Rerun(ctx, tflow.RerunCanceled)
	if err != nil || rerun.Main.Subs.Running != 2 || rerun.Main.Subs.Canceled != 0 {
		t.Fatalf("rerun: %+v %v", rerun, err)
	}
	sub, err := s.GetSub(ctx, old.Key)
	if err != nil || sub.Attempt != 2 || sub.ExternalRef != "" {
		t.Fatalf("attempt: %+v %v", sub, err)
	}
	if _, err := e.Report(ctx, tflow.Report{Execution: old.Execution("job"), Status: tflow.SubSucceeded}); !errors.Is(err, tflow.ErrStaleAttempt) {
		t.Fatalf("old report accepted: %v", err)
	}
	if err := e.Resume(ctx, old.Execution("job"), event{id: "old"}); !errors.Is(err, tflow.ErrStaleAttempt) {
		t.Fatalf("old wake: %v", err)
	}
	if m := finishAll(t, e, s); m.Subs.Succeeded != 7 {
		t.Fatalf("resumed unplanned tail: %+v", m)
	}
}

func TestSelectiveRerunAndNoTargets(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(3), tflow.Options{})
	if _, err := e.submit(ctx, spec(3)); err != nil {
		t.Fatal(err)
	}
	subs := list(t, s, tflow.SubRunning)
	report(t, e, subs[0], tflow.SubFailed)
	report(t, e, subs[1], tflow.SubCanceled)
	report(t, e, subs[2], tflow.SubSucceeded)
	r, err := e.RerunSubTasks(ctx, []string{subs[0].Key})
	if err != nil || r.Main.Subs.Failed != 0 || r.Main.Subs.Canceled != 1 || r.Main.Subs.Succeeded != 1 {
		t.Fatalf("selective: %+v %v", r, err)
	}
	sub, _ := s.GetSub(ctx, subs[0].Key)
	report(t, e, sub, tflow.SubSucceeded)
	if _, err := e.Rerun(ctx, tflow.RerunFailed); !errors.Is(err, tflow.ErrNoSubTaskToRerun) {
		t.Fatalf("empty scope: %v", err)
	}
	if _, err := e.RerunSubTasks(ctx, []string{subs[2].Key}); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatalf("succeeded rerun: %v", err)
	}
}

func TestDispatchFailurePreservesStateAndIntent(t *testing.T) {
	s := newStore()
	d := &memory.FakeDispatcher{}
	failure := errors.New("transport down")
	d.SetError(failure)
	e := newEngine(t, s, newSource(2), tflow.Options{Dispatcher: d})
	m, err := e.submit(ctx, spec(2))
	var committed *tflow.CommittedError
	if !errors.Is(err, failure) || !errors.As(err, &committed) || m.Subs.Running != 2 {
		t.Fatalf("dispatch failure: %+v %v", m, err)
	}
	pending, err := s.PendingCommands(ctx)
	if err != nil || len(pending) == 0 {
		t.Fatal("lost durable work")
	}
	failedCommands := d.Commands()
	d.SetError(nil)
	deliveryClock.advance()
	if _, err := e.DispatchPending(ctx, 100, d); err != nil {
		t.Fatal(err)
	}
	retried := d.Commands()[len(failedCommands):]
	for _, failed := range failedCommands {
		found := false
		for _, retry := range retried {
			found = found || retry.ID == failed.ID
		}
		if !found {
			t.Fatal("delivery retry changed idempotency key")
		}
	}
	current, _ := s.GetMain(ctx)
	if current.Status != tflow.MainRunning || current.Subs.Running != 2 {
		t.Fatalf("transport altered domain: %+v", current)
	}
}

func TestCompletionFailureAndCancelRace(t *testing.T) {
	for i := 0; i < 40; i++ {
		s := newStore()
		e := newEngine(t, s, newSource(1), tflow.Options{})
		if _, err := e.submit(ctx, spec(1)); err != nil {
			t.Fatal(err)
		}
		sub := list(t, s, tflow.SubRunning)[0]
		var wg sync.WaitGroup
		errs := make(chan error, 3)
		for _, operation := range []func() error{
			func() error {
				_, err := e.Report(ctx, tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded})
				return err
			},
			func() error { _, err := e.Cancel(ctx); return err },
			func() error { _, err := e.Fail(ctx, "stop"); return err },
		} {
			wg.Add(1)
			go func(operation func() error) { defer wg.Done(); errs <- operation() }(operation)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil && !errors.Is(err, tflow.ErrAlreadyTerminal) && !errors.Is(err, tflow.ErrConflict) {
				t.Fatal(err)
			}
		}
		m, _ := s.GetMain(ctx)
		if !m.Status.Terminal() || m.Subs.Running != 0 || m.Subs.TerminalCount() != 1 {
			t.Fatalf("race drift: %+v", m)
		}
	}
}

func TestMainDeciderCancelsActiveWork(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(10), tflow.Options{
		MainDecider: tflow.MainDeciderFunc(func(ctx context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
			if view.Summary().Failed > 0 {
				return tflow.MainDecision{Action: tflow.Fail, Reason: "failure threshold"}, nil
			}
			return tflow.DefaultMainDecider{}.DecideMain(ctx, view)
		}),
	})
	if _, err := e.submit(ctx, spec(3)); err != nil {
		t.Fatal(err)
	}
	r := report(t, e, list(t, s, tflow.SubRunning)[0], tflow.SubFailed)
	if r.Main.Status != tflow.MainFailed || r.Main.Subs.Failed != 1 || r.Main.Subs.Canceled != 2 || r.Main.DispatchCursor != 3 {
		t.Fatalf("circuit break: %+v", r.Main)
	}
}

func TestDeliveryPanicPreservesCommand(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(1), tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	_, err := e.DispatchPending(ctx, 1, tflow.DispatcherFunc(func(context.Context, tflow.Command) error { panic("broken adapter") }))
	if err == nil || !strings.Contains(err.Error(), "broken adapter") {
		t.Fatalf("panic: %v", err)
	}
	commands, err := s.PendingCommands(ctx)
	if err != nil || len(commands) == 0 || !commands[0].ExpiresAt.IsZero() {
		t.Fatalf("panic lost work: %+v %v", commands, err)
	}
}

func TestPlannerFailureAfterReportIsRecoverable(t *testing.T) {
	s, src := newStore(), newSource(5)
	var unavailable atomic.Bool
	failure := errors.New("source unavailable")
	src.state.unavailable, src.state.failure = &unavailable, failure
	e := newEngine(t, s, src, tflow.Options{})
	if _, err := e.submit(ctx, spec(2)); err != nil {
		t.Fatal(err)
	}
	sub := list(t, s, tflow.SubRunning)[0]
	unavailable.Store(true)
	r, err := e.Report(ctx, tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded})
	var committed *tflow.CommittedError
	if !errors.Is(err, failure) || !errors.As(err, &committed) || !r.Changed || r.Main.Subs.Succeeded != 1 {
		t.Fatalf("lost committed report: %+v %v", r, err)
	}
	pending, err := s.PendingCommands(ctx)
	if err != nil || len(pending) == 0 {
		t.Fatal("lost recovery command")
	}
	unavailable.Store(false)
	restarted := newEngine(t, s, nil, tflow.Options{})
	if _, err := restarted.DispatchPending(ctx, 100, &memory.FakeDispatcher{}); err != nil {
		t.Fatal(err)
	}
	if m := finishAll(t, restarted, s); m.Subs.Succeeded != 5 {
		t.Fatalf("recovery: %+v", m)
	}
}

type filteredSource struct{ visible bool }

func (s *filteredSource) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	if !s.visible {
		return &batch{updated: &filteredSource{visible: true}, more: true}, nil
	}
	return &batch{tasks: []tflow.SubTask{&rowTask{row: 0}}, updated: s}, nil
}

func TestEmptySourcePageCommitsBusinessState(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, &filteredSource{}, tflow.Options{})
	input := spec(1)
	m, err := e.submit(ctx, input)
	if err != nil || m.DispatchCursor != 1 || m.Subs.Running != 1 || !m.DispatchExhausted {
		t.Fatalf("empty page: %+v %v", m, err)
	}
}

func TestConcurrentRerunDoesNotIncrementAttemptTwice(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(2), tflow.Options{})
	if _, err := e.submit(ctx, spec(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var succeeded atomic.Int64
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Rerun(ctx, tflow.RerunCanceled)
			if err == nil {
				succeeded.Add(1)
			} else if !errors.Is(err, tflow.ErrInvalid) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if succeeded.Load() != 1 {
		t.Fatalf("rerun winners=%d", succeeded.Load())
	}
	for _, sub := range list(t, s) {
		if sub.Attempt != 2 || sub.Status != tflow.SubRunning {
			t.Fatalf("duplicate rerun: %+v", sub)
		}
	}
}

type pageConflictStore struct {
	tflow.Store
	failPage atomic.Bool
}

func (s *pageConflictStore) WithTx(ctx context.Context, fn func(tflow.Tx) error) error {
	return s.Store.WithTx(ctx, func(tx tflow.Tx) error {
		before, readErr := tx.Main()
		if err := fn(tx); err != nil {
			return err
		}
		after, err := tx.Main()
		if err != nil {
			return err
		}
		if readErr == nil && after.DispatchCursor > before.DispatchCursor && s.failPage.CompareAndSwap(true, false) {
			return tflow.ErrConflict
		}
		return nil
	})
}

func TestBusinessProgressDoesNotAdvanceWhenPageCommitRollsBack(t *testing.T) {
	s := &pageConflictStore{Store: newStore()}
	task := newSource(6)
	e := newEngine(t, s, task, tflow.Options{})
	_, err := e.submit(ctx, spec(2))
	if err != nil {
		t.Fatal(err)
	}
	previousRecord, _ := s.GetMain(ctx)
	previous := previousRecord.Task.(*source)
	if task.position != 0 || previous.position != 2 {
		t.Fatal("submission mutated the caller's task")
	}
	sub := list(t, s, tflow.SubRunning)[0]
	s.failPage.Store(true)
	r := report(t, e, sub, tflow.SubSucceeded)
	currentRecord, _ := s.GetMain(ctx)
	if previous.position != 2 || currentRecord.Task.(*source).position != 3 || r.Main.DispatchCursor != 3 {
		t.Fatalf("rolled-back progress leaked: previous=%d current=%+v", previous.position, r.Main)
	}
	task.state.mu.Lock()
	cursors := append([]int64(nil), task.state.cursors...)
	task.state.mu.Unlock()
	if len(cursors) != 3 || cursors[0] != 0 || cursors[1] != 2 || cursors[2] != 2 {
		t.Fatalf("CAS retry did not replan from committed business state: %v", cursors)
	}
}

type stalledTask struct{}

func (s *stalledTask) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return &batch{updated: s, more: true}, nil
}

func TestOpaqueEmptyPlansAreBoundedWithoutComparingBusinessState(t *testing.T) {
	s := newStore()
	task := &stalledTask{}
	e := newEngine(t, s, task, tflow.Options{MaxReconcilePasses: 2})
	input := spec(1)
	m, err := e.submit(ctx, input)
	var committed *tflow.CommittedError
	if !errors.Is(err, tflow.ErrReconcileLimit) || !errors.As(err, &committed) || m.Status != tflow.MainRunning {
		t.Fatalf("unbounded/no-op planning: %+v %v", m, err)
	}
	currentRecord, _ := s.GetMain(ctx)
	if currentRecord.Task != task || m.DispatchCursor != 0 {
		t.Fatal("framework changed the opaque task")
	}
	pending, err := s.PendingCommands(ctx)
	if err != nil || len(pending) == 0 {
		t.Fatal("bounded planning lost recovery intent")
	}
}
