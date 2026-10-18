package tests

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

func testEngine(t *testing.T, store Infrastructure, options Options) *Engine {
	t.Helper()
	e, err := NewEngine(store, options)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func definition[I, O any](fn FlowFunc[I, O]) Workflow[I, O] {
	// The root Flow adds no namespace, so keys carry no prefix.
	// 根 Flow 不追加命名空间，因此 key 不带前缀。
	return NewWorkflow("test", "1", Flow[I, O](fn))
}

func TestNewEngine(t *testing.T) {
	for _, option := range []Options{{LeaseTTL: -1}, {CleanupTimeout: -1}, {LeaseTTL: 1}, {MaxFlowDepth: -1}} {
		if _, err := NewEngine(memory.New(), option); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid options: %v", err)
		}
	}
	if _, err := NewEngine(nil, Options{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestEngineDrive(t *testing.T) {
	t.Run("replay and explicit input", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		calls := 0
		var phasesMu sync.Mutex
		var phases []EventType
		defer e.Subscribe(func(_ context.Context, event Event) {
			phasesMu.Lock()
			defer phasesMu.Unlock()
			phases = append(phases, event.Type)
		})()
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("double", AsAction(func(ctx context.Context, in int) (int, error) {
				calls++
				if ActionKey(ctx) != "double" {
					t.Fatalf("key: %s", ActionKey(ctx))
				}
				if ctx.Value("trace") != "parent" {
					t.Fatal("context value lost")
				}
				return in * 2, nil
			}), in)
		}))
		ctx := context.WithValue(context.Background(), "trace", "parent")
		for i := 0; i < 2; i++ {
			result, err := e.Drive(ctx, flow, 3)
			if err != nil || result.Status != Completed || result.Output != 6 {
				t.Fatalf("%+v %v", result, err)
			}
		}
		if calls != 1 {
			t.Fatalf("calls=%d", calls)
		}
		found := false
		for _, phase := range phases {
			if phase == ActionReplayed {
				found = true
			}
		}
		if !found {
			t.Fatal("replay not observed")
		}
		result, err := e.Drive(ctx, flow, 4)
		if !errors.Is(err, ErrChanged) || result.Status != Failed {
			t.Fatalf("input mismatch: %+v %v", result, err)
		}
		flow.Version = "2"
		result, err = e.Drive(ctx, flow, 3)
		if !errors.Is(err, ErrChanged) || result.Status != Failed {
			t.Fatalf("version mismatch: %+v %v", result, err)
		}
	})
	t.Run("ordinary failure retries", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		transient := errors.New("503")
		calls := 0
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("service", AsAction(func(context.Context, int) (int, error) {
				calls++
				if calls == 1 {
					return 0, transient
				}
				return 9, nil
			}), in)
		}))
		result, err := e.Drive(context.Background(), flow, 1)
		if !errors.Is(err, transient) || result.Status != Running {
			t.Fatalf("%+v %v", result, err)
		}
		r, _ := store.Get(context.Background(), "service")
		if r.Status != Running {
			t.Fatalf("permanently failed: %+v", r)
		}
		result, err = e.Drive(context.Background(), flow, 1)
		if err != nil || result.Output != 9 || calls != 2 {
			t.Fatalf("%+v %v calls=%d", result, err, calls)
		}
	})
	t.Run("permanent and replayed error", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		cause := errors.New("declined")
		calls := 0
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("pay", AsAction(func(context.Context, int) (int, error) { calls++; return 0, Permanent(cause) }), in)
		}))
		result, err := e.Drive(context.Background(), flow, 1)
		var business *ActionError
		if result.Status != Failed || !errors.Is(err, cause) || !errors.As(err, &business) {
			t.Fatalf("%+v %v", result, err)
		}
		result, err = e.Drive(context.Background(), flow, 1)
		if result.Status != Failed || !errors.As(err, &business) || business.Message != "declined" || business.Cause != nil || calls != 1 {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("panic retains stack and can retry", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		broken := true
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("bug", AsAction(func(context.Context, int) (int, error) {
				if broken {
					panic("broken")
				}
				return 7, nil
			}), in)
		}))
		result, err := e.Drive(context.Background(), flow, 0)
		var p *PanicError
		if !errors.As(err, &p) || len(p.Stack) == 0 || result.Status != Running {
			t.Fatalf("%+v %v", result, err)
		}
		broken = false
		result, err = e.Drive(context.Background(), flow, 0)
		if err != nil || result.Output != 7 {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("timeout leaves running and releases", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("slow", AsAction(func(ctx context.Context, _ int) (int, error) {
				<-ctx.Done()
				return 1, nil
			}), in, ActionOptions{Timeout: time.Millisecond})
		}))
		result, err := e.Drive(context.Background(), flow, 0)
		if !errors.Is(err, context.DeadlineExceeded) || result.Status != Running {
			t.Fatalf("%+v %v", result, err)
		}
		r, _ := store.Get(context.Background(), "slow")
		if r.Status != Running {
			t.Fatal(r)
		}
		if _, err := store.Claim(context.Background(), "slow", time.Second); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("invalid definition and cancelled input", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		_, err := e.Drive(context.Background(), Workflow[int, int]{}, 0)
		if !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = e.Drive(ctx, definition(FlowFunc[int, int](func(*FlowContext, int) (int, error) { t.Fatal("executed"); return 0, nil })), 0)
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("recovered suspension never persists progress", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		calls := 0
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			func() {
				defer func() { _ = recover() }()
				f.WaitFor[string]("approval", in)
			}()
			// A recovered halt re-halts on the next primitive: no zero-value side effect.
			// 被 recover 的 halt 会在下一个原语再次停止：不会产生零值副作用。
			f.Run("charge", AsAction(func(context.Context, int) (int, error) { calls++; return 1, nil }), in)
			return 123, nil
		}))
		result, err := e.Drive(context.Background(), flow, 0)
		if err != nil || result.Status != Suspended || result.Output != 0 || len(result.Pending) != 1 || calls != 0 {
			t.Fatalf("%+v %v calls=%d", result, err, calls)
		}
		if r, _ := store.Get(context.Background(), "charge"); r != nil {
			t.Fatalf("persisted after halt: %+v", r)
		}
	})
	t.Run("swallowed halt without suspension fails", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			func() {
				defer func() { _ = recover() }()
				f.Run("bad key", Pure(func(n int) int { return n }), in)
			}()
			return 1, nil
		}))
		result, err := e.Drive(context.Background(), flow, 0)
		if result.Status != Failed || !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("handled business error allows compensation", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		compensated := 0
		flow := definition(FlowFunc[int, string](func(f *FlowContext, in int) (string, error) {
			if _, err := f.Run("pay", AsAction(func(context.Context, int) (int, error) {
				return 0, Permanent(errors.New("declined"))
			}), in); err != nil {
				return f.Run("refund", AsAction(func(context.Context, int) (string, error) {
					compensated++
					return "refunded", nil
				}), in)
			}
			return "paid", nil
		}))
		for i := 0; i < 2; i++ {
			result, err := e.Drive(context.Background(), flow, 0)
			if err != nil || result.Status != Completed || result.Output != "refunded" {
				t.Fatalf("%+v %v", result, err)
			}
		}
		if compensated != 1 {
			t.Fatal(compensated)
		}
	})
	t.Run("returned plain error is terminal", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		cause := errors.New("invalid order")
		flow := definition(FlowFunc[int, int](func(*FlowContext, int) (int, error) { return 0, cause }))
		result, err := e.Drive(context.Background(), flow, 0)
		if result.Status != Failed || !errors.Is(err, cause) {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("orchestration panic is terminal", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		flow := definition(FlowFunc[int, int](func(*FlowContext, int) (int, error) { panic("bug") }))
		result, err := e.Drive(context.Background(), flow, 0)
		var p *PanicError
		if result.Status != Failed || !errors.As(err, &p) || len(p.Stack) == 0 {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("retry policy commits exhausted failure", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		calls := 0
		transient := errors.New("503")
		retry := ActionOptions{Retry: RetryPolicy{MaxAttempts: 3, InitialInterval: time.Microsecond}}
		flow := definition(FlowFunc[int, string](func(f *FlowContext, in int) (string, error) {
			_, err := f.Run("service", AsAction(func(context.Context, int) (int, error) { calls++; return 0, transient }), in, retry)
			var business *ActionError
			if errors.As(err, &business) {
				return "fallback", nil
			}
			return "ok", nil
		}))
		for i := 0; i < 2; i++ {
			result, err := e.Drive(context.Background(), flow, 0)
			if err != nil || result.Output != "fallback" || calls != 3 {
				t.Fatalf("%+v %v calls=%d", result, err, calls)
			}
		}
		if r, _ := store.Get(context.Background(), "service"); r.Status != Failed || r.Error != "503" {
			t.Fatal(r)
		}
	})
	t.Run("retry policy recovers transient failure", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		calls := 0
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("service", AsAction(func(context.Context, int) (int, error) {
				calls++
				if calls < 3 {
					panic("flaky")
				}
				return 7, nil
			}), in, ActionOptions{Retry: RetryPolicy{MaxAttempts: 5, InitialInterval: time.Microsecond}})
		}))
		result, err := e.Drive(context.Background(), flow, 0)
		if err != nil || result.Output != 7 || calls != 3 {
			t.Fatalf("%+v %v calls=%d", result, err, calls)
		}
	})
	t.Run("listener panic isolated", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		defer e.Subscribe(func(context.Context, Event) { panic("metrics") })()
		result, err := e.Drive(context.Background(), definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("pure", Pure(func(n int) int { return n + 1 }), in)
		})), 4)
		if err != nil || result.Output != 5 {
			t.Fatalf("%+v %v", result, err)
		}
	})
}

