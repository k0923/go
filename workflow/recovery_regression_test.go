package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

type failureCommitStore struct {
	*MemoryInfrastructure
	failure error
	after   bool
}

func (s *failureCommitStore) Commit(ctx context.Context, token Lease, record *Record, version int64) (*Record, error) {
	if record.Status == Failed && s.failure != nil {
		if s.after {
			if _, err := s.MemoryInfrastructure.Commit(ctx, token, record, version); err != nil {
				return nil, err
			}
		}
		return nil, s.failure
	}
	return s.MemoryInfrastructure.Commit(ctx, token, record, version)
}

func TestPermanentFailureCommitRecovery(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit_applied_%t", after), func(t *testing.T) {
			outage := errors.New("commit response unavailable")
			domain := errors.New("declined")
			memory := NewMemoryInfrastructure()
			store := &failureCommitStore{MemoryInfrastructure: memory, failure: outage, after: after}
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
			memory := NewMemoryInfrastructure()
			engine := testEngine(t, memory, Options{})
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
			record, _ := memory.Get(context.Background(), "action")
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

func TestJoinedSuspensionsPreserveOtherErrors(t *testing.T) {
	for _, mode := range []string{"suspended", "ordinary", "permanent"} {
		t.Run(mode, func(t *testing.T) {
			engine := testEngine(t, NewMemoryInfrastructure(), Options{})
			cause := errors.New("validation failed")
			flow := definition(FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
				_, left := f.WaitFor[int]("left", input)
				_, right := f.WaitFor[int]("right", input)
				var other error
				if mode == "ordinary" {
					other = cause
				}
				if mode == "permanent" {
					other = Permanent(cause)
				}
				return 0, fmt.Errorf("flow cleanup: %w", errors.Join(left, fmt.Errorf("right: %w", right), other))
			}))
			result, err := engine.Drive(context.Background(), flow, 1)
			want := map[string]Status{"suspended": Suspended, "ordinary": Running, "permanent": Failed}[mode]
			if result.Status != want || len(result.Pending) != 2 {
				t.Fatalf("lost joined state: %+v %v", result, err)
			}
			if mode == "suspended" && err != nil || mode != "suspended" && !errors.Is(err, cause) {
				t.Fatalf("lost real error: %v", err)
			}
		})
	}
}

func TestActionOutputNormalizedBeforeCommit(t *testing.T) {
	engine := testEngine(t, NewMemoryInfrastructure(), Options{})
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
	engine := testEngine(t, NewMemoryInfrastructure(), Options{})
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
	memory := NewMemoryInfrastructure()
	engine := testEngine(t, memory, Options{})
	key := "bad"
	flow := definition(FlowFunc[int, WaitAnyResult[string]](func(f *FlowContext, input int) (WaitAnyResult[string], error) {
		return f.WaitAny[string]("race", WaitForSignal(key, make(chan int)), WaitForSignal("good", input))
	}))
	result, err := engine.Drive(context.Background(), flow, 1)
	if result.Status != Suspended || err != nil || len(result.Pending) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	key = "renamed"
	if _, err = engine.Drive(context.Background(), flow, 1); !errors.Is(err, ErrChanged) {
		t.Fatal("failed candidate rename not rejected", err)
	}
	engine = testEngine(t, NewMemoryInfrastructure(), Options{})
	invalid := definition(FlowFunc[int, WaitAnyResult[string]](func(f *FlowContext, input int) (WaitAnyResult[string], error) {
		return f.WaitAny[string]("race", Signal{Key: "bad", Kind: "CUSTOM", Request: make(chan int)}, WaitForSignal("good", input))
	}))
	if _, err = engine.Drive(context.Background(), invalid, 1); !errors.Is(err, ErrSignalUnsupported) {
		t.Fatal("unsupported kind bypassed by encoding error", err)
	}
	messages, _ := engine.store.(*MemoryInfrastructure).Pending(context.Background(), time.Now())
	if len(messages) != 0 {
		t.Fatal("invalid group registered a provider request", messages)
	}
}
