package fsm_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/k0923/go/fsm"
)

type orderState string

func (orderState) FSMState() {}

const (
	pending   orderState = "pending"
	paid      orderState = "paid"
	cancelled orderState = "cancelled"
)

type payment struct{ Amount int }
type order struct{ Total, Received int }

var pay = fsm.NewEvent[orderState, payment]("pay", "支付")

func mustBuild[D any](t *testing.T, b *fsm.Builder[orderState, D]) *fsm.Definition[orderState, D] {
	t.Helper()
	d, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustMachine[D any](t *testing.T, d *fsm.Definition[orderState, D], initial orderState, data D) *fsm.Machine[orderState, D] {
	t.Helper()
	m, err := d.NewMachine(initial, data)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTransitionAndErrors(t *testing.T) {
	b := fsm.NewBuilder[orderState, *order]()
	b.On(pay).From(pending).To(paid).
		Guard(func(_ context.Context, o *order, p payment) (bool, error) {
			return p.Amount == o.Total, nil
		}).
		Action(func(_ context.Context, o *order, p payment) error {
			o.Received += p.Amount
			return nil
		})
	d := mustBuild(t, b)
	o := &order{Total: 100}
	m := mustMachine(t, d, pending, o)
	ctx := context.Background()

	if err := m.Fire(ctx, pay, payment{50}); !errors.Is(err, fsm.ErrGuardRejected) {
		t.Fatalf("expected guard rejection, got %v", err)
	}
	if m.State() != pending || o.Received != 0 {
		t.Fatal("rejected transition changed state or ran action")
	}
	if err := m.Fire(ctx, fsm.NewEvent[orderState, int]("missing"), 1); !errors.Is(err, fsm.ErrUnknownEvent) {
		t.Fatalf("expected unknown event, got %v", err)
	}
	if err := m.Fire(ctx, fsm.NewEvent[orderState, int]("pay"), 1); !errors.Is(err, fsm.ErrEventTypeMismatch) {
		t.Fatalf("expected type mismatch, got %v", err)
	}
	// Reconstructing the same typed event does not require its description.
	if err := m.Fire(ctx, fsm.NewEvent[orderState, payment]("pay"), payment{100}); err != nil {
		t.Fatal(err)
	}
	if m.State() != paid || o.Received != 100 {
		t.Fatalf("state = %v, received = %d", m.State(), o.Received)
	}
	err := m.Fire(ctx, pay, payment{100})
	if !errors.Is(err, fsm.ErrTransitionNotAllowed) {
		t.Fatalf("expected invalid transition, got %v", err)
	}
	var detail *fsm.TransitionError[orderState]
	if !errors.As(err, &detail) || detail.State != paid || detail.Event != "pay" || detail.Operation != "lookup" {
		t.Fatalf("missing transition context: %v", err)
	}
	if _, err := d.NewMachine(orderState("unknown"), o); !errors.Is(err, fsm.ErrUnknownState) {
		t.Fatalf("expected unknown state, got %v", err)
	}
}

func TestCallbackFailures(t *testing.T) {
	cause := errors.New("storage unavailable")
	for _, operation := range []string{"guard", "action"} {
		t.Run(operation, func(t *testing.T) {
			b := fsm.NewBuilder[orderState, *order]()
			called := false
			r := b.On(pay).From(pending).To(paid).
				Action(func(context.Context, *order, payment) error {
					called = true
					return cause
				})
			if operation == "guard" {
				r.Guard(func(context.Context, *order, payment) (bool, error) {
					return false, cause
				})
			}
			m := mustMachine(t, mustBuild(t, b), pending, &order{})
			err := m.Fire(context.Background(), pay, payment{})
			var detail *fsm.TransitionError[orderState]
			if !errors.Is(err, cause) || !errors.As(err, &detail) || detail.Operation != operation {
				t.Fatalf("lost callback error: %v", err)
			}
			if m.State() != pending || called != (operation == "action") {
				t.Fatal("incorrect state or callback execution after failure")
			}
		})
	}
}

func TestDefinitionValidation(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fsm.Builder[orderState, struct{}])
	}{
		{"empty", func(b *fsm.Builder[orderState, struct{}]) {}},
		{"missing source", func(b *fsm.Builder[orderState, struct{}]) { b.On(pay).To(paid) }},
		{"missing target", func(b *fsm.Builder[orderState, struct{}]) { b.On(pay).From(pending) }},
		{"empty event", func(b *fsm.Builder[orderState, struct{}]) {
			b.On(fsm.NewEvent[orderState, int](" \n")).From(pending).To(paid)
		}},
		{"duplicate state", func(b *fsm.Builder[orderState, struct{}]) { b.State(pending).State(pending) }},
		{"duplicate source", func(b *fsm.Builder[orderState, struct{}]) {
			b.On(pay).From(pending, pending).To(paid)
		}},
		{"duplicate transition", func(b *fsm.Builder[orderState, struct{}]) {
			b.On(pay).From(pending).To(paid)
			b.On(pay).From(pending).To(cancelled)
		}},
		{"conflicting event type", func(b *fsm.Builder[orderState, struct{}]) {
			b.On(pay).From(pending).To(paid)
			b.On(fsm.NewEvent[orderState, int]("pay", "支付")).From(paid).To(cancelled)
		}},
		{"conflicting event description", func(b *fsm.Builder[orderState, struct{}]) {
			b.On(pay).From(pending).To(paid)
			b.On(fsm.NewEvent[orderState, payment]("pay", "different")).From(paid).To(cancelled)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fsm.NewBuilder[orderState, struct{}]()
			tt.configure(b)
			if _, err := b.Build(); !errors.Is(err, fsm.ErrInvalidDefinition) {
				t.Fatalf("expected invalid definition, got %v", err)
			}
		})
	}
}

