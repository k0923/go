package tests

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/memory"
)

// countingStore counts per-step reads; *memory.Infrastructure still provides
// RecordLister, Clock, WaitAny and the notification outbox.
// countingStore 统计逐步读取次数；*memory.Infrastructure 仍提供 RecordLister、
// Clock、WaitAny 和通知发件箱。
type countingStore struct {
	*memory.Infrastructure
	gets, puts atomic.Int32
}

func (s *countingStore) Get(ctx context.Context, key string) (*Record, error) {
	s.gets.Add(1)
	return s.Infrastructure.Get(ctx, key)
}

func (s *countingStore) Put(ctx context.Context, r *Record) (*Record, error) {
	if r.Kind != DefinitionKind {
		s.puts.Add(1)
	}
	return s.Infrastructure.Put(ctx, r)
}

func TestOptionalInfrastructure(t *testing.T) {
	ctx := context.Background()
	t.Run("preloaded terminal records replay without per-step reads", func(t *testing.T) {
		store := &countingStore{Infrastructure: memory.New()}
		e := testEngine(t, store, Options{})
		input := 1
		calls := 0
		flow := definition(FlowFunc[int, string](func(f *FlowContext, in int) (string, error) {
			out, err := f.Run("step", AsAction(func(context.Context, int) (string, error) { calls++; return "done", nil }), input)
			if err != nil {
				return "", err
			}
			f.Now("at")
			f.Sleep("pause", 0)
			return out + ":" + f.WaitFor[string]("reply", in), nil
		}))
		if result, err := e.Drive(ctx, flow, 1); err != nil || result.Status != Suspended {
			t.Fatalf("%+v %v", result, err)
		}
		if ok, err := e.Signal(ctx, "reply", "yes"); !ok || err != nil {
			t.Fatalf("%t %v", ok, err)
		}
		store.gets.Store(0)
		store.puts.Store(0)
		for range 2 {
			result, err := e.Drive(ctx, flow, 1)
			if err != nil || result.Status != Completed || result.Output != "done:yes" {
				t.Fatalf("%+v %v", result, err)
			}
		}
		if store.gets.Load() != 0 || store.puts.Load() != 0 || calls != 1 {
			t.Fatalf("gets=%d puts=%d calls=%d", store.gets.Load(), store.puts.Load(), calls)
		}
		// A changed input replays the preloaded record; a changed kind is drift.
		// 输入变化时回放预加载记录；Kind 变化才是漂移。
		input = 2
		if result, err := e.Drive(ctx, flow, 1); err != nil || result.Status != Completed || calls != 1 {
			t.Fatalf("%+v %v calls=%d", result, err, calls)
		}
		changed := definition(FlowFunc[int, string](func(f *FlowContext, in int) (string, error) {
			return fmt.Sprint(f.WaitFor[int]("step", input)), nil
		}))
		if result, err := e.Drive(ctx, changed, 1); result.Status != Failed || !errors.Is(err, ErrChanged) {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("store without lister keeps reading per step", func(t *testing.T) {
		store := &countingStore{Infrastructure: memory.New()}
		e := testEngine(t, struct{ Infrastructure }{store}, Options{})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("step", Pure(func(in int) int { return in + 1 }), in)
		}))
		for range 2 {
			if result, err := e.Drive(ctx, flow, 1); err != nil || result.Output != 2 {
				t.Fatalf("%+v %v", result, err)
			}
		}
		if store.gets.Load() == 0 {
			t.Fatal("expected per-step reads without RecordLister")
		}
	})
	t.Run("single Flow workflow stores unprefixed keys", func(t *testing.T) {
		store := memory.New()
		e := testEngine(t, store, Options{})
		flow := NewWorkflow("order", "1", FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("charge", Pure(func(in int) int { return in * 2 }), in)
		}))
		if result, err := e.Drive(ctx, flow, 2); err != nil || result.Output != 4 {
			t.Fatalf("%+v %v", result, err)
		}
		if record, _ := store.Get(ctx, "charge"); record == nil || record.Status != Completed {
			t.Fatal("charge not stored at its local key", record)
		}
		if _, err := e.Drive(ctx, NewWorkflow[int, int]("order", "1", nil), 2); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	})
	t.Run("store clock is the default engine clock", func(t *testing.T) {
		now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		store := memory.New()
		store.SetClock(func() time.Time { return now })
		e := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, time.Time](func(f *FlowContext, _ int) (time.Time, error) {
			f.Sleep("pause", time.Hour)
			return f.Now("at"), nil
		}))
		result, err := e.Drive(ctx, flow, 0)
		if err != nil || result.Status != Suspended || !result.Pending[0].Until.Equal(now.Add(time.Hour)) {
			t.Fatalf("%+v %v", result, err)
		}
		now = now.Add(time.Hour)
		result, err = e.Drive(ctx, flow, 0)
		if err != nil || result.Status != Completed || !result.Output.Equal(now) {
			t.Fatalf("%+v %v", result, err)
		}
	})
}
