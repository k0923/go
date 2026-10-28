package sqlite_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/internal/storetest"
	"github.com/k0923/go/tflow/sqlite"
)

type task struct {
	Total, Next     int
	Wait, Validated bool
	Route           string
	sink            *sink
}
type unit string

func (u unit) Key() string { return string(u) }

type event struct{ ID, Value string }

func (e event) Key() string { return e.ID }

type page struct {
	tflow.Page
	Next *task
}
type sink struct {
	mu     sync.Mutex
	routes map[string]string
}

func (t *task) Plan(_ context.Context, req tflow.PlanRequest) (tflow.Plan, error) {
	next := *t
	next.Next = min(t.Total, t.Next+req.Limit())
	p := page{Next: &next}
	p.More = next.Next < t.Total
	for i := t.Next; i < next.Next; i++ {
		p.Items = append(p.Items, unit(fmt.Sprint(i)))
	}
	return p, nil
}
func (t *task) DecideMain(ctx context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
	if t.Wait && !t.Validated {
		return tflow.MainDecision{Action: tflow.Continue}, nil
	}
	return (tflow.DefaultMainDecider{}).DecideMain(ctx, view)
}
func (t *task) OnTerminated(_ context.Context, e tflow.MainTermination) error {
	if t.sink != nil {
		t.sink.mu.Lock()
		defer t.sink.mu.Unlock()
		t.sink.routes[e.ID] = t.Route
	}
	return nil
}

// This codec is deliberately application-owned. A business can replace these
// tagged JSON envelopes with protobuf or another schema without changing tflow.
type envelope struct {
	Type    string
	Version int
	Payload json.RawMessage
}
type codec struct {
	sink       *sink
	failEncode bool
	badDecode  bool
	failKind   string
}

func (c *codec) Encode(value any) ([]byte, error) {
	if c.failEncode {
		return nil, errors.New("codec unavailable")
	}
	var kind string
	switch value.(type) {
	case *task:
		kind = "batch"
	case unit:
		kind = "unit"
	case event:
		kind = "event"
	case tflow.FillPolicy:
		kind = "fill"
	case tflow.WavePolicy:
		kind = "wave"
	case tflow.BufferPolicy:
		kind = "buffer"
	case tflow.DefaultMainDecider:
		kind = "default_decider"
	default:
		return nil, fmt.Errorf("unregistered business type %T", value)
	}
	if c.failKind == kind {
		return nil, fmt.Errorf("encode %s failed", kind)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{Type: kind, Version: 1, Payload: data})
}
func (c *codec) Decode(data []byte) (any, error) {
	if c.badDecode {
		return "wrong type", nil
	}
	var e envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	if e.Version != 1 {
		return nil, fmt.Errorf("unsupported schema %d", e.Version)
	}
	var value any
	switch e.Type {
	case "batch":
		value = &task{sink: c.sink}
	case "unit":
		value = new(unit)
	case "event":
		value = new(event)
	case "fill":
		value = &tflow.FillPolicy{}
	case "wave":
		value = &tflow.WavePolicy{}
	case "buffer":
		value = &tflow.BufferPolicy{}
	case "default_decider":
		value = &tflow.DefaultMainDecider{}
	default:
		return nil, fmt.Errorf("unknown business type %q", e.Type)
	}
	if err := json.Unmarshal(e.Payload, value); err != nil {
		return nil, err
	}
	switch v := value.(type) {
	case *unit:
		return *v, nil
	case *event:
		return *v, nil
	case *tflow.FillPolicy:
		return *v, nil
	case *tflow.WavePolicy:
		return *v, nil
	case *tflow.BufferPolicy:
		return *v, nil
	case *tflow.DefaultMainDecider:
		return *v, nil
	}
	return value, nil
}
func options(c *codec) sqlite.Options {
	return sqlite.Options{Codec: c, CommitPlan: func(old tflow.MainTask, p tflow.Plan) (tflow.MainTask, error) {
		if next, ok := p.(page); ok {
			return next.Next, nil
		}
		return old, nil
	}}
}
func repository(t *testing.T, path string, c *codec) *sqlite.Repository {
	t.Helper()
	r, err := sqlite.New(t.Context(), path, options(c))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}
