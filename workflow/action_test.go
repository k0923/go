package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

type interfaceTestAction struct{ calls *atomic.Int32 }

func (a interfaceTestAction) Run(ctx context.Context, input int) (pollOutput, error) {
	if ctx.Err() != nil {
		return pollOutput{}, ctx.Err()
	}
	a.calls.Add(1)
	return pollOutput{Count: input + 5, Finished: true}, nil
}

func TestActionFn(t *testing.T) {
	t.Run("forwards context input output and error", func(t *testing.T) {
		cause := errors.New("action rejected")
		ctx := context.WithValue(context.Background(), actionKey{}, "step")
		fn := ActionFn[int, int](func(received context.Context, input int) (int, error) {
			if received != ctx || input != 3 || ActionKey(received) != "step" {
				t.Fatal("context or input lost")
			}
			return 7, cause
		})
		result, err := fn.Run(ctx, 3)
		if result != 7 || !errors.Is(err, cause) {
			t.Fatalf("%d %v", result, err)
		}
	})
	t.Run("nil function returns invalid", func(t *testing.T) {
		var fn ActionFn[int, int]
		result, err := fn.Run(context.Background(), 3)
		if result != 0 || !errors.Is(err, ErrInvalid) {
			t.Fatalf("%d %v", result, err)
		}
	})
}

func TestAsAction(t *testing.T) {
	action := AsAction(func(_ context.Context, input string) (int, error) { return len(input), nil })
	result, err := action.Run(context.Background(), "abc")
	if err != nil || result != 3 {
		t.Fatalf("%d %v", result, err)
	}
}

func TestInterfaceActionExecution(t *testing.T) {
	for _, mode := range []string{"Run", "Go", "Loop"} {
		t.Run(mode, func(t *testing.T) {
			store := NewMemoryInfrastructure()
			engine := testEngine(t, store, Options{})
			var calls atomic.Int32
			action := &interfaceTestAction{calls: &calls}
			flow := definition(FlowFunc[int, pollOutput](func(f *FlowContext, input int) (pollOutput, error) {
				switch mode {
				case "Go":
					return f.Go("step", action, input).Wait()
				case "Loop":
					return f.Loop("step", action, input, LoopOptions{Interval: 1, MaxAttempts: 2})
				default:
					return f.Run("step", action, input)
				}
			}))
			for i := 0; i < 2; i++ {
				result, err := engine.Drive(context.Background(), flow, 3)
				if err != nil || result.Status != Completed || result.Output.Count != 8 {
					t.Fatalf("%+v %v", result, err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("interface action repeated: %d", calls.Load())
			}
		})
	}
	t.Run("value receiver is supported", func(t *testing.T) {
		var calls atomic.Int32
		action := interfaceTestAction{calls: &calls}
		engine := testEngine(t, NewMemoryInfrastructure(), Options{})
		result, err := engine.Drive(context.Background(), definition(FlowFunc[int, pollOutput](
			func(f *FlowContext, input int) (pollOutput, error) { return f.Run("step", action, input) },
		)), 3)
		if err != nil || result.Output.Count != 8 || calls.Load() != 1 {
			t.Fatalf("%+v %v", result, err)
		}
	})
}

func TestInvalidAction(t *testing.T) {
	for _, mode := range []string{"Run", "Go", "Loop"} {
		t.Run(mode, func(t *testing.T) {
			for _, action := range []Action[int, pollOutput]{
				nil, (*interfaceTestAction)(nil), ActionFn[int, pollOutput](nil),
				AsAction[int, pollOutput](nil), Pure[int, pollOutput](nil),
			} {
				store := NewMemoryInfrastructure()
				engine := testEngine(t, store, Options{})
				started := 0
				engine.Subscribe(func(context.Context, Event) { started++ }, ActionStarted)
				flow := definition(FlowFunc[int, pollOutput](func(f *FlowContext, input int) (pollOutput, error) {
					switch mode {
					case "Go":
						return f.Go("step", action, input).Wait()
					case "Loop":
						return f.Loop("step", action, input, LoopOptions{Interval: 1, MaxAttempts: 1})
					default:
						return f.Run("step", action, input)
					}
				}))
				result, err := engine.Drive(context.Background(), flow, 3)
				if !errors.Is(err, ErrInvalid) || result.Status != Running || started != 0 {
					t.Fatalf("%+v %v starts=%d", result, err, started)
				}
				if len(store.leases) != 0 || len(store.records) != 1 {
					t.Fatal("invalid action acquired lease or wrote a step record")
				}
			}
		})
	}
}
