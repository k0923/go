package tests

import (
	"context"
	"testing"
	"time"

	. "github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/memory"
)

// slowCommitInfrastructure waits for renewal from inside the final write.
// If the engine stops renewal before commit, this operation cannot complete.
// slowCommitInfrastructure 会在最终写入内部等待续租。
// 如果引擎在提交前停止续租，该操作将无法完成。
type slowCommitInfrastructure struct {
	Infrastructure
	renewed chan struct{}
}

func (s *slowCommitInfrastructure) Renew(ctx context.Context, token Lease, ttl time.Duration) (*Lease, error) {
	renewed, err := s.Infrastructure.Renew(ctx, token, ttl)
	if err == nil {
		select {
		case s.renewed <- struct{}{}:
		case <-ctx.Done():
		}
	}
	return renewed, err
}

func (s *slowCommitInfrastructure) Commit(ctx context.Context, token Lease, record *Record, expected int64) (*Record, error) {
	if record.Status == Completed {
		for i := 0; i < 3; i++ {
			select {
			case <-s.renewed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return s.Infrastructure.Commit(ctx, token, record, expected)
}

func TestEngineLeaseIntegration(t *testing.T) {
	store := memory.New()
	slow := &slowCommitInfrastructure{Infrastructure: store, renewed: make(chan struct{}, 4)}
	engine := testEngine(t, slow, Options{LeaseTTL: 150 * time.Millisecond})
	flow := definition(FlowFunc[int, int](func(f *FlowContext, input int) (int, error) {
		return f.Run("step", Pure(func(n int) int { return n + 1 }), input)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := engine.Drive(ctx, flow, 10)
	if err != nil || result.Status != Completed || result.Output != 11 {
		t.Fatalf("renewal did not protect commit: %+v %v", result, err)
	}
	record, err := store.Get(context.Background(), "step")
	if err != nil || record.Status != Completed {
		t.Fatalf("result not committed: %+v %v", record, err)
	}
	if _, err := store.Claim(context.Background(), "step", time.Second); err != nil {
		t.Fatalf("runner did not release after commit: %v", err)
	}
}