func TestFlowContextRun(t *testing.T) {
	t.Run("duplicate and invalid keys", func(t *testing.T) {
		for _, key := range []string{"", "x|y", "中文", "a-b", "one"} {
			e := testEngine(t, memory.New(), Options{})
			flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
				f.Run("one", Pure(func(n int) int { return n }), in)
				return f.Run(key, Pure(func(n int) int { return n }), in)
			}))
			result, err := e.Drive(context.Background(), flow, 0)
			if !errors.Is(err, ErrInvalid) || result.Status != Failed {
				t.Fatalf("key=%q: %+v %v", key, result, err)
			}
		}
	})
	t.Run("concurrent drivers execute once", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		started := make(chan struct{})
		finish := make(chan struct{})
		var calls atomic.Int32
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("exclusive", AsAction(func(context.Context, int) (int, error) {
				calls.Add(1)
				close(started)
				<-finish
				return 4, nil
			}), in)
		}))
		first := make(chan error, 1)
		go func() { _, err := e.Drive(context.Background(), flow, 0); first <- err }()
		<-started
		_, err := e.Drive(context.Background(), flow, 0)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("second: %v", err)
		}
		close(finish)
		if err = <-first; err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 {
			t.Fatal(calls.Load())
		}
	})
	t.Run("action input change replays with warning", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		var changed []Event
		e.Subscribe(func(_ context.Context, event Event) { changed = append(changed, event) }, InputChanged)
		input, calls := 1, 0
		flow := definition(FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) {
			return f.Run("step", Pure(func(n int) int { calls++; return n }), input)
		}))
		if _, err := e.Drive(context.Background(), flow, 0); err != nil {
			t.Fatal(err)
		}
		input = 2
		result, err := e.Drive(context.Background(), flow, 0)
		if err != nil || result.Status != Completed || result.Output != 1 || calls != 1 {
			t.Fatalf("%+v %v calls=%d", result, err, calls)
		}
		if len(changed) != 1 || changed[0].Key != "step" || changed[0].Kind != ActionKind {
			t.Fatalf("%+v", changed)
		}
	})
	t.Run("action type change replays with type_changed", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		var events []Event
		e.Subscribe(func(_ context.Context, event Event) { events = append(events, event) }, TypeChanged)
		type v1 struct{ N int }
		type v2 struct{ N int } // renamed type, same JSON shape
		if _, err := e.Drive(context.Background(), definition(FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) {
			out, err := f.Run("step", Pure(func(n int) v1 { return v1{n} }), 1)
			return out.N, err
		})), 0); err != nil {
			t.Fatal(err)
		}
		// Same key, renamed output type: replayed, not rejected.
		// key 相同、输出类型改名：照常回放，不拒绝。
		result, err := e.Drive(context.Background(), definition(FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) {
			out, err := f.Run("step", Pure(func(n int) v2 { t.Fatal("re-executed"); return v2{} }), 1)
			return out.N, err
		})), 0)
		if err != nil || result.Status != Completed || result.Output != 1 {
			t.Fatalf("%+v %v", result, err)
		}
		if len(events) != 1 || events[0].Key != "step" || events[0].StoredType == events[0].StepType ||
			!strings.Contains(events[0].StoredType, "v1") || !strings.Contains(events[0].StepType, "v2") {
			t.Fatalf("%+v", events)
		}
		// An incompatible shape still fails to decode, deterministically.
		// 结构不兼容时解码仍会确定地失败。
		result, err = e.Drive(context.Background(), definition(FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) {
			out, err := f.Run("step", Pure(func(n int) string { return "x" }), 1)
			return len(out), err
		})), 0)
		if err == nil || result.Status != Failed {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("kind change rejected", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		if _, err := e.Drive(context.Background(), definition(FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) {
			return f.Run("step", Pure(func(n int) int { return n }), 1)
		})), 0); err != nil {
			t.Fatal(err)
		}
		result, err := e.Drive(context.Background(), definition(FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) {
			return f.WaitFor[int]("step", 1), nil
		})), 0)
		if !errors.Is(err, ErrChanged) || result.Status != Failed {
			t.Fatalf("%+v %v", result, err)
		}
	})
}

func TestRootRunFlow(t *testing.T) {
	e := testEngine(t, memory.New(), Options{})
	leaf := FlowFunc[int, string](func(f *FlowContext, in int) (string, error) {
		return f.Run("action", AsAction(func(ctx context.Context, _ int) (string, error) { return ActionKey(ctx), nil }), in)
	})
	flow := NewWorkflow("test", "1", FlowFunc[int, string](func(f *FlowContext, in int) (string, error) { return f.RunFlow("leaf", leaf, in) }))
	result, err := e.Drive(context.Background(), flow, 1)
	if err != nil || result.Output != "leaf|action" {
		t.Fatalf("%+v %v", result, err)
	}
	// A nil root, including a typed nil FlowFunc, is rejected before running.
	// nil 根（包括带类型的 nil FlowFunc）会在执行前被拒绝。
	for _, root := range []Flow[int, string]{nil, FlowFunc[int, string](nil)} {
		if result, err := e.Drive(context.Background(), NewWorkflow("test", "1", root), 1); !errors.Is(err, ErrInvalid) || result.Status == Completed {
			t.Fatalf("%+v %v", result, err)
		}
	}
}
