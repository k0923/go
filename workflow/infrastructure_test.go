package workflow

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryInfrastructure(t *testing.T) {
	t.Run("fencing expiry CAS and deep copies", func(t *testing.T) {
		now := time.Now()
		m := NewMemoryInfrastructure()
		m.now = func() time.Time { return now }
		ctx := context.Background()
		first, err := m.Claim(ctx, "step", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		r, err := m.Commit(ctx, *first, &Record{Key: "step", Kind: ActionKind, Status: Running, Input: []byte(`"input"`)}, 0)
		if err != nil || r.Version != 1 {
			t.Fatalf("%+v %v", r, err)
		}
		r.Input[1] = 'X'
		fresh, _ := m.Get(ctx, "step")
		if string(fresh.Input) != `"input"` {
			t.Fatal("stored data mutated")
		}
		now = now.Add(time.Minute)
		fresh.Status = Completed
		fresh.Result = []byte(`7`)
		if _, err := m.Commit(ctx, *first, fresh, 1); !errors.Is(err, ErrConflict) {
			t.Fatal("expired owner committed", err)
		}
		second, err := m.Claim(ctx, "step", time.Minute)
		if err != nil || second.Generation == first.Generation {
			t.Fatalf("%+v %v", second, err)
		}
		if _, err := m.Commit(ctx, *first, fresh, 1); !errors.Is(err, ErrConflict) {
			t.Fatal("zombie committed", err)
		}
		if err := m.Release(ctx, *first); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		if _, err := m.Renew(ctx, *first, time.Minute); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		renewed, err := m.Renew(ctx, *second, time.Hour)
		if err != nil || !renewed.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("%+v %v", renewed, err)
		}
		if _, err := m.Commit(ctx, *second, fresh, 0); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		committed, err := m.Commit(ctx, *second, fresh, 1)
		if err != nil || committed.Version != 2 || committed.Status != Completed {
			t.Fatalf("%+v %v", committed, err)
		}
		if _, err := m.Commit(ctx, *second, committed, 2); !errors.Is(err, ErrConflict) {
			t.Fatal("terminal rewritten", err)
		}
		if err := m.Release(ctx, *second); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("resolve racing registration returns resolved snapshot", func(t *testing.T) {
		m := NewMemoryInfrastructure()
		ctx := context.Background()
		request := &Record{Key: "callback", Kind: SignalKind, Status: Suspended, Input: []byte(`"request"`)}
		stored, err := m.Put(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = m.Resolve(ctx, "callback", stored.Version, []byte(`"approved"`)); err != nil {
			t.Fatal(err)
		}
		existing, err := m.Put(ctx, request)
		if err != nil || existing.Status != Completed || string(existing.Result) != `"approved"` {
			t.Fatalf("%+v %v", existing, err)
		}
		messages, _ := m.Pending(ctx, time.Now())
		if len(messages) != 2 {
			t.Fatal(messages)
		}
		if _, err := m.Put(ctx, &Record{Key: "callback", Kind: SleepKind, Status: Suspended}); !errors.Is(err, ErrChanged) {
			t.Fatal(err)
		}
	})
	t.Run("context cancellation rejects operations", func(t *testing.T) {
		m := NewMemoryInfrastructure()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, getErr := m.Get(ctx, "x")
		_, claimErr := m.Claim(ctx, "x", time.Second)
		_, renewErr := m.Renew(ctx, Lease{}, time.Second)
		releaseErr := m.Release(ctx, Lease{})
		_, commitErr := m.Commit(ctx, Lease{}, &Record{}, 0)
		_, putErr := m.Put(ctx, &Record{})
		_, resolveErr := m.Resolve(ctx, "x", 1, nil)
		_, pendingErr := m.Pending(ctx, time.Now())
		ackErr := m.Ack(ctx, "x")
		for _, err := range []error{getErr, claimErr, renewErr, releaseErr, commitErr, putErr, resolveErr, pendingErr, ackErr} {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel lost: %v", err)
			}
		}
	})
}

type failingInfrastructure struct {
	Infrastructure
	getError, putError, commitError, renewError, releaseError error
}

func (s *failingInfrastructure) Get(ctx context.Context, key string) (*Record, error) {
	if s.getError != nil {
		return nil, s.getError
	}
	return s.Infrastructure.Get(ctx, key)
}
func (s *failingInfrastructure) Put(ctx context.Context, r *Record) (*Record, error) {
	if s.putError != nil {
		return nil, s.putError
	}
	return s.Infrastructure.Put(ctx, r)
}
func (s *failingInfrastructure) Commit(ctx context.Context, l Lease, r *Record, version int64) (*Record, error) {
	if s.commitError != nil && r.Status == Completed {
		return nil, s.commitError
	}
	return s.Infrastructure.Commit(ctx, l, r, version)
}
func (s *failingInfrastructure) Renew(ctx context.Context, l Lease, ttl time.Duration) (*Lease, error) {
	if s.renewError != nil {
		return nil, s.renewError
	}
	return s.Infrastructure.Renew(ctx, l, ttl)
}
func (s *failingInfrastructure) Release(ctx context.Context, l Lease) error {
	if s.releaseError != nil {
		return s.releaseError
	}
	return s.Infrastructure.Release(ctx, l)
}

func TestEngineInfrastructureFailure(t *testing.T) {
	t.Run("transient read and commit errors preserve running state", func(t *testing.T) {
		for _, op := range []string{"Get", "Commit"} {
			store := NewMemoryInfrastructure()
			cause := errors.New("database unavailable")
			fake := &failingInfrastructure{Infrastructure: store}
			if op == "Get" {
				fake.getError = cause
			} else {
				fake.commitError = cause
			}
			e := testEngine(t, fake, Options{})
			flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
				return f.Run("step", Pure(func(n int) int { return n + 1 }), in)
			}))
			result, err := e.Drive(context.Background(), flow, 0)
			if !errors.Is(err, cause) || result.Status != Running {
				t.Fatalf("%+v %v", result, err)
			}
			fake.getError, fake.commitError = nil, nil
			result, err = e.Drive(context.Background(), flow, 0)
			if err != nil || result.Output != 1 {
				t.Fatalf("%+v %v", result, err)
			}
		}
	})
	t.Run("renewal failure cancels action and never commits result", func(t *testing.T) {
		store := NewMemoryInfrastructure()
		cause := errors.New("renewal unavailable")
		fake := &failingInfrastructure{Infrastructure: store, renewError: cause}
		e := testEngine(t, fake, Options{LeaseTTL: 30 * time.Millisecond})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("slow", AsAction(func(ctx context.Context, _ int) (int, error) {
				<-ctx.Done()
				return 99, nil
			}), in)
		}))
		result, err := e.Drive(context.Background(), flow, 0)
		if !errors.Is(err, cause) || result.Status != Running {
			t.Fatalf("%+v %v", result, err)
		}
		record, _ := store.Get(context.Background(), "slow")
		if record.Status != Running || len(record.Result) != 0 {
			t.Fatal(record)
		}
	})
	t.Run("release failure visible after completed commit", func(t *testing.T) {
		store := NewMemoryInfrastructure()
		cause := errors.New("release unavailable")
		fake := &failingInfrastructure{Infrastructure: store, releaseError: cause}
		e := testEngine(t, fake, Options{})
		calls := 0
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			return f.Run("step", AsAction(func(context.Context, int) (int, error) { calls++; return 4, nil }), in)
		}))
		_, err := e.Drive(context.Background(), flow, 0)
		if !errors.Is(err, cause) {
			t.Fatal(err)
		}
		fake.releaseError = nil
		result, err := e.Drive(context.Background(), flow, 0)
		if err != nil || result.Output != 4 || calls != 1 {
			t.Fatalf("%+v %v calls=%d", result, err, calls)
		}
	})
	t.Run("signal registration failure does not claim lease", func(t *testing.T) {
		store := NewMemoryInfrastructure()
		e := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, string](func(f *FlowContext, in int) (string, error) { return f.WaitFor[string]("signal", in) }))
		// Initialize the definition first, then reject only signal writes.
		// 先初始化定义，然后只拒绝 signal 写入。
		first, err := e.Drive(context.Background(), flow, 0)
		if err != nil || first.Status != Suspended {
			t.Fatalf("%+v %v", first, err)
		}
		cause := errors.New("registration unavailable")
		e.store = &signalFailingInfrastructure{Infrastructure: store, cause: cause}
		result, err := e.Drive(context.Background(), flow, 0)
		if !errors.Is(err, cause) || result.Status != Running {
			t.Fatalf("%+v %v", result, err)
		}
		if _, err := store.Claim(context.Background(), "signal", time.Second); err != nil {
			t.Fatal(err)
		}
	})
}

type signalFailingInfrastructure struct {
	Infrastructure
	cause error
}

func (s *signalFailingInfrastructure) Put(ctx context.Context, r *Record) (*Record, error) {
	if r.Kind == SignalKind {
		return nil, s.cause
	}
	return s.Infrastructure.Put(ctx, r)
}
