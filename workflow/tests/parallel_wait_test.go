package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

// One context remains: Flows compose Actions, waits and Flows. What stays
// forbidden at compile time is executing a Workflow as a step or outside Drive.
// 只保留一种上下文：Flow 组合 Action、等待与 Flow。编译期仍禁止的是把 Workflow
// 当作步骤执行，或在 Drive 之外执行它。
func TestFlowCallHierarchy(t *testing.T) {
	// Compile external business code, rather than merely inspecting private flags.
	source := `package boundary
import "github.com/k0923/go/workflow"
func forbidden(f *workflow.FlowContext, root workflow.Workflow[int, int]) {
	root.Run(f, 1)
	f.RunFlow[int, int]("root", root, 1)
	var _ workflow.Flow[int, int] = root
	_ = workflow.WorkflowContext{}
}
`
	path := filepath.Join(t.TempDir(), "boundary.go")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "go", "test", path).CombinedOutput()
	if err == nil || ctx.Err() != nil {
		t.Fatalf("expected compile-time rejection, got %v: %s", err, output)
	}
	for _, want := range []string{
		"root.Run undefined", "missing method Run", "undefined: workflow.WorkflowContext",
	} {
		if !strings.Contains(string(output), want) {
			t.Errorf("missing compiler rejection %q:\n%s", want, output)
		}
	}
}

