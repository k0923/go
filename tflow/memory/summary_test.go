package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type summaryTask struct{}
type summaryUnit struct{}

func (summaryUnit) Key() string { return "unit" }
func (summaryTask) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return tflow.Page{Items: []tflow.SubTask{summaryUnit{}}}, nil
}

func TestChildTransitionRequiresMainVersionForDecisions(t *testing.T) {
	ctx := context.Background()
	s := memory.New().ForMain("version")
	e, _ := tflow.New(s, tflow.Options{})
	if _, err := e.Submit(ctx, summaryTask{}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetMain(ctx)
	err := s.WithTx(ctx, func(tx tflow.Tx) error {
		sub, _ := tx.Sub("unit")
		sub.Version++
		sub.Status = tflow.SubSucceeded
		return tx.SaveSub(sub.Version-1, sub.SubState)
	})
	if !errors.Is(err, tflow.ErrInvalid) {
		t.Fatalf("unversioned summary accepted: %v", err)
	}
	current, _ := s.GetMain(ctx)
	if current.MainProgress != before.MainProgress {
		t.Fatal("unversioned child write leaked")
	}
}

func TestSaveMainCannotOverwriteStoreSummary(t *testing.T) {
	ctx := context.Background()
	s := memory.New().ForMain("summary")
	e, _ := tflow.New(s, tflow.Options{})
	if _, err := e.Submit(ctx, summaryTask{}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(tx tflow.Tx) error {
		record, _ := tx.Main()
		version := record.Version
		record.Subs = tflow.SubSummary{Failed: 100}
		record.Version++
		return tx.SaveMain(version, record.MainState)
	}); err != nil {
		t.Fatal(err)
	}
	record, _ := s.GetMain(ctx)
	if record.Subs.Running != 1 || record.Subs.Failed != 0 {
		t.Fatalf("business overwrote aggregation: %+v", record.Subs)
	}
}
