package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

// Replay identity is the built key + kind (+ the stored path). Inputs and
// leaf types are observed only (input_changed / type_changed).
// 回放身份是构建出的 key + kind（+ 存储路径）。输入和叶子类型只被观测
// （input_changed / type_changed）。
func TestReplayIdentity(t *testing.T) {
	ctx := context.Background()
	t.Run("mounted Flow signature is not identity", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		calls := 0
		charge := func(f *FlowContext) (int, error) {
			return f.Run("charge", Pure(func(n int) int { calls++; return n * 10 }), 1)
		}
		v1 := NewWorkflow("order", "1", FlowFunc[int, int](func(w *FlowContext, in int) (int, error) {
			return w.RunFlow("pay", FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) { return charge(f) }), in)
		}))
		if result, err := e.Drive(ctx, v1, 0); err != nil || result.Output != 10 {
			t.Fatalf("%+v %v", result, err)
		}
		record, _ := store.Get(ctx, "pay|charge")
		if record == nil || record.Type != "action[int,int]" ||
			!record.Path.Equal(Path{{Name: "pay"}, {Name: "charge"}}) {
			t.Fatalf("record: %+v", record)
		}
		// The mounted Flow's input type changed: inner steps still replay.
		// 挂载 Flow 的输入类型变了：内部步骤仍然回放。
		v2 := NewWorkflow("order", "1", FlowFunc[int, int](func(w *FlowContext, in int) (int, error) {
			return w.RunFlow("pay", FlowFunc[string, int](func(f *FlowContext, _ string) (int, error) { return charge(f) }), fmt.Sprint(in))
		}))
		if result, err := e.Drive(ctx, v2, 0); err != nil || result.Output != 10 || calls != 1 {
			t.Fatalf("%+v %v calls=%d", result, err, calls)
		}
	})
	t.Run("GoFlow and RunFlow share the mount path", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		calls := 0
		leaf := FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("step", Pure(func(n int) int { calls++; return n + 1 }), in)
		})
		run := NewWorkflow("order", "1", FlowFunc[int, int](func(w *FlowContext, in int) (int, error) { return w.RunFlow("leaf", leaf, in) }))
		async := NewWorkflow("order", "1", FlowFunc[int, int](func(w *FlowContext, in int) (int, error) { return w.GoFlow("leaf", leaf, in).Wait() }))
		for _, flow := range []Workflow[int, int]{run, async} {
			if result, err := e.Drive(ctx, flow, 1); err != nil || result.Output != 2 || calls != 1 {
				t.Fatalf("%+v %v calls=%d", result, err, calls)
			}
		}
	})
	t.Run("re-executed record takes the current input, type and path", func(t *testing.T) {
		store := memory.New()
		store.Seed(&Record{Key: "step", Kind: ActionKind, Status: Running, Version: 1, Input: []byte("1")})
		e := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("step", Pure(func(n int) int { return n * 3 }), in)
		}))
		if result, err := e.Drive(ctx, flow, 2); err != nil || result.Output != 6 {
			t.Fatalf("%+v %v", result, err)
		}
		if record, _ := store.Get(ctx, "step"); record.Type != "action[int,int]" || string(record.Input) != "2" ||
			!record.Path.Equal(Path{{Name: "step"}}) {
			t.Fatalf("not refreshed: %+v", record)
		}
	})
	t.Run("checkpoint identities", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			f.Now("at")
			f.Sleep("pause", 0)
			return f.UUID("id"), nil
		}))
		first, err := e.Drive(ctx, flow, 0)
		if err != nil || first.Status != Completed {
			t.Fatalf("%+v %v", first, err)
		}
		for key, want := range map[string]string{"at": "now[time.Time]", "pause": "sleep", "id": "uuid[string]"} {
			if record, _ := store.Get(ctx, key); record == nil || record.Type != want {
				t.Fatalf("%s: %+v", key, record)
			}
		}
		// Now and UUID share a kind; the primitive tag still tells them apart.
		// Now 与 UUID 共用 kind；原语标签仍能区分它们。
		swapped := definition(FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			f.UUID("at")
			return "", nil
		}))
		if _, err = e.Drive(ctx, swapped, 0); !errors.Is(err, ErrChanged) {
			t.Fatal(err)
		}
	})
	t.Run("WaitAny members replay changed inputs and types", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		var typeChanged atomic.Int32
		defer e.Subscribe(func(context.Context, Event) { typeChanged.Add(1) }, TypeChanged)()
		request, timeout := 1, time.Hour
		flow := definition(FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			reply := OnSignal[string]("reply", request)
			f.WaitAny("race", reply, OnSleep("timeout", timeout))
			return reply.Value(), nil
		}))
		if result, err := e.Drive(ctx, flow, 0); err != nil || result.Status != Suspended {
			t.Fatalf("%+v %v", result, err)
		}
		request, timeout = 2, time.Minute
		if result, err := e.Drive(ctx, flow, 0); err != nil || result.Status != Suspended {
			t.Fatalf("changed inputs must replay: %+v %v", result, err)
		}
		renamed := definition(FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			f.WaitAny("race", OnSignal[string]("answer", request), OnSleep("timeout", timeout))
			return "", nil
		}))
		if _, err := e.Drive(ctx, renamed, 0); !errors.Is(err, ErrChanged) {
			t.Fatal("renamed member must be drift", err)
		}
	})
}

