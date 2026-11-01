package debugger

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/k0923/go/cflow"
)

// BenchmarkLoadExecution loads one boundary of a run holding n boundaries, each
// owning three records, a standalone child and a terminal loop. The cost must
// depend on that boundary only, not on n.
// BenchmarkLoadExecution 在包含 n 个执行边界的运行中加载其中一个；每个边界拥有三条记录、
// 一个独立子级和一个终态循环。耗时只应取决于该边界，与 n 无关。
func BenchmarkLoadExecution(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("boundaries=%d", n), func(b *testing.B) {
			store, err := OpenStore(filepath.Join(b.TempDir(), "bench.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			if err := store.tx(ctx, "bench", func(t *txn) error { return seedBoundaries(t, n) }); err != nil {
				b.Fatal(err)
			}
			owner := fmt.Sprintf("owner%d", n/2)
			b.ResetTimer()
			for b.Loop() {
				snapshot, err := store.LoadExecution(ctx, "bench", owner)
				if err != nil || len(snapshot.Records) != 3 || len(snapshot.Flows) != 1 || len(snapshot.Loops) != 1 {
					b.Fatalf("%+v %v", snapshot, err)
				}
			}
		})
	}
}

func seedBoundaries(t *txn, n int) error {
	now := time.Unix(1_800_000_000, 0)
	for i := range n {
		owner := fmt.Sprintf("owner%d", i)
		for j := range 3 {
			key := fmt.Sprintf("%s|step%d", owner, j)
			if err := t.saveRecord(&cflow.Record{Key: key, Path: cflow.Path{{Name: "step"}}, ExecutionKey: owner,
				Kind: cflow.ActionKind, Status: cflow.Completed, Version: 2, Result: []byte(`1`), StartedAt: now, EndedAt: now}, ""); err != nil {
				return err
			}
		}
		child := &cflow.FlowExecution{Key: owner + "|child", ParentKey: owner, Path: cflow.Path{{Name: "child"}},
			Workflow: "w", FlowName: "c", Status: cflow.Completed, Version: 2, Input: []byte(`1`), Output: []byte(`1`)}
		if err := t.saveFlow(child); err != nil {
			return err
		}
		loop := cflow.NewLoopState(cflow.LoopSpec{Key: owner + "|loop", ParentKey: owner, Path: cflow.Path{{Name: "loop"}},
			Workflow: "w", FlowName: "c", ItemsHash: "h", Depth: 1})
		if err := t.saveLoop(loop); err != nil {
			return err
		}
	}
	return nil
}