type floatState float64

func (floatState) FSMState() {}

type compositeState struct{ Value any }

func (compositeState) FSMState() {}

func TestInvalidStateValues(t *testing.T) {
	b := fsm.NewBuilder[floatState, struct{}]()
	b.State(floatState(math.NaN()))
	if _, err := b.Build(); !errors.Is(err, fsm.ErrInvalidDefinition) {
		t.Fatalf("NaN was accepted: %v", err)
	}
	c := fsm.NewBuilder[compositeState, struct{}]()
	c.State(compositeState{Value: []int{1}})
	if _, err := c.Build(); !errors.Is(err, fsm.ErrInvalidDefinition) {
		t.Fatalf("non-comparable field was accepted: %v", err)
	}
	for _, position := range []string{"source", "target"} {
		t.Run(position, func(t *testing.T) {
			b := fsm.NewBuilder[floatState, struct{}]()
			from, to := floatState(0), floatState(1)
			if position == "source" {
				from = floatState(math.NaN())
			} else {
				to = floatState(math.NaN())
			}
			b.On(fsm.NewEvent[floatState, payment]("pay")).From(from).To(to)
			if d, err := b.Build(); d != nil || !errors.Is(err, fsm.ErrInvalidDefinition) {
				t.Fatalf("invalid %s state accepted: %v, %v", position, d, err)
			}
		})
	}
}

func TestBuildSnapshotsRulesAndSources(t *testing.T) {
	b := fsm.NewBuilder[orderState, *order]()
	sources := []orderState{pending}
	r := b.On(pay).From(sources...).To(paid).
		Action(func(_ context.Context, o *order, p payment) error {
			o.Received = p.Amount
			return nil
		})
	sources[0] = cancelled
	first := mustBuild(t, b)
	diagram := first.Mermaid()
	r.From(pending).To(cancelled).Description("new rule").Action(nil)
	b.State(orderState("isolated"))
	second := mustBuild(t, b)

	o := &order{}
	m := mustMachine(t, first, pending, o)
	if err := m.Fire(context.Background(), pay, payment{10}); err != nil {
		t.Fatal(err)
	}
	if m.State() != paid || o.Received != 10 || first.Mermaid() != diagram {
		t.Fatal("builder edits mutated an existing definition")
	}
	m2 := mustMachine(t, second, pending, &order{})
	if err := m2.Fire(context.Background(), pay, payment{}); err != nil || m2.State() != cancelled {
		t.Fatalf("new definition did not reflect edits: %v", err)
	}
}

func TestInterfacePayloadIncludingNil(t *testing.T) {
	event := fsm.NewEvent[orderState, any]("value")
	b := fsm.NewBuilder[orderState, *[]any]()
	b.On(event).From(pending).To(pending).
		Guard(func(_ context.Context, _ *[]any, value any) (bool, error) {
			return true, nil
		}).
		Action(func(_ context.Context, values *[]any, value any) error {
			*values = append(*values, value)
			return nil
		})
	var values []any
	m := mustMachine(t, mustBuild(t, b), pending, &values)
	for _, value := range []any{nil, "text", 42} {
		if err := m.Fire(context.Background(), event, value); err != nil {
			t.Fatal(err)
		}
	}
	if len(values) != 3 || values[0] != nil || values[1] != "text" || values[2] != 42 {
		t.Fatalf("payload values were lost: %#v", values)
	}
}

