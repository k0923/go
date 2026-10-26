package tests

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/memory"
	"github.com/k0923/go/dflow/sqlite"
	"github.com/k0923/go/lease"
)

// instances runs test against two Infrastructure handles bound to the same
// execution: one shared memory store, or two SQLite connections to one file,
// simulating independent HA instances driving nodes of one run.
//
// instances 使用绑定到同一执行的两个 Infrastructure handle 运行测试：一个共享的
// memory store，或同一文件上的两个 SQLite 连接，用来模拟独立 HA 实例驱动同一
// run 的节点。
func instances(t *testing.T, test func(*testing.T, [2]dflow.Infrastructure)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		infra := memory.New()
		test(t, [2]dflow.Infrastructure{infra, infra})
	})
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ha.db")
		var handles [2]dflow.Infrastructure
		for index := range handles {
			store, err := sqlite.Open(context.Background(), path, "run", fixtureCodec{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			handles[index] = store
		}
		test(t, handles)
	})
}

func fanInDefinition() dflow.Definition {
	return dflow.Definition{
		Nodes: []dflow.NodeSpec{
			{ID: "start", Node: &dflow.StartNode{}},
			{ID: "a", Node: namedNode("barrier")},
			{ID: "b", Node: namedNode("barrier")},
			{ID: "join", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"a": dflow.Ref("a", "value"), "b": dflow.Ref("b", "value")}},
		},
		Edges: []dflow.Edge{edge("start", "a"), edge("start", "b"), edge("a", "join"), edge("b", "join")},
	}
}

func TestNodesOfOneRunExecuteInParallelAcrossInstances(t *testing.T) {
	instances(t, func(t *testing.T, infra [2]dflow.Infrastructure) {
		var arrived sync.WaitGroup
		arrived.Add(2)
		registry := testRegistry{}
		register(t, registry, "barrier", func(ctx context.Context, _ map[string]any) (dflow.Result, error) {
			arrived.Done()
			done := make(chan struct{})
			go func() { arrived.Wait(); close(done) }()
			select {
			case <-done:
				// Both nodes are inside Run at the same time.
				//
				// 两个节点同时位于 Run 内部。
				return dflow.Result{Output: map[string]any{"value": dflow.NodeID(ctx)}}, nil
			case <-time.After(2 * time.Second):
				return dflow.Result{}, errors.New("sibling node did not run concurrently")
			}
		})
		ctx := context.Background()
		engines := [2]*observedEngine{
			engine(t, fanInDefinition(), registry, infra[0], dflow.Options{}),
			engine(t, fanInDefinition(), registry, infra[1], dflow.Options{}),
		}
		start, err := engines[0].Start(ctx, nil)
		if err != nil || start != "start" {
			t.Fatal(start, err)
		}
		started, err := engines[0].Drive(ctx, start)
		if err != nil || !slices.Equal(started.Ready, []string{"a", "b"}) {
			t.Fatal(started, err)
		}
		results := make(chan dflow.Executed, 2)
		errs := make(chan error, 2)
		for index, id := range started.Ready {
			go func() {
				executed, err := engines[index].Drive(ctx, id)
				results <- executed
				errs <- err
			}()
		}
		var downstream []string
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			downstream = append(downstream, (<-results).Ready...)
		}
		if !slices.Equal(downstream, []string{"join"}) {
			t.Fatal("join must become ready exactly once", downstream)
		}
		executed, err := engines[1].Drive(ctx, "join")
		output := executed.Output
		if err != nil || executed.Status != dflow.Succeeded || output["a"] != "a" || output["b"] != "b" {
			t.Fatal(executed, err)
		}
	})
}

func TestDuplicateDriveExecutesNodeOnce(t *testing.T) {
	instances(t, func(t *testing.T, infra [2]dflow.Infrastructure) {
		var runs atomic.Int32
		release := make(chan struct{})
		registry := testRegistry{}
		register(t, registry, "slow", func(context.Context, map[string]any) (dflow.Result, error) {
			runs.Add(1)
			<-release
			return dflow.Result{Output: map[string]any{"value": "done"}}, nil
		})
		def := dflow.Definition{Nodes: []dflow.NodeSpec{{ID: "a", Node: namedNode("slow")}}}
		ctx := context.Background()
		engines := [2]*observedEngine{
			engine(t, def, registry, infra[0], dflow.Options{}),
			engine(t, def, registry, infra[1], dflow.Options{}),
		}
		if _, err := engines[0].Start(ctx, nil); err != nil {
			t.Fatal(err)
		}
		const drivers = 8
		var wg sync.WaitGroup
		results := make(chan dflow.Executed, drivers)
		for index := range drivers {
			wg.Go(func() {
				executed, err := engines[index%2].Drive(ctx, "a")
				if err != nil {
					t.Error(err)
				}
				results <- executed
			})
		}
		// Losers return immediately while the winner is still inside Run.
		//
		// winner 仍在 Run 内部时，loser 会立即返回。
		deadline := time.After(2 * time.Second)
		for losers := 0; losers < drivers-1; {
			select {
			case executed := <-results:
				if executed.Status != "" {
					t.Fatal("duplicate drive executed the node", executed)
				}
				losers++
			case <-deadline:
				t.Fatal("duplicate drives blocked on the running node")
			}
		}
		if ready, err := engines[1].Recover(ctx); err != nil || len(ready) != 0 {
			t.Fatal("node under a live lease must not be listed as drivable", ready, err)
		}
		close(release)
		wg.Wait()
		if winner := <-results; winner.Status != dflow.Succeeded || runs.Load() != 1 {
			t.Fatal(winner, runs.Load())
		}
	})
}

