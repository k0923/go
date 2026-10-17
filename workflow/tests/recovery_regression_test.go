package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	. "github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

type failureCommitStore struct {
	*memory.Infrastructure
	failure error
	after   bool
}

func (s *failureCommitStore) Commit(ctx context.Context, token Lease, record *Record, version int64) (*Record, error) {
	if record.Status == Failed && s.failure != nil {
		if s.after {
			if _, err := s.Infrastructure.Commit(ctx, token, record, version); err != nil {
				return nil, err
			}
		}
		return nil, s.failure
	}
	return s.Infrastructure.Commit(ctx, token, record, version)
}

func TestPermanentFailureCommitRecovery(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit_applied_%t", after), func(t *testing.T) {
			outage := errors.New("commit response unavailable")
			domain := errors.New("declined")
			store := &failureCommitStore{Infrastructure: memory.New(), failure: outage, after: after}
			engine := testEngine(t, store, Options{})
			calls := 0
			flow := definition(FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
				return f.Run("action", AsAction(func(context.Context, int) (int, error) {
					calls++
					return 0, Permanent(domain)
				}), input)
			}))
			result, err := engine.Drive(context.Background(), flow, 1)
			if result.Status != Running || !errors.Is(err, outage) || !errors.Is(err, domain) {
				t.Fatalf("unconfirmed commit terminated: %+v %v", result, err)
			}
			store.failure = nil
			result, err = engine.Drive(context.Background(), flow, 1)
			if result.Status != Failed || err == nil {
				t.Fatalf("recovery did not converge to committed failure: %+v %v", result, err)
			}
			wantCalls := 2
			if after {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestPermanentTimeoutAndPanicRemainRetryable(t *testing.T) {
	for _, mode := range []string{"timeout", "panic", "flow_panic"} {
		t.Run(mode, func(t *testing.T) {
			store := memory.New()
			engine := testEngine(t, store, Options{})
			domain := errors.New("domain cause")
			broken := true
			flow := definition(FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
				if mode == "flow_panic" && broken {
					panic(Permanent(domain))
				}
				return f.Run("action", AsAction(func(ctx context.Context, _ int) (int, error) {
					if broken {
						if mode == "timeout" {
							<-ctx.Done()
							return 0, Permanent(domain)
						}
						panic(Permanent(domain))
					}
					return 7, nil
				}), input, ActionOptions{Timeout: time.Millisecond})
			}))
			result, err := engine.Drive(context.Background(), flow, 1)
			if mode == "flow_panic" {
				// Orchestration panics are definition bugs: terminal, fixed by a new version.
				// 编排 panic 属于定义缺陷：终态，通过新版本修复。
				var panicErr *PanicError
				if result.Status != Failed || !errors.As(err, &panicErr) || len(panicErr.Stack) == 0 {
					t.Fatalf("flow panic not terminal: %+v %v", result, err)
				}
				return
			}
			if result.Status != Running || !errors.Is(err, domain) {
				t.Fatalf("uncommitted fault terminated: %+v %v", result, err)
			}
			if mode == "timeout" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			} else {
				var panicErr *PanicError
				if !errors.As(err, &panicErr) || len(panicErr.Stack) == 0 {
					t.Fatal("panic diagnostics lost", err)
				}
			}
			record, _ := store.Get(context.Background(), "action")
			if record != nil && record.Status != Running {
				t.Fatalf("fault persisted as failure: %+v", record)
			}
			broken = false
			result, err = engine.Drive(context.Background(), flow, 1)
			if result.Status != Completed || err != nil || result.Output != 7 {
				t.Fatalf("fault cannot recover: %+v %v", result, err)
			}
		})
	}
}

// Waits on ONE strand are sequential: the first pending wait halts the strand,
// so later waits are not registered and the handler's return value is ignored.
// Independent waits must run on separate strands (Go/GoFlow) to register together.
// 同一 strand 上的等待是顺序的：第一个挂起的等待会停止该 strand，因此后续等待
// 不会被登记，处理函数的返回值也会被忽略。独立等待必须位于不同 strand（Go/GoFlow）
// 才能一起登记。
func TestSequentialWaitsHaltStrand(t *testing.T) {
	store := memory.New()
	engine := testEngine(t, store, Options{})
	cause := errors.New("never observed")
	flow := definition(FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
		f.WaitFor[int]("left", input)
		f.WaitFor[int]("right", input)
		return 0, cause
	}))
	result, err := engine.Drive(context.Background(), flow, 1)
	if err != nil || result.Status != Suspended || len(result.Pending) != 1 || result.Pending[0].Key != "left" {
		t.Fatalf("%+v %v", result, err)
	}
	if record, _ := store.Get(context.Background(), "right"); record != nil {
		t.Fatalf("later wait registered: %+v", record)
	}
}