type toolCall struct {
	ID   string
	Name string
}

func (c toolCall) StepKey() string { return c.ID }

func TestStepKeyAndScope(t *testing.T) {
	ctx := context.Background()
	t.Run("StepKey gives fixed names a data identity", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		var calls atomic.Int32
		tool := Pure(func(c toolCall) string { calls.Add(1); return "ok:" + c.Name })
		calls0 := []toolCall{{"call_1", "weather"}, {"a|b[c]", "email"}}
		flow := definition(FlowFunc[[]toolCall, []string](func(f *FlowContext, in []toolCall) ([]string, error) {
			var futures []*Future[string]
			for _, call := range in {
				futures = append(futures, f.Go("tool_call", tool, call))
			}
			var out []string
			for _, future := range futures {
				v, err := future.Wait()
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		}))
		for range 2 {
			if result, err := e.Drive(ctx, flow, calls0); err != nil || len(result.Output) != 2 {
				t.Fatalf("%+v %v", result, err)
			}
		}
		if calls.Load() != 2 {
			t.Fatalf("calls=%d", calls.Load())
		}
		// The ID is escaped by DefaultKey and kept raw in Path.
		// ID 在 DefaultKey 中被转义，在 Path 中保持原样。
		record, _ := store.Get(ctx, `tool_call[a\|b\[c\]]`)
		if record == nil || !record.Path.Equal(Path{{Name: "tool_call", ID: "a|b[c]"}}) {
			t.Fatalf("%+v", record)
		}
		// Same StepKey twice in one drive is a duplicate.
		// 同一次 drive 中出现两次相同的 StepKey 属于重复。
		dup := testEngine(t, memory.New(), Options{})
		if result, err := dup.Drive(ctx, flow, []toolCall{{"x", "a"}, {"x", "b"}}); result.Status != Failed || !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("Scope namespaces loop iterations without records", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, n int) (int, error) {
			sum := 0
			for i := range n {
				s := f.Scope("step", fmt.Sprint(i))
				v, err := s.Run("llm", Pure(func(i int) int { return i * i }), i)
				if err != nil {
					return 0, err
				}
				s.Sleep("pause", 0)
				sum += v
			}
			return sum, nil
		}))
		if result, err := e.Drive(ctx, flow, 3); err != nil || result.Output != 5 {
			t.Fatalf("%+v %v", result, err)
		}
		if record, _ := store.Get(ctx, "step[2]|llm"); record == nil || record.Status != Completed {
			t.Fatalf("%+v", record)
		}
		if record, _ := store.Get(ctx, "step[2]"); record != nil {
			t.Fatalf("scope must not create a record: %+v", record)
		}
	})
	t.Run("StepKey panic and invalid IDs fail the drive", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) {
			f.Scope("s", strings.Repeat("x", MaxIDLength+1))
			return 0, nil
		}))
		if result, err := e.Drive(ctx, flow, 0); result.Status != Failed || !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v %v", result, err)
		}
		bad := definition(FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) {
			return f.Run("step", Pure(func(panicky) int { return 0 }), panicky{})
		}))
		if result, err := testEngine(t, memory.New(), Options{}).Drive(ctx, bad, 0); result.Status != Failed || !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v %v", result, err)
		}
	})
}

type panicky struct{}

func (panicky) StepKey() string { panic("boom") }

