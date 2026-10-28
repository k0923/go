package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/sqlite"
)

func BenchmarkSnapshotAndStateTransaction(b *testing.B) {
	for _, size := range []int{100, 1000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			r, err := sqlite.New(b.Context(), filepath.Join(b.TempDir(), "benchmark.db"), options(&codec{}))
			if err != nil {
				b.Fatal(err)
			}
			defer func() {
				if err := r.Close(); err != nil {
					b.Error(err)
				}
			}()
			s, err := r.Open(b.Context(), "job")
			if err != nil {
				b.Fatal(err)
			}
			e, err := tflow.New(s, tflow.Options{})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := e.Submit(b.Context(), &task{Total: size}, tflow.SubmitOptions{ConcurrencyLimit: int64(size)}); err != nil {
				b.Fatal(err)
			}
			b.Run("read", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := s.GetMain(b.Context()); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("state_tx", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if err := s.WithTx(b.Context(), func(tx tflow.Tx) error {
						m, err := tx.Main()
						if err != nil {
							return err
						}
						version := m.Version
						m.Version++
						return tx.SaveMain(version, m.MainState)
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func BenchmarkConcurrentReport(b *testing.B) {
	r, err := sqlite.New(b.Context(), filepath.Join(b.TempDir(), "reports.db"), options(&codec{}))
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			b.Error(err)
		}
	}()
	s, err := r.Open(b.Context(), "job")
	if err != nil {
		b.Fatal(err)
	}
	e, err := tflow.New(s, tflow.Options{})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := e.Submit(b.Context(), &task{Total: 1}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err != nil {
		b.Fatal(err)
	}
	ref := tflow.Report{Execution: tflow.Execution{MainID: "job", SubKey: "0", Attempt: 1}, Status: tflow.SubSucceeded}
	// Idempotent report contention measures the bound main's read/transaction cost.
	if _, err := e.Report(context.Background(), ref); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := e.Report(b.Context(), ref); err != nil {
				b.Error(err)
			}
		}
	})
}