// A framework failure on one strand wins over a sibling's suspension: the
// drive lost progress and must be retried rather than reported as waiting.
// 某个 strand 上的框架故障优先于兄弟 strand 的挂起：drive 丢失了进度，
// 必须重试，而不是报告为等待中。
func TestAbortWinsOverSiblingSuspension(t *testing.T) {
	engine := testEngine(t, memory.New(), Options{})
	cause := errors.New("503")
	waiting := make(chan struct{})
	root := NewWorkflow("test", "1", func(w *WorkflowContext, input int) (int, error) {
		w.GoFlow("wait", FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
			defer close(waiting)
			return f.WaitFor[int]("reply", input), nil
		}), input)
		return w.RunFlow("fail", FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
			<-waiting
			return f.Run("io", AsAction(func(context.Context, int) (int, error) { return 0, cause }), input)
		}), input)
	})
	result, err := engine.Drive(context.Background(), root, 1)
	if !errors.Is(err, cause) || result.Status != Running || len(result.Pending) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestActionOutputNormalizedBeforeCommit(t *testing.T) {
	engine := testEngine(t, memory.New(), Options{})
	calls := 0
	flow := definition(FlowFunc[int, string](func(f *FlowContext, input int) (string, error) {
		result, err := f.Run("value", AsAction(func(context.Context, int) (map[string]any, error) {
			calls++
			return map[string]any{"n": int(1)}, nil
		}), input)
		return fmt.Sprintf("%T:%v", result["n"], result["n"]), err
	}))
	for range 2 {
		result, err := engine.Drive(context.Background(), flow, 1)
		if err != nil || result.Output != "float64:1" {
			t.Fatalf("first execution and replay disagree: %+v %v", result, err)
		}
	}
	if calls != 1 {
		t.Fatal("action repeated", calls)
	}
}

func TestTypedJSONNumberPreservesLargeInteger(t *testing.T) {
	type payload struct{ Number json.Number }
	engine := testEngine(t, memory.New(), Options{})
	flow := definition(FlowFunc[int, payload](func(f *FlowContext, input int) (payload, error) {
		return f.Run("number", Pure(func(int) payload {
			return payload{Number: json.Number("9007199254740993")}
		}), input)
	}))
	for range 2 {
		result, err := engine.Drive(context.Background(), flow, 1)
		if err != nil || result.Output.Number.String() != "9007199254740993" {
			t.Fatalf("numeric precision changed: %+v %v", result, err)
		}
	}
}

func TestWaitAnyFailedCandidateIdentity(t *testing.T) {
	store := memory.New()
	engine := testEngine(t, store, Options{})
	key := "bad"
	flow := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, input int) (waitOutcome, error) {
		return waitAnyOutcome(f, "race", OnSignal[string](key, make(chan int)), OnSignal[string]("good", input))
	}))
	result, err := engine.Drive(context.Background(), flow, 1)
	if result.Status != Suspended || err != nil || len(result.Pending) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	key = "renamed"
	if _, err = engine.Drive(context.Background(), flow, 1); !errors.Is(err, ErrChanged) {
		t.Fatal("failed candidate rename not rejected", err)
	}
	fresh := memory.New()
	engine = testEngine(t, fresh, Options{})
	invalid := definition(FlowFunc[int, waitOutcome](func(f *FlowContext, input int) (waitOutcome, error) {
		return waitAnyOutcome(f, "race", OnKind[string]("CUSTOM", "bad", make(chan int)), OnSignal[string]("good", input))
	}))
	if _, err = engine.Drive(context.Background(), invalid, 1); !errors.Is(err, ErrSignalUnsupported) {
		t.Fatal("unsupported kind bypassed by encoding error", err)
	}
	messages, _ := fresh.Pending(context.Background(), time.Now())
	if len(messages) != 0 {
		t.Fatal("invalid group registered a provider request", messages)
	}
}
