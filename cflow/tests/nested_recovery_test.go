package tests

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

// These tests exercise the nesting introduced by FlowContext.RunFlow/GoFlow:
// root -> parallel group Flows -> parallel leaves -> synchronous wait Flow.
// Barriers force callback/Drive/Action interleavings without timing sleeps.
type recoveryBranch struct {
	ID string
}

func (b recoveryBranch) StepKey() string { return b.ID }

type recoveryCalls struct{ counts sync.Map }

func (c *recoveryCalls) action(ctx context.Context, value string) (string, error) {
	count, _ := c.counts.LoadOrStore(ActionKey(ctx), new(atomic.Int32))
	count.(*atomic.Int32).Add(1)
	return value, nil
}

func (c *recoveryCalls) check(t *testing.T, key string, want int32) {
	t.Helper()
	got := int32(0)
	if count, ok := c.counts.Load(key); ok {
		got = count.(*atomic.Int32).Load()
	}
	if got != want {
		t.Fatalf("%s executed %d times, want %d", key, got, want)
	}
}

func nestedRecoveryDefinition(calls *recoveryCalls) Workflow[int, []string] {
	wait := FlowFunc[string, string](func(f *FlowContext, request string) (string, error) {
		return f.WaitFor[string]("reply", request), nil
	})
	leaf := FlowFunc[recoveryBranch, string](func(f *FlowContext, branch recoveryBranch) (string, error) {
		request, err := f.Run("before", AsAction(calls.action), branch.ID)
		if err != nil {
			return "", err
		}
		answer, err := f.RunFlow("wait", wait, request)
		if err != nil {
			return "", err
		}
		return f.Run("after", AsAction(calls.action), answer)
	})
	group := FlowFunc[recoveryBranch, []string](func(f *FlowContext, _ recoveryBranch) ([]string, error) {
		a := f.GoFlow("leaf", leaf, recoveryBranch{"a"})
		b := f.GoFlow("leaf", leaf, recoveryBranch{"b"})
		// Wait in the reverse order of declaration. Completion order must not
		// select another branch's response or consume a Future's value.
		bv, err := b.Wait()
		if err != nil {
			return nil, err
		}
		av, err := a.Wait()
		return []string{av, bv}, err
	})
	return NewWorkflow("nested_recovery", "1", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
		x := f.GoFlow("group", group, recoveryBranch{"x"})
		y := f.GoFlow("group", group, recoveryBranch{"y"})
		xv, err := x.Wait()
		if err != nil {
			return nil, err
		}
		yv, err := y.Wait()
		return append(xv, yv...), err
	}))
}

func checkRecoveryPending(t *testing.T, result Result[[]string], keys ...string) {
	t.Helper()
	got := make([]string, len(result.Pending))
	for i, pending := range result.Pending {
		got[i] = pending.Key
		if pending.Kind != SignalKind || DefaultKey(pending.Path) != pending.Key {
			t.Fatalf("invalid pending address: %+v", pending)
		}
	}
	if result.Status != Suspended || !reflect.DeepEqual(got, keys) {
		t.Fatalf("result=%+v, want suspended keys %v", result, keys)
	}
}

func signalRecovery(t *testing.T, e *Engine, ctx context.Context, key, answer string) {
	t.Helper()
	accepted, err := e.Signal(ctx, key, answer)
	if err != nil || !accepted {
		t.Fatalf("Signal(%s)=%t, %v", key, accepted, err)
	}
}