func TestStatelessFireDoesNotRetainState(t *testing.T) {
	b := fsm.NewBuilder[orderState, *order]()
	b.On(pay).From(pending).To(paid).
		Guard(func(_ context.Context, o *order, p payment) (bool, error) {
			return p.Amount == o.Total, nil
		}).
		Action(func(_ context.Context, o *order, p payment) error {
			o.Received += p.Amount
			return nil
		})
	d := mustBuild(t, b)
	o := &order{Total: 100}
	ctx := context.Background()

	// A rejected guard returns the unchanged source state and no error state leak.
	next, err := d.Fire(ctx, pending, o, pay, payment{50})
	if !errors.Is(err, fsm.ErrGuardRejected) || next != pending {
		t.Fatalf("guard rejection should return source state, got %v, %v", next, err)
	}
	if o.Received != 0 {
		t.Fatal("rejected transition ran the action")
	}

	// The same definition computes transitions for independent current states
	// without storing anything between calls.
	for range 3 {
		next, err = d.Fire(ctx, pending, o, pay, payment{100})
		if err != nil || next != paid {
			t.Fatalf("expected paid, got %v, %v", next, err)
		}
	}
	if o.Received != 300 {
		t.Fatalf("action ran %d times, expected 3", o.Received/100)
	}

	// A disallowed (from, event) pair carries transition context.
	next, err = d.Fire(ctx, paid, o, pay, payment{100})
	var detail *fsm.TransitionError[orderState]
	if !errors.Is(err, fsm.ErrTransitionNotAllowed) || next != paid || !errors.As(err, &detail) || detail.State != paid {
		t.Fatalf("missing transition context: %v, %v", next, err)
	}
}

func TestMultiSourceZeroStateAndIsolatedState(t *testing.T) {
	b := fsm.NewBuilder[orderState, struct{}]()
	b.State(orderState("isolated"), "没有转换")
	b.On(pay).From(pending, paid).To(orderState(""))
	d := mustBuild(t, b)
	for _, source := range []orderState{pending, paid} {
		m := mustMachine(t, d, source, struct{}{})
		if err := m.Fire(context.Background(), pay, payment{}); err != nil || m.State() != "" {
			t.Fatalf("transition from %v failed: %v", source, err)
		}
	}
	isolated := mustMachine(t, d, orderState("isolated"), struct{}{})
	if err := isolated.Fire(context.Background(), pay, payment{}); !errors.Is(err, fsm.ErrTransitionNotAllowed) {
		t.Fatalf("isolated state accepted a transition: %v", err)
	}
}

func TestDefinitionFire(t *testing.T) {
	cause := errors.New("callback failed")
	for _, phase := range []string{"unknown event", "wrong type", "disallowed", "guard rejected", "guard error", "action error", "cancel before", "cancel guard", "cancel action", "success"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls []string
			o := &order{}
			b := fsm.NewBuilder[orderState, *order]()
			b.On(pay).From(pending).To(paid).
				Guard(func(gotCtx context.Context, gotOrder *order, value payment) (bool, error) {
					if gotCtx != ctx || gotOrder != o || value.Amount != 7 {
						t.Fatal("guard did not receive the supplied context, data and payload")
					}
					calls = append(calls, "guard")
					switch phase {
					case "guard rejected":
						return false, nil
					case "guard error":
						return true, cause
					case "cancel guard":
						cancel()
					}
					return true, nil
				}).
				Action(func(gotCtx context.Context, gotOrder *order, value payment) error {
					if gotCtx != ctx || gotOrder != o || value.Amount != 7 {
						t.Fatal("action did not receive the supplied context, data and payload")
					}
					calls = append(calls, "action")
					o.Received += value.Amount
					if phase == "action error" {
						return cause
					}
					if phase == "cancel action" {
						cancel()
					}
					return nil
				})
			d := mustBuild(t, b)
			from, event := pending, pay
			var wantErr error
			var wantCalls []string
			wantState, operation := pending, ""
			switch phase {
			case "unknown event":
				event = fsm.NewEvent[orderState, payment]("unknown")
				wantErr, operation = fsm.ErrUnknownEvent, "lookup"
			case "wrong type":
				wantErr, operation = fsm.ErrEventTypeMismatch, "lookup"
			case "disallowed":
				from, wantState = paid, paid
				wantErr, operation = fsm.ErrTransitionNotAllowed, "lookup"
			case "guard rejected":
				wantErr, operation = fsm.ErrGuardRejected, "guard"
				wantCalls = []string{"guard"}
			case "guard error":
				wantErr, operation = cause, "guard"
				wantCalls = []string{"guard"}
			case "action error":
				wantErr, operation = cause, "action"
				wantCalls = []string{"guard", "action"}
			case "cancel before":
				cancel()
				wantErr = context.Canceled
			case "cancel guard":
				wantErr = context.Canceled
				wantCalls = []string{"guard"}
			default:
				wantState, wantCalls = paid, []string{"guard", "action"}
			}
			var next orderState
			var err error
			if phase == "wrong type" {
				next, err = d.Fire(ctx, from, o, fsm.NewEvent[orderState, int]("pay"), 7)
			} else {
				next, err = d.Fire(ctx, from, o, event, payment{7})
			}
			if next != wantState || !errors.Is(err, wantErr) || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("state=%v error=%v calls=%v; want %v, %v, %v", next, err, calls, wantState, wantErr, wantCalls)
			}
			if operation != "" {
				var detail *fsm.TransitionError[orderState]
				if !errors.As(err, &detail) || detail.State != from || detail.Event != event.Name() || detail.Operation != operation {
					t.Fatalf("incorrect transition context: %v", err)
				}
			}
			wantReceived := 0
			if len(calls) == 2 {
				wantReceived = 7
			}
			if o.Received != wantReceived {
				t.Fatalf("business data=%+v; action side effects must be retained even on error", o)
			}
		})
	}
	t.Run("nil definition", func(t *testing.T) {
		var d *fsm.Definition[orderState, struct{}]
		next, err := d.Fire(context.Background(), pending, struct{}{}, pay, payment{})
		if next != pending || !errors.Is(err, fsm.ErrInvalidDefinition) {
			t.Fatalf("state=%v error=%v", next, err)
		}
	})
}