func TestNestedFlows(t *testing.T) {
	ctx := context.Background()
	t.Run("Flow mounts Flow with keyed paths", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		sub := FlowFunc[toolCall, string](func(f *FlowContext, c toolCall) (string, error) {
			return f.Run("work", Pure(func(c toolCall) string { return "done:" + c.ID }), c)
		})
		agent := FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			a := f.GoFlow("agent", sub, toolCall{ID: "t1"})
			b, err := f.RunFlow("agent", sub, toolCall{ID: "t2"})
			if err != nil {
				return "", err
			}
			va, err := a.Wait()
			return va + "," + b, err
		})
		flow := NewWorkflow("nested", "1", agent)
		if result, err := e.Drive(ctx, flow, 0); err != nil || result.Output != "done:t1,done:t2" {
			t.Fatalf("%+v %v", result, err)
		}
		record, _ := store.Get(ctx, "agent[t1]|work[t1]")
		if record == nil || !record.Path.Equal(Path{{Name: "agent", ID: "t1"}, {Name: "work", ID: "t1"}}) {
			t.Fatalf("%+v", record)
		}
	})
	t.Run("recursion is bounded by MaxFlowDepth", func(t *testing.T) {
		var recurse FlowFunc[int, int]
		recurse = func(f *FlowContext, n int) (int, error) {
			v, err := f.RunFlow("child", recurse, n+1)
			return v + 1, err
		}
		e := testEngine(t, memory.New(), Options{MaxFlowDepth: 3})
		result, err := e.Drive(ctx, NewWorkflow("recurse", "1", recurse), 0)
		if result.Status != Failed || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "MaxFlowDepth") {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("depth 3 within the limit succeeds", func(t *testing.T) {
		var down FlowFunc[int, int]
		down = func(f *FlowContext, n int) (int, error) {
			if n == 0 {
				return f.Run("leaf", Pure(func(int) int { return 42 }), 0)
			}
			return f.RunFlow("child", down, n-1)
		}
		store := memory.New()
		e := testEngine(t, store, Options{MaxFlowDepth: 3})
		if result, err := e.Drive(ctx, NewWorkflow("down", "1", down), 3); err != nil || result.Output != 42 {
			t.Fatalf("%+v %v", result, err)
		}
		if record, _ := store.Get(ctx, "child|child|child|leaf"); record == nil {
			t.Fatal("leaf not found")
		}
	})
}

// lossyKeys is a deliberately broken KeyBuilder that drops IDs.
// lossyKeys 是一个故意丢弃 ID 的错误 KeyBuilder。
type lossyKeys struct{ *memory.Infrastructure }

func (lossyKeys) BuildKey(p Path) string {
	var names []string
	for _, s := range p {
		names = append(names, s.Name)
	}
	return "custom:" + strings.Join(names, "/")
}

func TestKeyBuilder(t *testing.T) {
	ctx := context.Background()
	t.Run("custom builder addresses records", func(t *testing.T) {
		store := lossyKeys{memory.New()}
		e := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.RunFlow("pay", FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
				return f.Run("charge", Pure(func(n int) int { return n + 1 }), in)
			}), in)
		}))
		if result, err := e.Drive(ctx, flow, 1); err != nil || result.Output != 2 {
			t.Fatalf("%+v %v", result, err)
		}
		if record, _ := store.Get(ctx, "custom:pay/charge"); record == nil {
			t.Fatal("custom key not used")
		}
	})
	t.Run("non-injective builder is rejected within one drive", func(t *testing.T) {
		e := testEngine(t, lossyKeys{memory.New()}, Options{})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			a := f.Go("tool", Pure(func(c toolCall) int { return 1 }), toolCall{ID: "a"})
			b := f.Go("tool", Pure(func(c toolCall) int { return 2 }), toolCall{ID: "b"})
			x, _ := a.Wait()
			y, _ := b.Wait()
			return x + y, nil
		}))
		result, err := e.Drive(ctx, flow, 0)
		if result.Status != Failed || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "not injective") {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("non-injective builder is rejected across drives by the stored path", func(t *testing.T) {
		store := lossyKeys{memory.New()}
		e := testEngine(t, store, Options{})
		id := "a"
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("tool", Pure(func(c toolCall) int { return len(c.ID) }), toolCall{ID: id})
		}))
		if _, err := e.Drive(ctx, flow, 0); err != nil {
			t.Fatal(err)
		}
		id = "b" // different path, same lossy key
		if result, err := e.Drive(ctx, flow, 0); result.Status != Failed || !errors.Is(err, ErrChanged) {
			t.Fatalf("%+v %v", result, err)
		}
	})
}