func TestNestedFlowRecovery(t *testing.T) {
	t.Run("synchronous siblings register waits sequentially", func(t *testing.T) {
		ctx := context.Background()
		store := memory.New()
		engine := testEngine(t, store, Options{})
		wait := FlowFunc[int, string](func(f *FlowContext, n int) (string, error) {
			return f.WaitFor[string]("reply", n), nil
		})
		root := NewWorkflow("sequential", "1", FlowFunc[int, string](func(f *FlowContext, n int) (string, error) {
			a, err := f.RunFlow("a", wait, n)
			if err != nil {
				return "", err
			}
			b, err := f.RunFlow("b", wait, n)
			return a + b, err
		}))
		first, err := engine.Drive(ctx, root, 0)
		if err != nil || first.Status != Suspended || len(first.Pending) != 1 || first.Pending[0].Key != "a|reply" {
			t.Fatalf("%+v %v", first, err)
		}
		if record, err := store.Get(ctx, "b|reply"); err != nil || record != nil {
			t.Fatalf("second synchronous Flow registered too early: %+v %v", record, err)
		}
		signalRecovery(t, engine, ctx, "a|reply", "A")
		second, err := engine.Drive(ctx, root, 0)
		if err != nil || second.Status != Suspended || len(second.Pending) != 1 || second.Pending[0].Key != "b|reply" {
			t.Fatalf("%+v %v", second, err)
		}
		signalRecovery(t, engine, ctx, "b|reply", "B")
		last, err := engine.Drive(ctx, root, 0)
		if err != nil || last.Status != Completed || last.Output != "AB" {
			t.Fatalf("%+v %v", last, err)
		}
	})

	t.Run("partial then concurrent callbacks preserve nested branch identity", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		store := memory.New()
		calls := &recoveryCalls{}
		root := nestedRecoveryDefinition(calls)
		engine := testEngine(t, store, Options{})
		keys := []string{
			"group[x]|leaf[a]|wait|reply", "group[x]|leaf[b]|wait|reply",
			"group[y]|leaf[a]|wait|reply", "group[y]|leaf[b]|wait|reply",
		}
		first, err := engine.Drive(ctx, root, 0)
		if err != nil {
			t.Fatal(err)
		}
		checkRecoveryPending(t, first, keys...)
		for _, key := range keys {
			prefix := key[:len(key)-len("wait|reply")]
			calls.check(t, prefix+"before", 1)
			calls.check(t, prefix+"after", 0)
		}
		// x/a finishes while x/b (waited first) is still suspended.
		signalRecovery(t, engine, ctx, keys[0], "XA")
		partial, err := testEngine(t, store, Options{}).Drive(ctx, root, 0)
		if err != nil {
			t.Fatal(err)
		}
		checkRecoveryPending(t, partial, keys[1:]...)
		calls.check(t, "group[x]|leaf[a]|after", 1)
		calls.check(t, "group[x]|leaf[b]|after", 0)

		var callbacks sync.WaitGroup
		var accepted atomic.Int32
		start := make(chan struct{})
		for i, key := range keys[1:] {
			for range 4 {
				callbacks.Add(1)
				go func() {
					defer callbacks.Done()
					<-start
					ok, err := engine.Signal(ctx, key, []string{"XB", "YA", "YB"}[i])
					if err != nil {
						t.Errorf("callback: %v", err)
					}
					if ok {
						accepted.Add(1)
					}
				}()
			}
		}
		close(start)
		callbacks.Wait()
		if accepted.Load() != 3 {
			t.Fatalf("accepted callbacks=%d, want one per remaining leaf", accepted.Load())
		}
		result, err := testEngine(t, store, Options{}).Drive(ctx, root, 0)
		want := []string{"XA", "XB", "YA", "YB"}
		if err != nil || result.Status != Completed || !reflect.DeepEqual(result.Output, want) {
			t.Fatalf("%+v %v", result, err)
		}
		// Independent engines may replay the finished run simultaneously.
		var drives sync.WaitGroup
		for range 8 {
			e := testEngine(t, store, Options{})
			drives.Add(1)
			go func() {
				defer drives.Done()
				result, err := e.Drive(ctx, root, 0)
				if err != nil || result.Status != Completed || !reflect.DeepEqual(result.Output, want) {
					t.Errorf("replay: %+v %v", result, err)
				}
			}()
		}
		drives.Wait()
		for _, key := range keys {
			prefix := key[:len(key)-len("wait|reply")]
			calls.check(t, prefix+"before", 1)
			calls.check(t, prefix+"after", 1)
		}
		if leases, _, _, _ := store.Stats(); leases != 0 {
			t.Fatal("lease leaked")
		}
	})

	t.Run("nested WaitAny winners and cancellations stay local", func(t *testing.T) {
		ctx := context.Background()
		var clock atomic.Int64
		clock.Store(time.Now().UnixNano())
		now := func() time.Time { return time.Unix(0, clock.Load()) }
		store := memory.New()
		store.SetClock(now)
		engine := testEngine(t, store, Options{})
		decision := FlowFunc[int, string](func(f *FlowContext, n int) (string, error) {
			reply := OnSignal[string]("reply", n)
			timer := OnSleep("timeout", time.Hour)
			if f.WaitAny("race", reply, timer) == reply {
				return reply.Value(), nil
			}
			return "timeout", nil
		})
		agent := FlowFunc[recoveryBranch, string](func(f *FlowContext, _ recoveryBranch) (string, error) {
			return f.RunFlow("decision", decision, 0)
		})
		root := NewWorkflow("races", "1", FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			left := f.GoFlow("agent", agent, recoveryBranch{"left"})
			right := f.GoFlow("agent", agent, recoveryBranch{"right"})
			lv, err := left.Wait()
			if err != nil {
				return "", err
			}
			rv, err := right.Wait()
			return lv + "/" + rv, err
		}))
		first, err := engine.Drive(ctx, root, 0)
		if err != nil || first.Status != Suspended || len(first.Pending) != 4 {
			t.Fatalf("%+v %v", first, err)
		}
		left := "agent[left]|decision|race|"
		right := "agent[right]|decision|race|"
		signalRecovery(t, engine, ctx, left+"reply", "approved")
		partial, err := engine.Drive(ctx, root, 0)
		if err != nil || partial.Status != Suspended || len(partial.Pending) != 2 {
			t.Fatalf("%+v %v", partial, err)
		}
		for _, pending := range partial.Pending {
			if pending.Key != right+"reply" && pending.Key != right+"timeout" {
				t.Fatalf("wrong group pending: %+v", pending)
			}
		}
		clock.Add(int64(time.Hour))
		last, err := engine.Drive(ctx, root, 0)
		if err != nil || last.Status != Completed || last.Output != "approved/timeout" {
			t.Fatalf("%+v %v", last, err)
		}
		if ok, err := engine.Signal(ctx, right+"reply", "late"); ok || err != nil {
			t.Fatalf("late callback=%t %v", ok, err)
		}
		for _, key := range []string{left + "timeout", right + "reply"} {
			record, err := store.Get(ctx, key)
			if err != nil || record == nil || record.Status != Cancelled {
				t.Fatalf("%s: %+v %v", key, record, err)
			}
		}
	})

	t.Run("signal during suspension keeps a durable wake", func(t *testing.T) {
		ctx := context.Background()
		store := memory.New()
		engine := testEngine(t, store, Options{})
		var resolveErr error
		var once sync.Once
		engine.Subscribe(func(_ context.Context, event Event) {
			once.Do(func() {
				_, resolveErr = engine.Signal(ctx, event.Key, "arrived")
			})
		}, WaitSuspended)
		leaf := FlowFunc[int, string](func(f *FlowContext, n int) (string, error) {
			return f.WaitFor[string]("reply", n), nil
		})
		middle := FlowFunc[int, string](func(f *FlowContext, n int) (string, error) {
			return f.RunFlow("leaf", leaf, n)
		})
		root := NewWorkflow("late_snapshot", "1", FlowFunc[int, string](func(f *FlowContext, n int) (string, error) {
			return f.GoFlow("middle", middle, n).Wait()
		}))
		first, err := engine.Drive(ctx, root, 0)
		if err != nil || resolveErr != nil || first.Status != Suspended || len(first.Pending) != 1 {
			t.Fatalf("%+v drive=%v signal=%v", first, err, resolveErr)
		}
		record, _ := store.Get(ctx, "middle|leaf|reply")
		if record == nil || record.Status != Completed {
			t.Fatalf("callback lost: %+v", record)
		}
		messages, err := store.Pending(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		wakes := 0
		for _, message := range messages {
			if message.Key == record.Key && message.Purpose == memory.WakeNotification {
				wakes++
			}
		}
		if wakes != 1 {
			t.Fatalf("wake count=%d", wakes)
		}
		last, err := testEngine(t, store, Options{}).Drive(ctx, root, 0)
		if err != nil || last.Status != Completed || last.Output != "arrived" {
			t.Fatalf("%+v %v", last, err)
		}
	})

	t.Run("overlapping resume conflicts without repeating an active Action", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		store := memory.New()
		engine := testEngine(t, store, Options{})
		started, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		action := AsAction(func(ctx context.Context, answer string) (string, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			select {
			case <-release:
				return answer + ":done", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		leaf := FlowFunc[int, string](func(f *FlowContext, n int) (string, error) {
			return f.Run("after", action, f.WaitFor[string]("reply", n))
		})
		middle := FlowFunc[int, string](func(f *FlowContext, n int) (string, error) {
			return f.RunFlow("leaf", leaf, n)
		})
		root := NewWorkflow("overlap", "1", FlowFunc[int, string](func(f *FlowContext, n int) (string, error) {
			return f.GoFlow("middle", middle, n).Wait()
		}))
		first, err := engine.Drive(ctx, root, 0)
		if err != nil || first.Status != Suspended {
			t.Fatalf("%+v %v", first, err)
		}
		signalRecovery(t, engine, ctx, "middle|leaf|reply", "accepted")
		type outcome struct {
			result Result[string]
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := engine.Drive(ctx, root, 0)
			done <- outcome{result, err}
		}()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("first resumed Action did not start")
		}
		// A different engine loses this Action's lease, not the whole run's
		// lease. Its cancellation must not cancel the first engine's Action.
		competing, err := testEngine(t, store, Options{}).Drive(ctx, root, 0)
		if competing.Status != Running || !errors.Is(err, ErrConflict) || calls.Load() != 1 {
			t.Fatalf("competing=%+v err=%v calls=%d", competing, err, calls.Load())
		}
		close(release)
		winner := <-done
		if winner.err != nil || winner.result.Status != Completed || winner.result.Output != "accepted:done" {
			t.Fatalf("winner=%+v %v", winner.result, winner.err)
		}
		last, err := testEngine(t, store, Options{}).Drive(ctx, root, 0)
		if err != nil || last.Status != Completed || last.Output != "accepted:done" || calls.Load() != 1 {
			t.Fatalf("retry=%+v %v calls=%d", last, err, calls.Load())
		}
		if leases, _, _, _ := store.Stats(); leases != 0 {
			t.Fatal("lease leaked after overlapping recovery")
		}
	})

	t.Run("nested suspension joins unwaited descendant Actions", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		store := memory.New()
		engine := testEngine(t, store, Options{})
		started, suspended, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		engine.Subscribe(func(context.Context, Event) { once.Do(func() { close(suspended) }) }, WaitSuspended)
		leaf := FlowFunc[int, int](func(f *FlowContext, n int) (int, error) {
			f.Go("io", AsAction(func(ctx context.Context, n int) (int, error) {
				close(started)
				defer close(stopped)
				select {
				case <-release:
					return n, nil
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			}), n)
			<-started
			return f.WaitFor[int]("reply", n), nil
		})
		middle := FlowFunc[int, int](func(f *FlowContext, n int) (int, error) {
			// Even without Wait(), this child belongs to middle's scope.
			f.GoFlow("leaf", leaf, n)
			return n, nil
		})
		root := NewWorkflow("join", "1", FlowFunc[int, int](func(f *FlowContext, n int) (int, error) {
			return f.RunFlow("middle", middle, n)
		}))
		done := make(chan error, 1)
		go func() {
			result, err := engine.Drive(ctx, root, 7)
			select {
			case <-stopped:
			default:
				err = fmt.Errorf("Drive returned before descendant Action stopped: %+v", result)
			}
			if err == nil && (result.Status != Suspended || len(result.Pending) != 1 || result.Pending[0].Key != "middle|leaf|reply") {
				err = fmt.Errorf("unexpected result %+v", result)
			}
			done <- err
		}()
		select {
		case <-suspended:
		case <-ctx.Done():
			t.Fatal("nested wait was not registered")
		}
		select {
		case err := <-done:
			t.Fatalf("Drive returned before release: %v", err)
		default:
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if leases, _, _, _ := store.Stats(); leases != 0 {
			t.Fatal("descendant lease leaked")
		}
	})

	t.Run("nested framework failure cancels descendants and keeps waits", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		store := memory.New()
		engine := testEngine(t, store, Options{})
		started, waiting, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		engine.Subscribe(func(context.Context, Event) { once.Do(func() { close(waiting) }) }, WaitSuspended)
		long := FlowFunc[int, int](func(f *FlowContext, n int) (int, error) {
			return f.Run("io", AsAction(func(ctx context.Context, _ int) (int, error) {
				close(started)
				defer close(stopped)
				<-ctx.Done()
				return 0, ctx.Err()
			}), n)
		})
		wait := FlowFunc[int, int](func(f *FlowContext, n int) (int, error) {
			return f.WaitFor[int]("reply", n), nil
		})
		cause := errors.New("transient provider failure")
		middle := FlowFunc[int, int](func(f *FlowContext, n int) (int, error) {
			f.GoFlow("long", long, n)
			f.GoFlow("waiting", wait, n)
			<-started
			<-waiting
			return f.Run("fail", AsAction(func(context.Context, int) (int, error) { return 0, cause }), n)
		})
		root := NewWorkflow("failure", "1", FlowFunc[int, int](func(f *FlowContext, n int) (int, error) {
			return f.GoFlow("middle", middle, n).Wait()
		}))
		result, err := engine.Drive(ctx, root, 0)
		if result.Status != Running || !errors.Is(err, cause) {
			t.Fatalf("%+v %v", result, err)
		}
		select {
		case <-stopped:
		default:
			t.Fatal("cancelled descendant IO was not joined")
		}
		record, err := store.Get(ctx, "middle|waiting|reply")
		if err != nil || record == nil || record.Status != Suspended {
			t.Fatalf("wait lost after sibling failure: %+v %v", record, err)
		}
		if leases, _, _, _ := store.Stats(); leases != 0 {
			t.Fatal("lease leaked after nested cancellation")
		}
	})
}