func open(t *testing.T, r *sqlite.Repository, id string) tflow.Store {
	t.Helper()
	s, err := r.Open(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func engine(t *testing.T, s tflow.Store, opts tflow.Options) *tflow.Engine {
	t.Helper()
	e, err := tflow.New(s, opts)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type syncExecutor func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error)

func (f syncExecutor) Start(ctx context.Context, task tflow.SubTask, ref tflow.Execution) (tflow.DispatchResult, error) {
	if f == nil {
		return tflow.DispatchResult{}, tflow.ErrExecutorUnavailable
	}
	result, err := f(ctx, task, ref)
	if err == nil && (result.Status == tflow.Accepted || result.Status == tflow.Waiting) {
		return tflow.DispatchResult{}, tflow.ErrInvalid
	}
	return result, err
}

func (syncExecutor) Resume(context.Context, tflow.SubTask, tflow.Execution, tflow.Event) (tflow.DispatchResult, error) {
	return tflow.DispatchResult{}, tflow.ErrResumeUnsupported
}

func (syncExecutor) Cancel(context.Context, tflow.SubTask, tflow.Execution) error { return nil }

func submit(t *testing.T, e *tflow.Engine, total int, concurrency int64) {
	t.Helper()
	if _, err := e.Submit(t.Context(), &task{Total: total}, tflow.SubmitOptions{ConcurrencyLimit: concurrency}); err != nil {
		t.Fatal(err)
	}
}
func finish(t *testing.T, e *tflow.Engine) {
	t.Helper()
	dispatcher, err := tflow.NewExecutorDispatcher(e, syncExecutor(func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error) {
		return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := e.DispatchPending(t.Context(), 100, dispatcher); err != nil {
			t.Fatal(err)
		}
		m, err := e.GetMain(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if m.Status.Terminal() {
			return
		}
	}
	t.Fatal("did not finish")
}

func TestStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Fixture {
		r := repository(t, filepath.Join(t.TempDir(), "contract.db"), &codec{})
		return storetest.Fixture{
			Store: open(t, r, "contract"), Main: &task{},
			Plan:    tflow.Page{Items: []tflow.SubTask{unit("a"), unit("b")}},
			Advance: func(d time.Duration) { time.Sleep(d + 5*time.Millisecond) },
		}
	})
}

func TestReopenPaginationConfigAndIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.db")
	r, err := sqlite.New(t.Context(), path, options(&codec{}))
	if err != nil {
		t.Fatal(err)
	}
	s := open(t, r, "job")
	e := engine(t, s, tflow.Options{})
	input := &task{Total: 9, Route: "parent/wait"}
	if _, err := e.Submit(t.Context(), input, tflow.SubmitOptions{
		ConcurrencyLimit: 2, PlanningPolicy: tflow.BufferPolicy{TargetPending: 2, MaxPlanSize: 4},
	}); err != nil {
		t.Fatal(err)
	}
	if input.Next != 0 {
		t.Fatal("caller task mutated")
	}
	before, err := s.GetMain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if before.Task.(*task).Next != 4 || before.DispatchCursor != 4 {
		t.Fatal(before)
	}
	// Reads must reconstruct independent business objects.
	before.Task.(*task).Next = 999
	again, err := s.GetMain(t.Context())
	if err != nil || again.Task.(*task).Next != 4 {
		t.Fatalf("alias: %+v %v", again, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = repository(t, path, &codec{})
	s = open(t, r, "job")
	e = engine(t, s, tflow.Options{PlanningPolicy: tflow.WavePolicy{Size: 1}})
	persisted, err := s.GetMain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := persisted.PlanningPolicy.(tflow.BufferPolicy); !ok || p.TargetPending != 2 {
		t.Fatal("policy changed on recovery")
	}
	finish(t, e)
	final, err := e.GetMain(t.Context())
	if err != nil || final.Status != tflow.MainSucceeded || final.Subs.Succeeded != 9 {
		t.Fatalf("%+v %v", final, err)
	}
	other := engine(t, open(t, r, "other"), tflow.Options{})
	submit(t, other, 1, 1)
	if _, err := other.Report(t.Context(), tflow.Report{Execution: tflow.Execution{MainID: "job", SubKey: "0", Attempt: 1}, Status: tflow.SubSucceeded}); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal(err)
	}
}

func TestVersionedBusinessWakeSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "validation.db")
	r, err := sqlite.New(t.Context(), path, options(&codec{}))
	if err != nil {
		t.Fatal(err)
	}
	s := open(t, r, "job")
	e := engine(t, s, tflow.Options{})
	if _, err := e.Submit(t.Context(), &task{Wait: true}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err != nil {
		t.Fatal(err)
	}
	m, err := s.GetMain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	updated := *m.Task.(*task)
	updated.Validated = true
	abort := errors.New("abort wake")
	err = s.WithTx(t.Context(), func(tx tflow.Tx) error {
		if err := tx.SaveTask(m.Version, &updated, time.Now()); err != nil {
			return err
		}
		next, err := tx.Main()
		if err != nil {
			return err
		}
		if err := tx.PutCommand(tflow.ReconcileCommand(s.MainID(), next.MainState)); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	preserved, err := s.GetMain(t.Context())
	if err != nil || preserved.Task.(*task).Validated || preserved.Version != m.Version {
		t.Fatal("rollback lost")
	}
	if _, err := e.UpdateTask(t.Context(), m.Version-1, &updated); !errors.Is(err, tflow.ErrInvalid) && !errors.Is(err, tflow.ErrConflict) {
		t.Fatal(err)
	}
	// Commit without inline reconciliation, simulating loss of the API process.
	if err := s.WithTx(t.Context(), func(tx tflow.Tx) error {
		if err := tx.SaveTask(m.Version, &updated, time.Now()); err != nil {
			return err
		}
		next, err := tx.Main()
		if err != nil {
			return err
		}
		return tx.PutCommand(tflow.ReconcileCommand(s.MainID(), next.MainState))
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = repository(t, path, &codec{})
	e = engine(t, open(t, r, "job"), tflow.Options{})
	if _, err := e.Drain(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	final, err := e.GetMain(t.Context())
	if err != nil || final.Status != tflow.MainSucceeded {
		t.Fatalf("%+v %v", final, err)
	}
}

func TestIndependentConnectionsConcurrentReportsAndSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	a := repository(t, path, &codec{})
	b := repository(t, path, &codec{})
	sa, sb := open(t, a, "job"), open(t, b, "job")
	ea, eb := engine(t, sa, tflow.Options{}), engine(t, sb, tflow.Options{})
	submit(t, ea, 30, 10)
	subs, err := sa.FindSubs(t.Context(), tflow.SubQuery{Statuses: []tflow.SubStatus{tflow.SubRunning}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, sub := range subs {
		for _, e := range []*tflow.Engine{ea, eb} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := e.Report(t.Context(), tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded})
				if err != nil {
					t.Error(err)
				}
				m, err := sb.GetMain(t.Context())
				if err != nil || tflow.ValidateMain(m) != nil {
					t.Errorf("inconsistent snapshot: %+v %v", m, err)
				}
			}()
		}
	}
	wg.Wait()
	m, err := sa.GetMain(t.Context())
	if err != nil || m.Subs.Succeeded != 10 || m.Subs.Running != 10 {
		t.Fatalf("%+v %v", m, err)
	}
	finish(t, eb)
}

func TestIndependentConnectionsLeaseFencingAndRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.db")
	a, b := repository(t, path, &codec{}), repository(t, path, &codec{})
	sa, sb := open(t, a, "job"), open(t, b, "job")
	e := engine(t, sa, tflow.Options{})
	submit(t, e, 0, 1)
	commands, err := sa.PendingCommands(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Clear all but one command to isolate competing claims.
	for i := 0; i < len(commands)-1; i++ {
		d, err := sa.ClaimCommand(t.Context(), time.Minute)
		if err != nil || d == nil {
			t.Fatal(err)
		}
		if err := sa.AckCommand(t.Context(), *d); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan *tflow.Delivery, 2)
	for _, s := range []tflow.Store{sa, sb} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := s.ClaimCommand(t.Context(), 100*time.Millisecond)
			if err != nil {
				t.Error(err)
			}
			results <- d
		}()
	}
	wg.Wait()
	close(results)
	var first *tflow.Delivery
	for d := range results {
		if d != nil {
			if first != nil {
				t.Fatal("double claim")
			}
			first = d
		}
	}
	if first == nil {
		t.Fatal("no claim")
	}
	time.Sleep(110 * time.Millisecond)
	if err := sa.AckCommand(t.Context(), *first); !errors.Is(err, tflow.ErrDeliveryLeaseLost) {
		t.Fatal(err)
	}
	second, err := sb.ClaimCommand(t.Context(), time.Minute)
	if err != nil || second == nil || second.Generation <= first.Generation {
		t.Fatalf("%+v %v", second, err)
	}
	if err := sa.ReleaseCommand(t.Context(), *first, 0, "old"); !errors.Is(err, tflow.ErrDeliveryLeaseLost) {
		t.Fatal(err)
	}
	if err := sb.ReleaseCommand(t.Context(), *second, 50*time.Millisecond, "retry"); err != nil {
		t.Fatal(err)
	}
	if d, err := sa.ClaimCommand(t.Context(), time.Minute); err != nil || d != nil {
		t.Fatalf("early retry %+v %v", d, err)
	}
	time.Sleep(60 * time.Millisecond)
	third, err := sa.ClaimCommand(t.Context(), time.Minute)
	if err != nil || third == nil || third.Command.ID != first.Command.ID {
		t.Fatal(err)
	}
	if err := sb.AckCommand(t.Context(), *third); err != nil {
		t.Fatal(err)
	}
	// Compact only payloads: acknowledgements and dedup survive reopen/GC.
	if n, err := a.CompactAcknowledged(t.Context(), time.Now().Add(time.Second)); err != nil || n == 0 {
		t.Fatalf("compact %d %v", n, err)
	}
	if err := sb.WithTx(t.Context(), func(tx tflow.Tx) error { return tx.PutCommand(third.Command) }); err != nil {
		t.Fatal(err)
	}
	if d, err := sb.ClaimCommand(t.Context(), time.Minute); err != nil || d != nil {
		t.Fatal("resurrected command")
	}
}

func TestResumeHistoricalCancellationAndTermination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorded := &sink{routes: make(map[string]string)}
	r, err := sqlite.New(t.Context(), path, options(&codec{sink: recorded}))
	if err != nil {
		t.Fatal(err)
	}
	s := open(t, r, "job")
	e := engine(t, s, tflow.Options{})
	if _, err := e.Submit(t.Context(), &task{Total: 1, Route: "old/wait"}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err != nil {
		t.Fatal(err)
	}
	executor := tflow.ExecutorFuncs{
		StartFunc: func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error) {
			return tflow.DispatchResult{Status: tflow.Waiting, ExternalRef: "old-run"}, nil
		},
		ResumeFunc: func(_ context.Context, _ tflow.SubTask, x tflow.Execution, evt tflow.Event) (tflow.DispatchResult, error) {
			if x.ExternalRef != "old-run" || evt.(event).Value != "ready" {
				return tflow.DispatchResult{}, errors.New("lost event/ref")
			}
			return tflow.DispatchResult{Status: tflow.Waiting}, nil
		},
	}
	d, err := tflow.NewExecutorDispatcher(e, executor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.DispatchPending(t.Context(), 100, d); err != nil {
		t.Fatal(err)
	}
	sub, err := s.GetSub(t.Context(), "0")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(t.Context(), sub.Execution("job"), event{ID: "wake", Value: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = repository(t, path, &codec{sink: recorded})
	s = open(t, r, "job")
	e = engine(t, s, tflow.Options{})
	d, err = tflow.NewExecutorDispatcher(e, executor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.DispatchPending(t.Context(), 100, d); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	old, err := s.PendingCommands(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Rerun(t.Context(), tflow.RerunCanceled); err != nil {
		t.Fatal(err)
	}
	m, err := s.GetMain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	next := *m.Task.(*task)
	next.Route = "new/wait"
	if _, err := e.UpdateTask(t.Context(), m.Version, &next); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Report(t.Context(), tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded}); !errors.Is(err, tflow.ErrStaleAttempt) {
		t.Fatal(err)
	}
	var canceled int
	for _, command := range old {
		if command.Command.Kind == tflow.CancelSub {
			x := tflow.ExecutorFuncs{CancelFunc: func(_ context.Context, _ tflow.SubTask, ref tflow.Execution) error {
				if ref.Attempt != 1 || ref.ExternalRef != "old-run" {
					return errors.New("cancel targeted new attempt")
				}
				canceled++
				return nil
			}}
			if err := e.Handle(t.Context(), command.Command, x); err != nil {
				t.Fatal(err)
			}
		}
		if command.Command.Kind == tflow.MainTerminated {
			if err := e.Handle(t.Context(), command.Command, nil); err != nil {
				t.Fatal(err)
			}
			recorded.mu.Lock()
			route := recorded.routes[command.Command.ID]
			recorded.mu.Unlock()
			if route != "old/wait" {
				t.Fatal("historical route replaced")
			}
		}
	}
	if canceled != 1 {
		t.Fatal("missing historical cancel")
	}
}

func TestCodecFailureAndEscapedTransaction(t *testing.T) {
	c := &codec{}
	r := repository(t, filepath.Join(t.TempDir(), "failure.db"), c)
	s := open(t, r, "job")
	e := engine(t, s, tflow.Options{})
	c.failEncode = true
	if _, err := e.Submit(t.Context(), &task{Total: 2}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err == nil {
		t.Fatal("codec failure ignored")
	}
	c.failEncode = false
	if _, err := s.GetMain(t.Context()); !errors.Is(err, tflow.ErrNotFound) {
		t.Fatal("partial insert")
	}
	submit(t, e, 2, 1)
	var escaped tflow.Tx
	if err := s.WithTx(t.Context(), func(tx tflow.Tx) error { escaped = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := escaped.Main(); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal("escaped tx usable")
	}
	c.badDecode = true
	if _, err := s.GetMain(t.Context()); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal("invalid codec type accepted")
	}
	c.badDecode = false
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.WithTx(canceled, func(tflow.Tx) error { t.Fatal("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	finish(t, e)
}

func TestDueMainsIncludesTerminalAndRespectsLease(t *testing.T) {
	r := repository(t, filepath.Join(t.TempDir(), "due.db"), &codec{})
	s := open(t, r, "a")
	e := engine(t, s, tflow.Options{})
	submit(t, e, 0, 1)
	ids, err := r.DueMains(t.Context(), 10)
	if err != nil || len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("%v %v", ids, err)
	}
	for {
		d, err := s.ClaimCommand(t.Context(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if d == nil {
			break
		}
	}
	ids, err = r.DueMains(t.Context(), 10)
	if err != nil || len(ids) != 0 {
		t.Fatalf("leased due: %v %v", ids, err)
	}
}

func TestProcessCrashAfterCommitAndBeforeAck(t *testing.T) {
	if path := os.Getenv("TFLOW_CRASH_DB"); path != "" {
		r, err := sqlite.New(context.Background(), path, options(&codec{}))
		if err != nil {
			panic(err)
		}
		s, err := r.Open(context.Background(), "job")
		if err != nil {
			panic(err)
		}
		e, err := tflow.New(s, tflow.Options{})
		if err != nil {
			panic(err)
		}
		if _, err := e.Submit(context.Background(), &task{Total: 1}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err != nil {
			panic(err)
		}
		d, err := s.ClaimCommand(context.Background(), 50*time.Millisecond)
		if err != nil || d == nil {
			panic("claim failed")
		}
		x := syncExecutor(func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error) {
			return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
		})
		if err := e.Handle(context.Background(), d.Command, x); err != nil {
			panic(err)
		}
		// Deliberate crash: neither Close nor Ack runs in this child.
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "crash.db")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessCrashAfterCommitAndBeforeAck$")
	cmd.Env = append(os.Environ(), "TFLOW_CRASH_DB="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %s %v", output, err)
	}
	time.Sleep(60 * time.Millisecond)
	r := repository(t, path, &codec{})
	s := open(t, r, "job")
	e := engine(t, s, tflow.Options{})
	var repeated int
	x := syncExecutor(func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error) {
		repeated++
		return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
	})
	d, err := tflow.NewExecutorDispatcher(e, x)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.DispatchPending(t.Context(), 100, d); err != nil {
		t.Fatal(err)
	}
	m, err := e.GetMain(t.Context())
	if err != nil || m.Status != tflow.MainSucceeded || m.Subs.Succeeded != 1 || repeated != 0 {
		t.Fatalf("crash recovery %+v repeated=%d %v", m, repeated, err)
	}
}

func TestQuarantineRequeueAndCommandPagination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dead.db")
	r := repository(t, path, &codec{})
	s := open(t, r, "job").(*sqlite.Store)
	e := engine(t, s, tflow.Options{})
	submit(t, e, 3, 3)
	all, err := s.PendingCommands(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var visited []tflow.CommandState
	after := ""
	for {
		page, err := s.FindCommands(t.Context(), tflow.CommandQuery{AfterID: after, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		visited = append(visited, page...)
		after = page[0].Command.ID
	}
	if len(visited) != len(all) {
		t.Fatal("pagination skipped commands")
	}
	starts, err := s.FindCommands(t.Context(), tflow.CommandQuery{Kinds: []tflow.CommandKind{tflow.StartSub}})
	if err != nil || len(starts) != 3 {
		t.Fatalf("filter %+v %v", starts, err)
	}
	if _, err := s.FindCommands(t.Context(), tflow.CommandQuery{Limit: -1}); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal(err)
	}
	d, err := s.ClaimCommand(t.Context(), time.Minute)
	if err != nil || d == nil {
		t.Fatal(err)
	}
	if err := s.QuarantineCommand(t.Context(), *d, "operator review"); err != nil {
		t.Fatal(err)
	}
	if err := s.AckCommand(t.Context(), *d); !errors.Is(err, tflow.ErrDeliveryLeaseLost) {
		t.Fatal("quarantined owner acknowledged")
	}
	if err := s.ReleaseCommand(t.Context(), *d, 0, "old owner"); !errors.Is(err, tflow.ErrDeliveryLeaseLost) {
		t.Fatal(err)
	}
	b := repository(t, path, &codec{})
	other := open(t, b, "job").(*sqlite.Store)
	dead, err := other.DeadLetters(t.Context(), tflow.CommandQuery{})
	if err != nil || len(dead) != 1 || dead[0].LastError != "operator review" {
		t.Fatalf("%+v %v", dead, err)
	}
	if err := other.RequeueCommand(t.Context(), d.Command.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.QuarantineCommand(t.Context(), *d, "stale"); !errors.Is(err, tflow.ErrDeliveryLeaseLost) {
		t.Fatal(err)
	}
	for {
		next, err := other.ClaimCommand(t.Context(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if next == nil {
			t.Fatal("requeued command lost")
		}
		if next.Command.ID == d.Command.ID {
			if next.Generation <= d.Generation {
				t.Fatal("requeue did not fence owner")
			}
			break
		}
		if err := other.AckCommand(t.Context(), *next); err != nil {
			t.Fatal(err)
		}
	}
}

type rejectingStore struct{ tflow.Store }
type rejectingTx struct{ tflow.Tx }

func (tx rejectingTx) PutCommand(c tflow.Command) error {
	if c.Kind == tflow.MainTerminated {
		return errors.New("terminal write failed")
	}
	return tx.Tx.PutCommand(c)
}
func (s rejectingStore) WithTx(ctx context.Context, fn func(tflow.Tx) error) error {
	return s.Store.WithTx(ctx, func(tx tflow.Tx) error { return fn(rejectingTx{tx}) })
}

func TestTerminalStateAndCommandRollbackOnSQLite(t *testing.T) {
	for _, operation := range []string{"cancel", "fail", "complete"} {
		t.Run(operation, func(t *testing.T) {
			r := repository(t, filepath.Join(t.TempDir(), "atomic.db"), &codec{})
			s := open(t, r, "job")
			e := engine(t, rejectingStore{s}, tflow.Options{})
			submit(t, e, 1, 1)
			var err error
			switch operation {
			case "cancel":
				_, err = e.Cancel(t.Context())
			case "fail":
				_, err = e.Fail(t.Context(), "stop")
			case "complete":
				_, err = e.Report(t.Context(), tflow.Report{Execution: tflow.Execution{MainID: "job", SubKey: "0", Attempt: 1}, Status: tflow.SubSucceeded})
			}
			if err == nil {
				t.Fatal("terminal command error ignored")
			}
			m, err := s.GetMain(t.Context())
			if err != nil || m.Status != tflow.MainRunning {
				t.Fatalf("partial terminal state %+v %v", m, err)
			}
			if operation != "complete" && m.Subs.Running != 1 {
				t.Fatal("cancel escaped rollback")
			}
			all, err := s.PendingCommands(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range all {
				if c.Command.Kind == tflow.MainTerminated {
					t.Fatal("rolled back notification persisted")
				}
			}
		})
	}
}

func TestConcurrentRerunHasOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rerun.db")
	a, b := repository(t, path, &codec{}), repository(t, path, &codec{})
	s := open(t, a, "job")
	e := engine(t, s, tflow.Options{})
	submit(t, e, 2, 2)
	if _, err := e.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	success := make(chan bool, 2)
	for _, e := range []*tflow.Engine{e, engine(t, open(t, b, "job"), tflow.Options{})} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Rerun(t.Context(), tflow.RerunCanceled)
			if err != nil && !errors.Is(err, tflow.ErrInvalid) {
				t.Error(err)
			}
			success <- err == nil
		}()
	}
	wg.Wait()
	close(success)
	winners := 0
	for ok := range success {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d rerun winners", winners)
	}
	subs, err := s.FindSubs(t.Context(), tflow.SubQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range subs {
		if sub.Attempt != 2 || sub.Status != tflow.SubRunning {
			t.Fatal(sub)
		}
	}
}

func TestCallbackPanicRollsBackAndReleasesConnection(t *testing.T) {
	r := repository(t, filepath.Join(t.TempDir(), "panic.db"), &codec{})
	s := open(t, r, "job")
	e := engine(t, s, tflow.Options{})
	submit(t, e, 1, 1)
	before, err := s.GetMain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("callback panic not propagated")
			}
		}()
		err := s.WithTx(t.Context(), func(tx tflow.Tx) error {
			m, err := tx.Main()
			if err != nil {
				return err
			}
			m.Version++
			m.Error = "uncommitted"
			if err := tx.SaveMain(before.Version, m.MainState); err != nil {
				return err
			}
			panic("callback")
		})
		if err != nil {
			t.Fatal(err)
		}
	}()
	after, err := s.GetMain(t.Context())
	if err != nil || after.Version != before.Version || after.Error != "" {
		t.Fatalf("%+v %v", after, err)
	}
	finish(t, e)
}

func TestCodecWriteFailuresAreAtomic(t *testing.T) {
	for _, kind := range []string{"batch", "fill", "default_decider", "unit"} {
		t.Run(kind, func(t *testing.T) {
			c := &codec{failKind: kind}
			r := repository(t, filepath.Join(t.TempDir(), "codec.db"), c)
			s := open(t, r, "job")
			e := engine(t, s, tflow.Options{})
			if _, err := e.Submit(t.Context(), &task{Total: 1}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err == nil {
				t.Fatal("failed codec committed")
			}
			c.failKind = ""
			if _, err := s.GetMain(t.Context()); !errors.Is(err, tflow.ErrNotFound) {
				t.Fatal("partial main")
			}
			if _, err := s.GetSub(t.Context(), "0"); !errors.Is(err, tflow.ErrNotFound) {
				t.Fatal("partial sub")
			}
			submit(t, e, 1, 1)
		})
	}
	c := &codec{}
	r := repository(t, filepath.Join(t.TempDir(), "event.db"), c)
	s := open(t, r, "job")
	e := engine(t, s, tflow.Options{})
	submit(t, e, 1, 1)
	sub, err := s.GetSub(t.Context(), "0")
	if err != nil {
		t.Fatal(err)
	}
	c.failKind = "event"
	if err := e.Resume(t.Context(), sub.Execution("job"), event{ID: "wake"}); err == nil {
		t.Fatal("failed event committed")
	}
	c.failKind = ""
	all, err := s.PendingCommands(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.Command.Kind == tflow.ResumeSub {
			t.Fatal("partial event persisted")
		}
	}
}

func TestInitializationAndClosedDatabaseErrors(t *testing.T) {
	if _, err := sqlite.New(t.Context(), "", options(&codec{})); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := sqlite.New(t.Context(), filepath.Join(t.TempDir(), "bad.db"), sqlite.Options{}); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := sqlite.New(t.Context(), t.TempDir(), options(&codec{})); err == nil {
		t.Fatal("directory opened as database")
	}
	path := filepath.Join(t.TempDir(), "schema.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE tflow_schema(version INTEGER NOT NULL); INSERT INTO tflow_schema VALUES(99)"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.New(t.Context(), path, options(&codec{})); err == nil {
		t.Fatal("unsupported schema accepted")
	}
	r := repository(t, filepath.Join(t.TempDir(), "closed.db"), &codec{})
	s := open(t, r, "job").(*sqlite.Store)
	submit(t, engine(t, s, tflow.Options{}), 1, 1)
	if _, err := r.Open(t.Context(), ""); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Open(canceled, "job"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.ClaimCommand(t.Context(), 0); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal(err)
	}
	if err := s.ReleaseCommand(t.Context(), tflow.Delivery{}, -1, ""); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := r.DueMains(t.Context(), 0); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, check := range []func() error{
		func() error { _, err := s.GetMain(t.Context()); return err },
		func() error { _, err := s.GetSub(t.Context(), "0"); return err },
		func() error { _, err := s.FindSubs(t.Context(), tflow.SubQuery{}); return err },
		func() error { return s.WithTx(t.Context(), func(tflow.Tx) error { return nil }) },
		func() error { _, err := s.ClaimCommand(t.Context(), time.Minute); return err },
		func() error { _, err := s.PendingCommands(t.Context()); return err },
		func() error { _, err := r.DueMains(t.Context(), 1); return err },
		func() error { _, err := r.CompactAcknowledged(t.Context(), time.Now()); return err },
	} {
		if err := check(); err == nil {
			t.Fatal("closed database error ignored")
		}
	}
}

func TestEngineClockCannotExpireSQLiteLease(t *testing.T) {
	r := repository(t, filepath.Join(t.TempDir(), "clock.db"), &codec{})
	s := open(t, r, "job")
	e := engine(t, s, tflow.Options{Clock: tflow.ClockFunc(func() time.Time { return time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC) })})
	submit(t, e, 1, 1)
	finish(t, e)
}
