package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func anyWaitFlow() Workflow[int, WaitAnyResult[string]] {
	return definition(FlowFunc[int, WaitAnyResult[string]](func(f *FlowContext, input int) (WaitAnyResult[string], error) {
		return f.WaitAny[string]("race", WaitCase[int]{Key: "a", Request: input}, WaitCase[int]{Key: "b", Request: input})
	}))
}

func TestFlowContextWaitAny(t *testing.T) {
	t.Run("all registered winner cancels losers and continues", func(t *testing.T) {
		ctx := context.Background()
		store := NewMemoryInfrastructure()
		e := testEngine(t, store, Options{})
		calls := 0
		flow := definition(FlowFunc[int, string](func(f *FlowContext, input int) (string, error) {
			winner, err := f.WaitAny[string]("race", WaitCase[int]{Key: "a", Request: input}, WaitCase[int]{Key: "b", Request: input})
			if err != nil {
				return "", err
			}
			return f.Run("after", func(context.Context, int) (string, error) { calls++; return winner.Key + ":" + winner.Response, nil }, input)
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
		if len(messages) != 2 || counts[WakeNotification] != 1 || counts[CancelNotification] != 1 || counts[RequestNotification] != 0 {
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
		store := NewMemoryInfrastructure()
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
		store := NewMemoryInfrastructure()
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
		if err != nil || result.Output.Response != "healthy" || len(result.Output.Failures) != 1 {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("all failed wakes and fails group", func(t *testing.T) {
		ctx := context.Background()
		store := NewMemoryInfrastructure()
		e := testEngine(t, store, Options{})
		_, _ = e.Drive(ctx, anyWaitFlow(), 1)
		for _, key := range []string{"race|a", "race|b"} {
			if _, err := e.FailWait(ctx, key, 1, errors.New("failed")); err != nil {
				t.Fatal(err)
			}
		}
		result, err := e.Drive(ctx, anyWaitFlow(), 1)
		var permanent *PermanentError
		if result.Status != Failed || !errors.As(err, &permanent) {
			t.Fatalf("%+v %v", result, err)
		}
		messages, _ := store.Pending(ctx, time.Now())
		if len(messages) != 1 || messages[0].Purpose != WakeNotification {
			t.Fatal(messages)
		}
		if ok, err := e.FailWait(ctx, "race|a", 1, errors.New("again")); ok || err != nil {
			t.Fatalf("%t %v", ok, err)
		}
	})
	t.Run("request encoding failure and panic isolated", func(t *testing.T) {
		for _, bad := range []any{make(chan int), panicWaitRequest{}} {
			e := testEngine(t, NewMemoryInfrastructure(), Options{})
			flow := definition(FlowFunc[int, WaitAnyResult[string]](func(f *FlowContext, in int) (WaitAnyResult[string], error) {
				return f.WaitAny[string]("race", WaitCase[any]{Key: "bad", Request: bad}, WaitCase[any]{Key: "good", Request: in})
			}))
			result, err := e.Drive(context.Background(), flow, 1)
			if err != nil || result.Status != Suspended || len(result.Pending) != 1 || result.Pending[0].Key != "race|good" {
				t.Fatalf("%+v %v", result, err)
			}
			if _, err := e.ResolveSignal(context.Background(), "race|good", 1, "done"); err != nil {
				t.Fatal(err)
			}
			result, err = e.Drive(context.Background(), flow, 1)
			if err != nil || result.Output.Response != "done" || len(result.Output.Failures) != 1 {
				t.Fatalf("%+v %v", result, err)
			}
		}
	})
	t.Run("invalid and duplicate keys register nothing", func(t *testing.T) {
		for _, cases := range [][]WaitCase[int]{nil, {{Key: "a"}, {Key: "a"}}, {{Key: "a"}, {Key: "b|x"}}} {
			store := NewMemoryInfrastructure()
			e := testEngine(t, store, Options{})
			flow := definition(FlowFunc[int, WaitAnyResult[string]](func(f *FlowContext, _ int) (WaitAnyResult[string], error) { return f.WaitAny[string]("race", cases...) }))
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
	t.Run("optional infrastructure and cancellation", func(t *testing.T) {
		store := NewMemoryInfrastructure()
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
		store := NewMemoryInfrastructure()
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
		if len(store.waitGroups) != 1 || len(store.waitOwners) != 2 {
			t.Fatal("unexpected waiting registrations")
		}
	})
}

type panicWaitRequest struct{}

func (panicWaitRequest) MarshalJSON() ([]byte, error) { panic("candidate marshaler failed") }