func TestParallelFlowWaitRecovery(t *testing.T) {
	t.Run("independent workflows with flat parallel waits and duplicate callbacks", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var clock atomic.Int64
		clock.Store(time.Now().UnixNano())
		now := func() time.Time { return time.Unix(0, clock.Load()) }
		const runs, branches = 12, 8
		type run struct {
			store         *memory.Infrastructure
			engine        *Engine
			flow          Workflow[int, int]
			before, after atomic.Int32
		}
		all := make([]*run, runs)
		for i := range all {
			r := &run{store: memory.New()}
			r.store.SetClock(now)
			r.engine = testEngine(t, r.store, Options{Now: now})
			r.flow = NewWorkflow("test", "1", FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
				futures := make([]*Future[int], branches)
				for index := range branches {
					child := FlowFunc[int, int](func(child *FlowContext, input int) (int, error) {
						if _, err := child.Run("before", AsAction(func(context.Context, int) (int, error) {
							r.before.Add(1)
							return input, nil
						}), input); err != nil {
							return 0, err
						}
						if index%2 == 0 {
							child.WaitFor[string]("reply", input)
						} else {
							child.Sleep("sleep", time.Hour)
						}
						return child.Run("after", AsAction(func(context.Context, int) (int, error) {
							r.after.Add(1)
							return 1, nil
						}), input)
					})
					futures[index] = f.GoFlow(fmt.Sprintf("branch_%d", index), child, input)
				}
				total := 0
				for _, future := range futures {
					value, err := future.Wait()
					if err != nil {
						return 0, err
					}
					total += value
				}
				return total, nil
			}))
			all[i] = r
		}
		for stage := range 4 {
			var wg sync.WaitGroup
			for index, r := range all {
				wg.Add(1)
				go func(index int, r *run) {
					defer wg.Done()
					result, err := r.engine.Drive(ctx, r.flow, index)
					if err != nil {
						t.Errorf("drive: %v", err)
						return
					}
					wantPending, wantAfter := 8, int32(0)
					if stage == 1 {
						wantPending, wantAfter = 4, 4
					}
					if stage >= 2 {
						wantPending, wantAfter = 0, 8
					}
					if len(result.Pending) != wantPending || r.before.Load() != 8 || r.after.Load() != wantAfter {
						t.Errorf("run %d stage %d: %+v before=%d after=%d", index, stage, result, r.before.Load(), r.after.Load())
					}
					if stage < 2 && result.Status != Suspended {
						t.Errorf("expected suspended: %+v", result)
					}
					if stage >= 2 && (result.Status != Completed || result.Output != 8) {
						t.Errorf("expected completed: %+v", result)
					}
					if stage == 0 {
						for _, pending := range result.Pending {
							if pending.Kind != SignalKind {
								continue
							}
							record, err := r.store.Get(ctx, pending.Key)
							if err != nil {
								t.Errorf("get: %v", err)
								return
							}
							var callbacks sync.WaitGroup
							var accepted atomic.Int32
							for range 4 {
								callbacks.Add(1)
								go func() {
									defer callbacks.Done()
									ok, err := r.engine.ResolveSignal(ctx, record.Key, record.Version, fmt.Sprintf("run_%d", index))
									if err != nil {
										t.Errorf("resolve: %v", err)
									}
									if ok {
										accepted.Add(1)
									}
								}()
							}
							callbacks.Wait()
							if accepted.Load() != 1 {
								t.Errorf("callback accepted %d times", accepted.Load())
							}
							resolved, _ := r.store.Get(ctx, record.Key)
							if string(resolved.Result) != fmt.Sprintf(`"run_%d"`, index) {
								t.Error("cross-run callback")
							}
						}
					}
					if leases, _, _, _ := r.store.Stats(); leases != 0 {
						t.Error("lease leaked")
					}
				}(index, r)
			}
			wg.Wait()
			if t.Failed() {
				return
			}
			if stage == 1 {
				clock.Add(int64(time.Hour))
			}
		}
	})
	t.Run("same scope replays a changed request and a changed type", func(t *testing.T) {
		store := memory.New()
		engine := testEngine(t, store, Options{})
		var changed atomic.Int32
		defer engine.Subscribe(func(context.Context, Event) { changed.Add(1) }, InputChanged)()
		request := 1
		flow := definition(FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) { return f.WaitFor[string]("reply", request), nil }))
		first, err := engine.Drive(context.Background(), flow, 0)
		if err != nil || first.Status != Suspended {
			t.Fatalf("%+v %v", first, err)
		}
		// The changed request is observed but not re-sent: the wait stays the same.
		// 变化的请求会被观测但不会重发：等待保持不变。
		request = 2
		second, err := engine.Drive(context.Background(), flow, 0)
		record, _ := store.Get(context.Background(), "reply")
		if err != nil || second.Status != Suspended || record == nil || string(record.Input) != "1" || changed.Load() != 1 {
			t.Fatalf("%+v %v %+v changed=%d", second, err, record, changed.Load())
		}
		var typeChanged atomic.Int32
		defer engine.Subscribe(func(context.Context, Event) { typeChanged.Add(1) }, TypeChanged)()
		typed := definition(FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			return fmt.Sprint(f.WaitFor[int]("reply", request)), nil
		}))
		if result, err := engine.Drive(context.Background(), typed, 0); err != nil || result.Status != Suspended || typeChanged.Load() != 1 {
			t.Fatalf("%+v %v type_changed=%d", result, err, typeChanged.Load())
		}
	})
	t.Run("redundant drives enqueue one request and one wake", func(t *testing.T) {
		store := memory.New()
		engine := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, string](func(f *FlowContext, input int) (string, error) { return f.WaitFor[string]("reply", input), nil }))
		for stage := range 2 {
			var wg sync.WaitGroup
			for range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					result, err := engine.Drive(context.Background(), flow, 1)
					if err != nil {
						t.Errorf("drive: %v", err)
						return
					}
					if stage == 0 && (result.Status != Suspended || len(result.Pending) != 1) {
						t.Errorf("%+v", result)
					}
					if stage == 1 && (result.Status != Completed || result.Output != "approved") {
						t.Errorf("%+v", result)
					}
				}()
			}
			wg.Wait()
			messages, err := store.Pending(context.Background(), time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			requests, wakes := 0, 0
			for _, message := range messages {
				if message.Purpose == memory.RequestNotification {
					requests++
				}
				if message.Purpose == memory.WakeNotification {
					wakes++
				}
			}
			if requests != 1 || wakes != stage {
				t.Fatalf("requests=%d wakes=%d", requests, wakes)
			}
			if stage == 0 {
				record, _ := store.Get(context.Background(), "reply")
				ok, err := engine.ResolveSignal(context.Background(), record.Key, record.Version, "approved")
				if err != nil || !ok {
					t.Fatalf("%t %v", ok, err)
				}
			}
		}
	})
}

