package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/k0923/go/cflow"
)

// BenchmarkLoadExecution loads one boundary of a run holding n boundaries; the
// cost must depend on that boundary only, not on n.
// BenchmarkLoadExecution 在包含 n 个执行边界的运行中加载其中一个；耗时只应取决于该边界，与 n 无关。
func BenchmarkLoadExecution(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("boundaries=%d", n), func(b *testing.B) {
			m := New()
			now := time.Unix(1_800_000_000, 0)
			for i := range n {
				owner := fmt.Sprintf("owner%d", i)
				for j := range 3 {
					m.Seed(&cflow.Record{Key: fmt.Sprintf("%s|step%d", owner, j), ExecutionKey: owner,
						Kind: cflow.ActionKind, Status: cflow.Completed, Version: 2, Result: []byte(`1`), EndedAt: now})
				}
			}
			ctx := context.Background()
			owner := fmt.Sprintf("owner%d", n/2)
			b.ResetTimer()
			for b.Loop() {
				snapshot, err := m.LoadExecution(ctx, owner)
				if err != nil || len(snapshot.Records) != 3 {
					b.Fatalf("%+v %v", snapshot, err)
				}
			}
		})
	}
}