func TestExpiredNodeLeaseIsReclaimedAndStaleOwnerFenced(t *testing.T) {
	instances(t, func(t *testing.T, infra [2]dflow.Infrastructure) {
		ctx := context.Background()
		if err := infra[0].Initialize(ctx, dflow.Manifest{Hash: "v1", Start: "a"}, nil); err != nil {
			t.Fatal(err)
		}
		stale, err := infra[0].ClaimNode(ctx, "a", 50*time.Millisecond)
		if err != nil || stale == nil {
			t.Fatal(stale, err)
		}
		if ready, err := infra[1].ReadyNodes(ctx); err != nil || len(ready) != 0 {
			t.Fatal("live lease listed as drivable", ready, err)
		}
		// The owner stopped renewing, as if it crashed.
		//
		// owner 停止续租，等同于已经崩溃。
		time.Sleep(80 * time.Millisecond)
		if _, err := infra[0].RenewNode(ctx, stale.Lease, time.Minute); !errors.Is(err, lease.ErrConflict) {
			t.Fatal("expired lease renewed", err)
		}
		if ready, err := infra[1].ReadyNodes(ctx); err != nil || !slices.Equal(ready, []string{"a"}) {
			t.Fatal("expired lease must be recoverable", ready, err)
		}
		current, err := infra[1].ClaimNode(ctx, "a", time.Minute)
		if err != nil || current == nil || current.Lease.Generation <= stale.Lease.Generation {
			t.Fatal(current, err)
		}
		late := dflow.NodeCommit{NodeID: "a", ExpectedVersion: stale.State.Version, State: dflow.NodeState{Status: dflow.Succeeded}}
		if _, err := infra[0].Commit(ctx, stale.Lease, late); !errors.Is(err, lease.ErrConflict) {
			t.Fatal("stale owner committed", err)
		}
		if err := infra[0].ReleaseNode(ctx, stale.Lease); !errors.Is(err, lease.ErrConflict) {
			t.Fatal("stale owner released the new lease", err)
		}
		commit := dflow.NodeCommit{NodeID: "a", ExpectedVersion: current.State.Version, State: dflow.NodeState{Status: dflow.Succeeded, Output: map[string]any{"value": "new"}}}
		if _, err := infra[1].Commit(ctx, current.Lease, commit); err != nil {
			t.Fatal(err)
		}
		node, err := infra[0].GetNode(ctx, "a")
		if err != nil || node.Status != dflow.Succeeded || node.Output["value"] != "new" {
			t.Fatal(node, err)
		}
	})
}

func TestCommitRejectsExpiredOwnLease(t *testing.T) {
	instances(t, func(t *testing.T, infra [2]dflow.Infrastructure) {
		ctx := context.Background()
		if err := infra[0].Initialize(ctx, dflow.Manifest{Hash: "v1", Start: "a"}, nil); err != nil {
			t.Fatal(err)
		}
		task, err := infra[0].ClaimNode(ctx, "a", 30*time.Millisecond)
		if err != nil || task == nil {
			t.Fatal(task, err)
		}
		time.Sleep(50 * time.Millisecond)
		// Nobody reclaimed yet, but the stored expiry has passed: not owned anymore.
		//
		// 还没有人重新 claim，但存储中的过期时间已经过去：它已不再被持有。
		commit := dflow.NodeCommit{NodeID: "a", ExpectedVersion: task.State.Version, State: dflow.NodeState{Status: dflow.Succeeded}}
		if _, err := infra[1].Commit(ctx, task.Lease, commit); !errors.Is(err, lease.ErrConflict) {
			t.Fatal("expired lease committed", err)
		}
	})
}

// lostRenewal makes every node lease renewal fail, as if another instance
// fenced this owner while its node was still running.
//
// lostRenewal 让每次节点租约续租都失败，就像另一个实例在节点仍在运行时 fencing
// 了当前 owner。
type lostRenewal struct{ *memory.Infrastructure }

func (s *lostRenewal) RenewNode(context.Context, lease.Token, time.Duration) (*lease.Token, error) {
	return nil, lease.ErrConflict
}

func TestLostNodeLeaseCancelsExecutionWithoutCommit(t *testing.T) {
	registry := testRegistry{}
	register(t, registry, "wait", func(ctx context.Context, _ map[string]any) (dflow.Result, error) {
		<-ctx.Done()
		return dflow.Result{}, ctx.Err()
	})
	infra := &lostRenewal{memory.New()}
	def := dflow.Definition{Nodes: []dflow.NodeSpec{{ID: "a", Node: namedNode("wait")}}}
	e := engine(t, def, registry, infra, dflow.Options{Lease: lease.Options{TTL: 30 * time.Millisecond}})
	ctx := context.Background()
	if _, err := e.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	executed, err := e.Drive(ctx, "a")
	var infraErr *dflow.InfrastructureError
	if !errors.Is(err, lease.ErrConflict) || !errors.As(err, &infraErr) || executed.Status != "" {
		t.Fatal("lost lease must surface as an uncertain infrastructure error", executed, err)
	}
	node, err := infra.GetNode(ctx, "a")
	if err != nil || node.Status != dflow.Running {
		t.Fatal("lost owner must not commit Failed", node, err)
	}
	if ready, err := e.Recover(ctx); err != nil || !slices.Equal(ready, []string{"a"}) {
		t.Fatal("released node must be immediately recoverable", ready, err)
	}
}
