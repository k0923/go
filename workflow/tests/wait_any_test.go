package tests

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	. "github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

// waitOutcome flattens a WaitAny result for assertions.
// waitOutcome 将 WaitAny 结果展平，便于断言。
type waitOutcome struct {
	Key      string
	Kind     Kind
	Response string
	Failures int
}

func waitAnyOutcome(f *FlowContext, key string, cases ...Case) (waitOutcome, error) {
	winner := f.WaitAny(key, cases...)
	var outcome waitOutcome
	for _, candidate := range cases {
		if candidate.Failure() != "" {
			outcome.Failures++
		}
	}
	outcome.Key, outcome.Kind = winner.Key(), winner.Kind()
	if signal, ok := winner.(*SignalCase[string]); ok {
		outcome.Response = signal.Value()
	}
	return outcome, nil
}

func anyWaitFlow() Workflow[int, waitOutcome] {
	return definition(FlowFunc[int, waitOutcome](func(f *FlowContext, input int) (waitOutcome, error) {
		return waitAnyOutcome(f, "race", OnSignal[string]("a", input), OnSignal[string]("b", input))
	}))
}

func TestFlowContextWaitAny(t *testing.T) {
	t.Run("all registered winner cancels losers and continues", func(t *testing.T) {
		ctx := context.Background()
		store := memory.New()
		e := testEngine(t, store, Options{})
		calls := 0
		flow := definition(FlowFunc[int, string](func(f *FlowContext, input int) (string, error) {
			a, b := OnSignal[string]("a", input), OnSignal[string]("b", input)
			winner := f.WaitAny("race", a, b)
			var answer string
			switch winner {
			case a:
				answer = a.Value()
			case b:
				answer = b.Value()
			}
			return f.Run("after", AsAction(func(context.Context, int) (string, error) { calls++; return winner.Key() + ":" + answer, nil }), input)
		}))
		for i := 0; i < 2; i++ {
			result, err := e.Drive(ctx, flow, 1)
			if err != nil || result.Status != Suspended || len(result.Pending) != 2 {
				t.Fatalf("%+v %v", result, err)
			}
		}
		messages, _ := store.Pending(ctx, time.Now())
		if len(messages) != 2 {
			t.Fatal(messages)
		}
		a, _ := store.Get(ctx, "race|a")
		b, _ := store.Get(ctx, "race|b")
		if a.Status != Suspended || b.Status != Suspended || calls != 0 {
			t.Fatal("waits not concurrently active")
		}
		if ok, err := e.ResolveSignal(ctx, b.Key, b.Version, "answer"); !ok || err != nil {
			t.Fatalf("%t %v", ok, err)
		}
		// Cancellation occurs at Resolve time, BEFORE any resume.
		// 取消发生在 Resolve 时刻，早于任何恢复。
		a, _ = store.Get(ctx, a.Key)
		if a.Status != Cancelled {
			t.Fatal("loser was not cancelled atomically", a)
		}
		messages, _ = store.Pending(ctx, time.Now())
		counts := map[string]int{}
		for _, message := range messages {
			counts[message.Purpose]++
		}
		if len(messages) != 2 || counts[memory.WakeNotification] != 1 || counts[memory.CancelNotification] != 1 || counts[memory.RequestNotification] != 0 {
			t.Fatal("missing wake/cancel or residual requests", messages)
		}
		if ok, err := e.ResolveSignal(ctx, a.Key, 1, "late"); ok || err != nil {
			t.Fatalf("late callback: %t %v", ok, err)
		}
		for i := 0; i < 2; i++ {
			result, err := testEngine(t, store, Options{}).Drive(ctx, flow, 1)
			if err != nil || result.Status != Completed || result.Output != "race|b:answer" {
				t.Fatalf("%+v %v", result, err)
			}
		}
		if calls != 1 {
			t.Fatal("downstream repeated", calls)
		}
	})
	t.Run("simultaneous callbacks accept one winner", func(t *testing.T) {
		ctx := context.Background()
		store := memory.New()
		e := testEngine(t, store, Options{})
		if _, err := e.Drive(ctx, anyWaitFlow(), 1); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		accepted := make(chan bool, 2)
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, key := range []string{"race|a", "race|b"} {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				<-start
				ok, err := e.ResolveSignal(ctx, key, 1, key)
				accepted <- ok
				errs <- err
			}(key)
		}
		close(start)
		wg.Wait()
		close(accepted)
		close(errs)
		count := 0
		for ok := range accepted {
			if ok {
				count++
			}
		}
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if count != 1 {
			t.Fatalf("winners=%d", count)
		}
		first, err := e.Drive(ctx, anyWaitFlow(), 1)
		if err != nil || first.Status != Completed || first.Output.Key != first.Output.Response {
			t.Fatalf("%+v %v", first, err)
		}
		for i := 0; i < 5; i++ {
			next, err := e.Drive(ctx, anyWaitFlow(), 1)
			if err != nil || next.Output.Key != first.Output.Key || next.Output.Response != first.Output.Response {
				t.Fatal("winner changed")
			}
		}
	})
	t.Run("candidate failure leaves healthy wait active", func(t *testing.T) {
		ctx := context.Background()
		store := memory.New()
		e := testEngine(t, store, Options{})
		_, _ = e.Drive(ctx, anyWaitFlow(), 1)
		if ok, err := e.FailWait(ctx, "race|a", 1, errors.New("provider unavailable")); !ok || err != nil {
			t.Fatalf("%t %v", ok, err)
		}
		result, err := e.Drive(ctx, anyWaitFlow(), 1)
		if err != nil || result.Status != Suspended || len(result.Pending) != 1 || result.Pending[0].Key != "race|b" {
			t.Fatalf("%+v %v", result, err)
		}
		if ok, err := e.ResolveSignal(ctx, "race|a", 1, "late"); ok || err != nil {
			t.Fatal("failed wait resurrected")
		}
		_, err = e.ResolveSignal(ctx, "race|b", 1, "healthy")
		if err != nil {
			t.Fatal(err)
		}
		result, err = e.Drive(ctx, anyWaitFlow(), 1)
		if err != nil || result.Output.Response != "healthy" || result.Output.Failures != 1 {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("all failed wakes and fails the drive terminally", func(t *testing.T) {
		ctx := context.Background()
		store := memory.New()
		e := testEngine(t, store, Options{})
		after := 0
		// The Flow cannot observe the group failure: like WaitFor it is a signal,
		// so nothing after WaitAny runs and the drive itself fails.
		// Flow 无法观察到组失败：与 WaitFor 一样它只是信号，WaitAny 之后的代码不会执行，
		// 由 drive 本身失败。
		flow := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, input int) (waitOutcome, error) {
			outcome, err := waitAnyOutcome(f, "race", OnSignal[string]("a", input), OnSignal[string]("b", input))
			after++
			return outcome, err
		}))
		_, _ = e.Drive(ctx, flow, 1)
		for _, key := range []string{"race|a", "race|b"} {
			if _, err := e.FailWait(ctx, key, 1, errors.New("failed")); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 2; i++ {
			result, err := e.Drive(ctx, flow, 1)
			if result.Status != Failed || !errors.Is(err, ErrWaitFailed) || after != 0 {
				t.Fatalf("%+v %v after=%d", result, err, after)
			}
		}
		messages, _ := store.Pending(ctx, time.Now())
		if len(messages) != 1 || messages[0].Purpose != memory.WakeNotification {
			t.Fatal(messages)
		}
		if ok, err := e.FailWait(ctx, "race|a", 1, errors.New("again")); ok || err != nil {
			t.Fatalf("%t %v", ok, err)
		}
	})
	t.Run("request encoding failure and panic isolated", func(t *testing.T) {
		for _, bad := range []any{make(chan int), panicWaitRequest{}} {
			e := testEngine(t, memory.New(), Options{})
			flow := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, in int) (waitOutcome, error) {
				return waitAnyOutcome(f, "race", OnSignal[string]("bad", bad), OnSignal[string]("good", in))
			}))
			result, err := e.Drive(context.Background(), flow, 1)
			if err != nil || result.Status != Suspended || len(result.Pending) != 1 || result.Pending[0].Key != "race|good" {
				t.Fatalf("%+v %v", result, err)
			}
			if _, err := e.ResolveSignal(context.Background(), "race|good", 1, "done"); err != nil {
				t.Fatal(err)
			}
			result, err = e.Drive(context.Background(), flow, 1)
			if err != nil || result.Output.Response != "done" || result.Output.Failures != 1 {
				t.Fatalf("%+v %v", result, err)
			}
		}
	})
	t.Run("invalid and duplicate keys register nothing", func(t *testing.T) {
		for _, keys := range [][]string{nil, {"a", "a"}, {"a", "b|x"}} {
			store := memory.New()
			e := testEngine(t, store, Options{})
			flow := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, in int) (waitOutcome, error) {
				var cases []Case
				for _, key := range keys {
					cases = append(cases, OnSignal[string](key, in))
				}
				return waitAnyOutcome(f, "race", cases...)
			}))
			_, err := e.Drive(context.Background(), flow, 0)
			if !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
			messages, _ := store.Pending(context.Background(), time.Now())
			if len(messages) != 0 {
				t.Fatal("partially registered", messages)
			}
		}
	})
	t.Run("nil reused and mistyped cases register nothing", func(t *testing.T) {
		shared := OnSignal[string]("shared", 1)
		for name, build := range map[string]func() []Case{
			"nil":        func() []Case { return []Case{OnSignal[string]("a", 1), nil} },
			"same call":  func() []Case { c := OnSignal[string]("a", 1); return []Case{c, c} },
			"sleep kind": func() []Case { return []Case{OnKind[string](SleepKind, "a", 1)} },
		} {
			store := memory.New()
			e := testEngine(t, store, Options{})
			flow := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, _ int) (waitOutcome, error) {
				return waitAnyOutcome(f, "race", build()...)
			}))
			result, err := e.Drive(context.Background(), flow, 0)
			if result.Status != Failed || !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s: %+v %v", name, result, err)
			}
			if messages, _ := store.Pending(context.Background(), time.Now()); len(messages) != 0 {
				t.Fatal(name, "partially registered", messages)
			}
		}
		// A case declared outside the Flow is consumed by the first drive.
		// 在 Flow 外部声明的 Case 会被第一次 drive 消耗。
		e := testEngine(t, memory.New(), Options{})
		flow := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, _ int) (waitOutcome, error) {
			return waitAnyOutcome(f, "race", shared)
		}))
		if result, err := e.Drive(context.Background(), flow, 0); err != nil || result.Status != Suspended {
			t.Fatalf("%+v %v", result, err)
		}
		if _, err := e.Drive(context.Background(), flow, 0); !errors.Is(err, ErrInvalid) {
			t.Fatal("reused case accepted", err)
		}
	})
	t.Run("heterogeneous response types", func(t *testing.T) {
		type approval struct{ By string }
		ctx := context.Background()
		e := testEngine(t, memory.New(), Options{})
		flow := definition(FlowFunc[int, string](func(f *FlowContext, in int) (string, error) {
			approve := OnSignal[approval]("approve", in)
			reject := OnSignal[int]("reject", in)
			timeout := OnSleep("timeout", time.Hour)
			switch f.WaitAny("decision", approve, reject, timeout) {
			case approve:
				return "approved by " + approve.Value().By, nil
			case reject:
				return fmt.Sprintf("rejected code %d", reject.Value()), nil
			case timeout:
				return "timeout", nil
			}
			return "", errors.New("unknown winner")
		}))
		if _, err := e.Drive(ctx, flow, 1); err != nil {
			t.Fatal(err)
		}
		if ok, err := e.Signal(ctx, "decision|reject", 42); !ok || err != nil {
			t.Fatalf("%t %v", ok, err)
		}
		result, err := e.Drive(ctx, flow, 1)
		if err != nil || result.Output != "rejected code 42" {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("optional infrastructure and cancellation", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, struct{ Infrastructure }{store}, Options{})
		if _, err := e.Drive(context.Background(), anyWaitFlow(), 0); !errors.Is(err, ErrWaitAnyUnsupported) {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.WaitAny(ctx, "race", []WaitRequest{{Key: "a", Request: []byte("1")}}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if _, err := store.FailWait(ctx, "a", 1, "failed"); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("registration snapshots and inputs cannot be mutated", func(t *testing.T) {
		ctx := context.Background()
		store := memory.New()
		requests := []WaitRequest{{Key: "race|a", Request: []byte(`"a"`)}, {Key: "race|b", Request: []byte(`"b"`)}}
		state, err := store.WaitAny(ctx, "race", requests)
		if err != nil {
			t.Fatal(err)
		}
		state.Pending[0].Key = "mutated"
		requests[0].Request[1] = 'x'
		original := []WaitRequest{{Key: "race|a", Request: []byte(`"a"`)}, {Key: "race|b", Request: []byte(`"b"`)}}
		state, err = store.WaitAny(ctx, "race", original)
		if err != nil || state.Pending[0].Key != "race|a" {
			t.Fatalf("%+v %v", state, err)
		}
		if _, err := store.WaitAny(ctx, "race", requests); !errors.Is(err, ErrChanged) {
			t.Fatal(err)
		}
		if _, err := store.Resolve(ctx, "race|a", 1, []byte(`invalid`)); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if _, err := store.Resolve(ctx, "race|b", 1, []byte(`"winner"`)); err != nil {
			t.Fatal(err)
		}
		state, err = store.WaitAny(ctx, "race", original)
		if err != nil || state.Status != Completed {
			t.Fatalf("%+v %v", state, err)
		}
		state.Response[1] = 'x'
		fresh, err := store.WaitAny(ctx, "race", original)
		if err != nil || string(fresh.Response) != `"winner"` {
			t.Fatalf("%+v %v", fresh, err)
		}
		// Repeated replay retains only the bounded history for this one group.
		// 重复回放只会保留这个组的有界历史。
		if _, _, groups, members := store.Stats(); groups != 1 || members != 2 {
			t.Fatal("unexpected waiting registrations")
		}
	})
}

type panicWaitRequest struct{}

func (panicWaitRequest) MarshalJSON() ([]byte, error) { panic("candidate marshaler failed") }