func TestDefinitionNewMachine(t *testing.T) {
	for _, d := range []*fsm.Definition[orderState, struct{}]{nil, {}} {
		m, err := d.NewMachine(pending, struct{}{})
		if m != nil || !errors.Is(err, fsm.ErrInvalidDefinition) {
			t.Fatalf("unbuilt definition accepted: %v, %v", m, err)
		}
	}
	b := fsm.NewBuilder[floatState, struct{}]()
	b.State(0)
	d, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if m, err := d.NewMachine(floatState(math.NaN()), struct{}{}); m != nil || !errors.Is(err, fsm.ErrUnknownState) {
		t.Fatalf("NaN initial state accepted: %v, %v", m, err)
	}
	c := fsm.NewBuilder[compositeState, struct{}]()
	c.State(compositeState{Value: 1})
	composite, err := c.Build()
	if err != nil {
		t.Fatal(err)
	}
	if m, err := composite.NewMachine(compositeState{Value: []int{1}}, struct{}{}); m != nil || !errors.Is(err, fsm.ErrUnknownState) {
		t.Fatalf("non-comparable initial state accepted: %v, %v", m, err)
	}
}

func TestMachineFire(t *testing.T) {
	for _, m := range []*fsm.Machine[orderState, struct{}]{nil, {}} {
		if err := m.Fire(context.Background(), pay, payment{}); !errors.Is(err, fsm.ErrUninitializedMachine) {
			t.Fatalf("uninitialized machine accepted an event: %v", err)
		}
	}
}

func TestNewEvent(t *testing.T) {
	event := fsm.NewEvent[orderState, int](" value ", "first", "second")
	if event.Name() != " value " || event.Description() != "first\nsecond" {
		t.Fatalf("event metadata changed: %q, %q", event.Name(), event.Description())
	}
	var zero fsm.Event[orderState, int]
	if zero.Name() != "" || zero.Description() != "" {
		t.Fatal("zero event should have empty metadata")
	}
}

func TestTransitionError(t *testing.T) {
	cause := errors.New("storage unavailable")
	err := &fsm.TransitionError[orderState]{State: pending, Event: "pay", Operation: "action", Err: cause}
	if err.Error() != `fsm: action event "pay" from pending: storage unavailable` || err.Unwrap() != cause || !errors.Is(err, cause) {
		t.Fatalf("lost error message or cause: %v", err)
	}
}

func TestTransitionBuilderSetters(t *testing.T) {
	b := fsm.NewBuilder[orderState, *order]()
	r := b.On(pay).From(cancelled).From(pending).To(cancelled).To(paid).
		Guard(func(context.Context, *order, payment) (bool, error) { return false, nil }).
		Action(func(context.Context, *order, payment) error { t.Fatal("removed action ran"); return nil })
	first := mustBuild(t, b)
	r.Guard(nil).Action(nil).Description("old").Description("new")
	second := mustBuild(t, b)
	if next, err := first.Fire(context.Background(), pending, &order{}, pay, payment{}); next != pending || !errors.Is(err, fsm.ErrGuardRejected) {
		t.Fatalf("changing guard mutated the built definition: %v, %v", next, err)
	}
	if next, err := second.Fire(context.Background(), pending, &order{}, pay, payment{}); next != paid || err != nil {
		t.Fatalf("nil callbacks or replacement target not applied: %v, %v", next, err)
	}
	if _, err := second.NewMachine(cancelled, &order{}); !errors.Is(err, fsm.ErrUnknownState) {
		t.Fatalf("replaced source/target remained registered: %v", err)
	}
}
