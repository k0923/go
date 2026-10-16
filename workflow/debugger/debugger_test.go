package debugger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0923/go/workflow"
)

type fixture struct {
	store  *Store
	server *httptest.Server
	now    time.Time
	mu     sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "debug.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: store, now: time.Unix(1_800_000_000, 0)}
	store.now = f.clock
	f.server = httptest.NewServer(NewServer(store))
	t.Cleanup(func() { f.server.Close(); store.Close() })
	return f
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *fixture) engine(t *testing.T, run string) (*workflow.Engine, *Client) {
	t.Helper()
	client, err := NewClient(f.server.URL, run)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := workflow.NewEngine(client, workflow.Options{Now: f.clock, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return engine, client
}

func definition[I, O any](flow workflow.FlowFunc[I, O]) workflow.Workflow[I, O] {
	return workflow.Workflow[I, O]{Name: "test", Version: "v1", Flow: flow}
}

func TestWaitForSubmitOverHTTP(t *testing.T) {
	f := newFixture(t)
	engine, client := f.engine(t, "run-a")
	ctx := context.Background()
	calls := 0
	flow := definition(func(fc *workflow.FlowContext, in int) (string, error) {
		n, err := fc.Run("double", func(context.Context, int) (int, error) { calls++; return in * 2, nil }, in)
		if err != nil {
			return "", err
		}
		answer, err := fc.WaitFor[map[string]string]("approval", map[string]int{"n": n})
		if err != nil {
			return "", err
		}
		return answer["by"], nil
	})
	result, err := engine.Drive(ctx, flow, 21)
	if err != nil || result.Status != workflow.Suspended || result.Pending[0].Key != "approval" {
		t.Fatalf("%+v %v", result, err)
	}
	detail, err := client.Detail(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var request *RecordView
	for i := range detail.Records {
		if detail.Records[i].Key == "approval" {
			request = &detail.Records[i]
		}
	}
	if request == nil || string(request.Input) != `{"n":42}` || request.Status != workflow.Suspended {
		t.Fatalf("request not visible: %+v", request)
	}
	if ok, err := client.Submit(ctx, "approval", map[string]string{"by": "alice"}); !ok || err != nil {
		t.Fatalf("submit %t %v", ok, err)
	}
	if ok, err := client.Submit(ctx, "approval", map[string]string{"by": "mallory"}); ok || err != nil {
		t.Fatalf("second submit must be ignored: %t %v", ok, err)
	}
	wakes, err := client.WaitWake(ctx, time.Second)
	if err != nil || len(wakes) != 1 || wakes[0].Key != "approval" {
		t.Fatalf("wake %+v %v", wakes, err)
	}
	result, err = engine.Drive(ctx, flow, 21)
	if err != nil || result.Status != workflow.Completed || result.Output != "alice" || calls != 1 {
		t.Fatalf("%+v %v calls=%d", result, err, calls)
	}
	if _, err = engine.Drive(ctx, flow, 22); !errors.Is(err, workflow.ErrChanged) {
		t.Fatalf("changed input must be rejected: %v", err)
	}
}

type poll struct{ N int }

func (p poll) Done() bool { return p.N >= 2 }

func TestLoopSleepAndFireOverHTTP(t *testing.T) {
	f := newFixture(t)
	engine, client := f.engine(t, "run-loop")
	ctx := context.Background()
	calls := 0
	flow := definition(func(fc *workflow.FlowContext, _ int) (poll, error) {
		return fc.Loop("poll", func(context.Context, int) (poll, error) { calls++; return poll{N: calls}, nil },
			0, workflow.LoopOptions{Interval: time.Hour, MaxAttempts: 3})
	})
	for i := 0; i < 3; i++ {
		result, err := engine.Drive(ctx, flow, 0)
		if err != nil || result.Status != workflow.Suspended || calls != 1 {
			t.Fatalf("%+v %v calls=%d", result, err, calls)
		}
	}
	if wakes, _ := client.WaitWake(ctx, 10*time.Millisecond); len(wakes) != 0 {
		t.Fatalf("sleep must not be due: %+v", wakes)
	}
	if err := client.FireSleep(ctx, "poll|sleep_1"); err != nil {
		t.Fatal(err)
	}
	if wakes, _ := client.WaitWake(ctx, time.Second); len(wakes) == 0 {
		t.Fatal("fired sleep must wake")
	}
	result, err := engine.Drive(ctx, flow, 0)
	if err != nil || result.Status != workflow.Completed || result.Output.N != 2 || calls != 2 {
		t.Fatalf("%+v %v calls=%d", result, err, calls)
	}
}

func TestSleepMaturesByClock(t *testing.T) {
	f := newFixture(t)
	engine, client := f.engine(t, "run-sleep")
	ctx := context.Background()
	flow := definition(func(fc *workflow.FlowContext, in int) (int, error) { return in, fc.Sleep("nap", time.Minute) })
	if result, _ := engine.Drive(ctx, flow, 7); result.Status != workflow.Suspended {
		t.Fatal(result)
	}
	f.advance(time.Minute)
	wakes, err := client.WaitWake(ctx, time.Second)
	if err != nil || len(wakes) != 1 || wakes[0].Key != "nap" {
		t.Fatalf("%+v %v", wakes, err)
	}
	if result, err := engine.Drive(ctx, flow, 7); err != nil || result.Output != 7 {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestWaitAnyOverHTTP(t *testing.T) {
	f := newFixture(t)
	engine, client := f.engine(t, "run-any")
	ctx := context.Background()
	flow := definition(func(fc *workflow.FlowContext, in int) (workflow.WaitAnyResult[string], error) {
		return fc.WaitAny[string]("race",
			workflow.WaitForSignal("a", in), workflow.WaitForSignal("b", in), workflow.SleepSignal("timeout", time.Hour))
	})
	result, err := engine.Drive(ctx, flow, 1)
	if err != nil || len(result.Pending) != 3 {
		t.Fatalf("%+v %v", result, err)
	}
	if ok, err := engine.FailWait(ctx, "race|a", 1, errors.New("provider down")); !ok || err != nil {
		t.Fatalf("fail %t %v", ok, err)
	}
	if ok, err := client.Submit(ctx, "race|b", "yes"); !ok || err != nil {
		t.Fatalf("submit %t %v", ok, err)
	}
	result, err = engine.Drive(ctx, flow, 1)
	if err != nil || result.Output.Key != "race|b" || result.Output.Response != "yes" || len(result.Output.Failures) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	detail, _ := client.Detail(ctx)
	status := map[string]workflow.Status{}
	for _, r := range detail.Records {
		status[r.Key] = r.Status
	}
	if status["race|timeout"] != workflow.Cancelled || status["race|a"] != workflow.Failed {
		t.Fatalf("%+v", status)
	}
	if ok, err := client.Submit(ctx, "race|a", "late"); ok || err != nil {
		t.Fatalf("late member %t %v", ok, err)
	}
}

func TestIdleWakePollDoesNotNotify(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	flow := definition(func(fc *workflow.FlowContext, in string) (string, error) {
		return fc.WaitFor[string]("approval", in)
	})
	engine, client := f.engine(t, "idle")
	if _, err := engine.Drive(ctx, flow, "x"); err != nil {
		t.Fatal(err)
	}
	updates, unsubscribe := f.store.hub.subscribe()
	defer unsubscribe()
	for range 3 {
		if wakes, err := client.WaitWake(ctx, 10*time.Millisecond); err != nil || len(wakes) != 0 {
			t.Fatalf("%+v %v", wakes, err)
		}
	}
	select {
	case run := <-updates:
		t.Fatalf("read-only wake polls must not publish, got %q", run)
	default:
	}
	if _, err := client.Submit(ctx, "approval", "ok"); err != nil {
		t.Fatal(err)
	}
	select {
	case run := <-updates:
		if run != "idle" {
			t.Fatalf("unexpected run %q", run)
		}
	default:
		t.Fatal("a write must publish")
	}
}

func TestRunsAreIsolated(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	flow := definition(func(fc *workflow.FlowContext, in string) (string, error) {
		return fc.WaitFor[string]("approval", in)
	})
	a, clientA := f.engine(t, "iso-a")
	b, clientB := f.engine(t, "iso-b")
	if _, err := a.Drive(ctx, flow, "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Drive(ctx, flow, "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := clientA.Submit(ctx, "approval", "for-a"); err != nil {
		t.Fatal(err)
	}
	if wakes, _ := clientB.WaitWake(ctx, 10*time.Millisecond); len(wakes) != 0 {
		t.Fatalf("run b must not see run a wakes: %+v", wakes)
	}
	ra, _ := a.Drive(ctx, flow, "A")
	rb, _ := b.Drive(ctx, flow, "B")
	if ra.Output != "for-a" || rb.Status != workflow.Suspended {
		t.Fatalf("%+v %+v", ra, rb)
	}
	runs, err := f.store.ListRuns(ctx)
	if err != nil || len(runs) != 2 {
		t.Fatalf("%+v %v", runs, err)
	}
}

func TestConcurrentEnginesExecuteActionOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var calls atomic.Int32
	release := make(chan struct{})
	flow := definition(func(fc *workflow.FlowContext, in int) (int, error) {
		return fc.Run("exclusive", func(context.Context, int) (int, error) {
			calls.Add(1)
			<-release
			return in + 1, nil
		}, in)
	})
	const workers = 4
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		engine, _ := f.engine(t, "shared")
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := engine.Drive(ctx, flow, 1)
			errs <- err
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	conflicts := 0
	for err := range errs {
		if errors.Is(err, workflow.ErrConflict) {
			conflicts++
		} else if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if calls.Load() != 1 || conflicts != workers-1 {
		t.Fatalf("calls=%d conflicts=%d", calls.Load(), conflicts)
	}
	engine, _ := f.engine(t, "shared")
	if result, err := engine.Drive(ctx, flow, 1); err != nil || result.Output != 2 || calls.Load() != 1 {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestRunDrivesUntilCompleted(t *testing.T) {
	f := newFixture(t)
	f.store.now = time.Now
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	flow := definition(func(fc *workflow.FlowContext, in string) (string, error) {
		answer, err := fc.WaitFor[string]("approval", in)
		if err != nil {
			return "", err
		}
		return fc.Run("finish", workflow.Pure(func(s string) string { return s + "!" }), answer)
	})
	done := make(chan workflow.Result[string], 1)
	go func() {
		result, err := Run(ctx, flow, "hi", Options{Server: f.server.URL, RunID: "auto", Logf: func(string, ...any) {}})
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	client, _ := NewClient(f.server.URL, "auto")
	var result workflow.Result[string]
submit:
	for {
		select {
		case result = <-done:
			t.Fatalf("Run returned before submit: %+v", result)
		default:
		}
		if ok, err := client.Submit(ctx, "approval", "approved"); ok && err == nil {
			break submit
		}
		time.Sleep(20 * time.Millisecond)
	}
	result = <-done
	if result.Output != "approved!" {
		t.Fatalf("%+v", result)
	}
	detail, _ := client.Detail(ctx)
	if detail.Run.Status != string(workflow.Completed) || detail.Run.Drives < 2 || string(detail.Run.Output) != `"approved!"` {
		t.Fatalf("%+v", detail.Run)
	}
	events, err := f.store.Events(ctx, "auto", 0, 0)
	if err != nil || len(events) == 0 {
		t.Fatalf("%d %v", len(events), err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		var payload struct{ Type string }
		_ = json.Unmarshal(e.Event, &payload)
		seen[e.Source+":"+payload.Type] = true
	}
	for _, want := range []string{"engine:drive_started", "engine:wait_suspended", "store:debugger.submit", "engine:action_finished"} {
		if !seen[want] {
			t.Fatalf("missing %s in %v", want, seen)
		}
	}
}

type shipment struct {
	Attempt int  `json:"attempt"`
	Done_   bool `json:"done"`
}

func (s shipment) Done() bool { return s.Done_ }

// TestEndToEndTwoIsolatedRuns mirrors examples/debugger: two workers attached
// to one server, advanced only through the HTTP operations the UI uses.
func TestEndToEndTwoIsolatedRuns(t *testing.T) {
	f := newFixture(t)
	f.store.now = time.Now
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var polls sync.Map
	flow := definition(func(fc *workflow.FlowContext, id string) (string, error) {
		approval, err := fc.WaitFor[map[string]any]("approval", map[string]string{"order": id})
		if err != nil {
			return "", err
		}
		if approval["approved"] != true {
			return "", workflow.Permanent(errors.New("rejected"))
		}
		notice := fc.GoFlow("notify", workflow.FlowFunc[string, string](func(fc *workflow.FlowContext, id string) (string, error) {
			winner, err := fc.WaitAny[string]("channel",
				workflow.WaitForSignal("email_ack", id), workflow.WaitForSignal("sms_ack", id),
				workflow.SleepSignal("give_up", time.Hour))
			return winner.Key + "=" + winner.Response, err
		}), id)
		ship, err := fc.Loop("ship", func(context.Context, string) (shipment, error) {
			value, _ := polls.LoadOrStore(id, new(atomic.Int32))
			n := int(value.(*atomic.Int32).Add(1))
			return shipment{Attempt: n, Done_: n == 3}, nil
		}, id, workflow.LoopOptions{Interval: time.Hour, MaxAttempts: 5})
		if err != nil {
			return "", err
		}
		text, err := notice.Wait()
		if err != nil {
			return "", err
		}
		if err = fc.Sleep("cooldown", time.Hour); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s shipped@%d %s", id, ship.Attempt, text), nil
	})
	type outcome struct {
		result workflow.Result[string]
		err    error
	}
	start := func(id string) chan outcome {
		done := make(chan outcome, 1)
		go func() {
			result, err := Run(ctx, flow, id, Options{Server: f.server.URL, RunID: id, RetryDelay: 50 * time.Millisecond, Logf: func(string, ...any) {}})
			done <- outcome{result, err}
		}()
		return done
	}
	doneA, doneB := start("order-a"), start("order-b")
	a, _ := NewClient(f.server.URL, "order-a")
	b, _ := NewClient(f.server.URL, "order-b")

	// eventually retries an operation until the worker has registered the wait.
	eventually := func(what string, op func() error) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := op()
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %v", what, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	submit := func(c *Client, key string, value any) func() error {
		return func() error {
			ok, err := c.Submit(ctx, key, value)
			if err == nil && !ok {
				err = errors.New("not accepted")
			}
			return err
		}
	}
	fire := func(c *Client, key string) func() error { return func() error { return c.FireSleep(ctx, key) } }

	eventually("approve a", submit(a, "approval", map[string]any{"approved": true, "by": "alice"}))
	eventually("reject b", submit(b, "approval", map[string]any{"approved": false}))
	eventually("fire ship 1", fire(a, "ship|sleep_1"))
	eventually("fire ship 2", fire(a, "ship|sleep_2"))
	eventually("sms ack", submit(a, "notify|channel|sms_ack", "read"))
	if ok, err := a.Submit(ctx, "notify|channel|email_ack", "late"); ok || err != nil {
		t.Fatalf("late loser must be ignored: %t %v", ok, err)
	}
	eventually("fire cooldown", fire(a, "cooldown"))

	ra, rb := <-doneA, <-doneB
	if ra.err != nil || ra.result.Output != "order-a shipped@3 notify|channel|sms_ack=read" {
		t.Fatalf("a: %+v %v", ra.result, ra.err)
	}
	var permanent *workflow.PermanentError
	if !errors.As(rb.err, &permanent) || rb.result.Status != workflow.Failed {
		t.Fatalf("b: %+v %v", rb.result, rb.err)
	}
	if value, _ := polls.Load("order-a"); value.(*atomic.Int32).Load() != 3 {
		t.Fatalf("loop attempts executed %d times", value.(*atomic.Int32).Load())
	}
	if _, polledB := polls.Load("order-b"); polledB {
		t.Fatal("rejected run must not poll")
	}
	detailA, _ := a.Detail(ctx)
	detailB, _ := b.Detail(ctx)
	status := map[string]workflow.Status{}
	for _, r := range detailA.Records {
		status[r.Key] = r.Status
	}
	if detailA.Run.Status != string(workflow.Completed) || status["notify|channel|email_ack"] != workflow.Cancelled ||
		status["notify|channel|give_up"] != workflow.Cancelled || status["ship|attempt_2"] != workflow.Completed {
		t.Fatalf("a: %s %+v", detailA.Run.Status, status)
	}
	if detailB.Run.Status != string(workflow.Failed) || detailB.Run.Error == "" || len(detailB.Records) != 2 {
		t.Fatalf("b: %+v records=%d", detailB.Run, len(detailB.Records))
	}
}

func TestUIAndErrors(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/", "/app.js", "/app.css"} {
		response, err := http.Get(f.server.URL + path)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %v %v", path, response, err)
		}
		response.Body.Close()
	}
	client, _ := NewClient(f.server.URL, "errs")
	ctx := context.Background()
	if _, err := client.Submit(ctx, "missing", 1); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("not found: %v", err)
	}
	if _, err := NewClient(f.server.URL, "bad id/x"); !errors.Is(err, workflow.ErrInvalid) {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, "k", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Claim(ctx, "k", time.Minute); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("held lease: %v", err)
	}
	if err = client.Release(ctx, *lease); err != nil {
		t.Fatal(err)
	}
	if err = client.Release(ctx, *lease); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("stale release: %v", err)
	}
	next, err := client.Claim(ctx, "k", time.Minute)
	if err != nil || next.Generation != lease.Generation+1 {
		t.Fatalf("%+v %v", next, err)
	}
	if _, err = client.Commit(ctx, *lease, &workflow.Record{Key: "k", Kind: workflow.ActionKind, Status: workflow.Running}, 0); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("fenced commit: %v", err)
	}
}