func TestFlowScopeJoinsActions(t *testing.T) {
	for _, mode := range []string{"RunFlow", "GoFlow"} {
		t.Run(mode+" joins unwaited Action failure", func(t *testing.T) {
			engine := testEngine(t, memory.New(), Options{})
			cause := errors.New("action failed")
			child := FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
				f.Go("failed", AsAction(func(context.Context, int) (int, error) { return 0, cause }), input)
				return 1, nil
			})
			var continued atomic.Int32
			root := NewWorkflow("test", "1", FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
				var value int
				var err error
				if mode == "RunFlow" {
					value, err = f.RunFlow("child", child, input)
				} else {
					value, err = f.GoFlow("child", child, input).Wait()
				}
				if err != nil {
					return 0, err
				}
				continued.Add(1)
				return value, nil
			}))
			result, err := engine.Drive(context.Background(), root, 1)
			if !errors.Is(err, cause) || result.Status != Running || continued.Load() != 0 {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
	t.Run("cancelled child joins IO and retains durable wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := memory.New()
		engine := testEngine(t, store, Options{})
		started, stopped := make(chan struct{}), make(chan struct{})
		engine.Subscribe(func(context.Context, Event) { cancel() }, WaitSuspended)
		child := FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
			f.Go("io", AsAction(func(ctx context.Context, input int) (int, error) {
				close(started)
				defer close(stopped)
				<-ctx.Done()
				return 0, ctx.Err()
			}), input)
			<-started
			f.WaitFor[string]("reply", input)
			return 0, nil
		})
		root := NewWorkflow("test", "1", FlowFunc[int, int](func(f *FlowContext, input int) (int, error) { return f.GoFlow("child", child, input).Wait() }))
		result, err := engine.Drive(ctx, root, 1)
		if !errors.Is(err, context.Canceled) || result.Status != Running {
			t.Fatalf("%+v %v", result, err)
		}
		select {
		case <-stopped:
		default:
			t.Fatal("IO not joined")
		}
		reply, _ := store.Get(context.Background(), "child|reply")
		if leases, _, _, _ := store.Stats(); leases != 0 || reply.Status != Suspended {
			t.Fatal("lease leak or signal lost")
		}
	})
}

func TestParallelFlowWaitAny(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	store := memory.New()
	store.SetClock(now)
	engine := testEngine(t, store, Options{Now: now})
	candidate := FlowFunc[int, string](func(f *FlowContext, input int) (string, error) {
		winner := f.WaitAny("race", OnSignal[string]("reply", input), OnSleep("timeout", time.Hour))
		return string(winner.Kind()), nil
	})
	root := NewWorkflow("test", "1", FlowFunc[int, string](func(f *FlowContext, input int) (string, error) {
		left := f.GoFlow("left", candidate, input)
		right := f.GoFlow("right", candidate, input)
		l, err := left.Wait()
		if err != nil {
			return "", err
		}
		r, err := right.Wait()
		return l + "+" + r, err
	}))
	ctx := context.Background()
	result, err := engine.Drive(ctx, root, 1)
	if err != nil || result.Status != Suspended || len(result.Pending) != 4 {
		t.Fatalf("%+v %v", result, err)
	}
	left, _ := store.Get(ctx, "left|race|reply")
	right, _ := store.Get(ctx, "right|race|reply")
	if ok, err := engine.ResolveSignal(ctx, left.Key, left.Version, "approved"); err != nil || !ok {
		t.Fatalf("%t %v", ok, err)
	}
	result, err = engine.Drive(ctx, root, 1)
	if err != nil || result.Status != Suspended || len(result.Pending) != 2 {
		t.Fatalf("%+v %v", result, err)
	}
	clock.Add(int64(time.Hour))
	for range 2 {
		result, err = engine.Drive(ctx, root, 1)
		if err != nil || result.Status != Completed || result.Output != "SIGNAL+SLEEP" {
			t.Fatalf("%+v %v", result, err)
		}
	}
	ok, err := engine.ResolveSignal(ctx, right.Key, right.Version, "late")
	if err != nil || ok {
		t.Fatalf("late callback: %t %v", ok, err)
	}
	for _, key := range []string{"left|race|timeout", right.Key} {
		record, err := store.Get(ctx, key)
		if err != nil || record.Status != Cancelled {
			t.Fatalf("%+v %v", record, err)
		}
	}
}
