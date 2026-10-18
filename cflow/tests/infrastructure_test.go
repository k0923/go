package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

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
			store := memory.New()
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
		store := memory.New()
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
		store := memory.New()
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
		store := memory.New()
		e := testEngine(t, store, Options{})
		flow := definition(FlowFunc[int, string](func(f *FlowContext, in int) (string, error) { return f.WaitFor[string]("signal", in), nil }))
		// Initialize the definition first, then reject only signal writes.
		// 先初始化定义，然后只拒绝 signal 写入。
		first, err := e.Drive(context.Background(), flow, 0)
		if err != nil || first.Status != Suspended {
			t.Fatalf("%+v %v", first, err)
		}
		cause := errors.New("registration unavailable")
		e = testEngine(t, &signalFailingInfrastructure{Infrastructure: store, cause: cause}, Options{})
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
