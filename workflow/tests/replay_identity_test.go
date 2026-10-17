package tests

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

// Replay identity is resolved key + kind + type path; input is observed only.
// 回放身份是解析后的 key + kind + 类型路径；输入只被观测。
func TestReplayIdentity(t *testing.T) {
	ctx := context.Background()
	t.Run("mounted Flow signature is part of every inner identity", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		var changed atomic.Int32
		defer e.Subscribe(func(context.Context, Event) { changed.Add(1) }, InputChanged)()
		calls, inner := 0, 1
		charge := func(f *FlowContext) (int, error) {
			return f.Run("charge", Pure(func(n int) int { calls++; return n * 10 }), inner)
		}
		v1 := NewWorkflow("order", "1", func(w *WorkflowContext, in int) (int, error) {
			return w.RunFlow("pay", FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) { return charge(f) }), in)
		})
		if result, err := e.Drive(ctx, v1, 0); err != nil || result.Output != 10 {
			t.Fatalf("%+v %v", result, err)
		}
		record, _ := store.Get(ctx, "pay|charge")
		if record == nil || record.Type != "flow[int,int]|action[int,int]" {
			t.Fatalf("type path: %+v", record)
		}
		// Same Flow signature, changed inner input: replay with a warning.
		// Flow 签名不变、内部输入变化：带告警回放。
		inner = 2
		if result, err := e.Drive(ctx, v1, 0); err != nil || result.Output != 10 || calls != 1 || changed.Load() != 1 {
			t.Fatalf("%+v %v calls=%d changed=%d", result, err, calls, changed.Load())
		}
		// Same outer key "pay" and inner key "charge" with identical inner types,
		// but the mounted Flow's input type changed: drift.
		// 外层 key "pay"、内层 key "charge" 与内部类型都不变，但挂载 Flow 的输入类型变了：漂移。
		v2 := NewWorkflow("order", "1", func(w *WorkflowContext, in int) (int, error) {
			return w.RunFlow("pay", FlowFunc[string, int](func(f *FlowContext, _ string) (int, error) { return charge(f) }), fmt.Sprint(in))
		})
		if result, err := e.Drive(ctx, v2, 0); result.Status != Failed || !errors.Is(err, ErrChanged) || calls != 1 {
			t.Fatalf("%+v %v calls=%d", result, err, calls)
		}
	})
	t.Run("GoFlow and RunFlow share the mount type path", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
		calls := 0
		leaf := FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("step", Pure(func(n int) int { calls++; return n + 1 }), in)
		})
		run := NewWorkflow("order", "1", func(w *WorkflowContext, in int) (int, error) { return w.RunFlow("leaf", leaf, in) })
		async := NewWorkflow("order", "1", func(w *WorkflowContext, in int) (int, error) { return w.GoFlow("leaf", leaf, in).Wait() })
		for _, flow := range []Workflow[int, int]{run, async} {
			if result, err := e.Drive(ctx, flow, 1); err != nil || result.Output != 2 || calls != 1 {
				t.Fatalf("%+v %v calls=%d", result, err, calls)
			}
		}
	})
	t.Run("record without type keeps strict input comparison", func(t *testing.T) {
		store := memory.New()
		store.Seed(&Record{Key: "step", Kind: ActionKind, Status: Completed, Version: 1,
			Input: []byte("1"), Result: []byte("5")})
		e := testEngine(t, store, Options{})
		input := 1
		flow := definition(FlowFunc[int, int](func(f *FlowContext, _ int) (int, error) {
			return f.Run("step", Pure(func(n int) int { t.Fatal("executed"); return n }), input)
		}))
		if result, err := e.Drive(ctx, flow, 0); err != nil || result.Output != 5 {
			t.Fatalf("%+v %v", result, err)
		}
		input = 2
		if result, err := e.Drive(ctx, flow, 0); result.Status != Failed || !errors.Is(err, ErrChanged) {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("re-executed record takes the current input and type", func(t *testing.T) {
		store := memory.New()
		store.Seed(&Record{Key: "step", Kind: ActionKind, Status: Running, Version: 1, Input: []byte("1")})
		e := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("step", Pure(func(n int) int { return n * 3 }), in)
		}))
		// Strict comparison still applies to the untyped Running record.
		// 无类型的 Running 记录仍按严格比较。
		if _, err := e.Drive(ctx, flow, 2); !errors.Is(err, ErrChanged) {
			t.Fatal(err)
		}
		store = memory.New()
		store.Seed(&Record{Key: "step", Kind: ActionKind, Status: Running, Version: 1, Input: []byte("1")})
		e = testEngine(t, store, Options{})
		if result, err := e.Drive(ctx, flow, 1); err != nil || result.Output != 3 {
			t.Fatalf("%+v %v", result, err)
		}
		if record, _ := store.Get(ctx, "step"); record.Type != "action[int,int]" || string(record.Input) != "1" {
			t.Fatalf("not upgraded: %+v", record)
		}
	})
	t.Run("checkpoint and sleep identities", func(t *testing.T) {
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
		// A checkpoint replayed under another primitive is drift.
		// 以另一种原语回放 checkpoint 属于漂移。
		swapped := definition(FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			f.UUID("at")
			return "", nil
		}))
		if _, err = e.Drive(ctx, swapped, 0); !errors.Is(err, ErrChanged) {
			t.Fatal(err)
		}
	})
	t.Run("WaitAny members compare by type", func(t *testing.T) {
		e := testEngine(t, memory.New(), Options{})
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
		typed := definition(FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			f.WaitAny("race", OnSignal[int]("reply", request), OnSleep("timeout", timeout))
			return "", nil
		}))
		if _, err := e.Drive(ctx, typed, 0); !errors.Is(err, ErrChanged) {
			t.Fatal(err)
		}
	})
}
