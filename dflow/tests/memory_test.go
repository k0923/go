package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/memory"
	"github.com/k0923/go/lease"
)

func TestMemoryFencingAndVersionCAS(t *testing.T) {
	ctx := context.Background()
	infra := memory.New()
	if err := infra.Initialize(ctx, dflow.Manifest{Hash: "v1", Start: "node"}, nil); err != nil {
		t.Fatal(err)
	}
	first, err := infra.ClaimNode(ctx, "node", time.Minute)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	if again, err := infra.ClaimNode(ctx, "node", time.Minute); err != nil || again != nil {
		t.Fatal("live node lease claimed twice", again, err)
	}
	if err := infra.ReleaseNode(ctx, first.Lease); err != nil {
		t.Fatal(err)
	}
	second, err := infra.ClaimNode(ctx, "node", time.Minute)
	if err != nil || second == nil {
		t.Fatal(second, err)
	}
	if second.Lease.Generation <= first.Lease.Generation {
		t.Fatal("node generation did not advance")
	}
	commit := dflow.NodeCommit{NodeID: "node", ExpectedVersion: first.State.Version, State: dflow.NodeState{Status: dflow.Succeeded}}
	if _, err := infra.Commit(ctx, first.Lease, commit); !errors.Is(err, lease.ErrConflict) {
		t.Fatalf("stale owner accepted: %v", err)
	}
	if _, err := infra.Commit(ctx, second.Lease, commit); !errors.Is(err, dflow.ErrConflict) {
		t.Fatal("old node version accepted", err)
	}
	commit.ExpectedVersion = second.State.Version
	if _, err := infra.Commit(ctx, second.Lease, commit); err != nil {
		t.Fatal(err)
	}
	if _, err := infra.Commit(ctx, second.Lease, commit); !errors.Is(err, dflow.ErrConflict) {
		t.Fatal("duplicate terminal commit accepted", err)
	}
	loaded, err := infra.Load(ctx)
	if err != nil || loaded.Nodes["node"].Version != second.State.Version+1 {
		t.Fatalf("incorrect snapshot version: %+v %v", loaded, err)
	}
	loaded.Nodes["node"] = dflow.NodeState{Status: dflow.Failed}
	again, err := infra.Load(ctx)
	if err != nil || again.Nodes["node"].Status != dflow.Succeeded {
		t.Fatalf("load aliases internal state: %+v %v", again, err)
	}
}
