package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type businessPlan struct{ nextMarker int }

func (businessPlan) Tasks() []tflow.SubTask { return nil }
func (businessPlan) HasMore() bool          { return true }

func TestCommitPlanIsolatesAndRollsBackBusinessProgress(t *testing.T) {
	calls := 0
	storage := memory.New(memory.WithPlanCommit(func(task tflow.MainTask, plan tflow.Plan) (tflow.MainTask, error) {
		calls++
		next := *task.(*businessMain)
		next.marker = plan.(businessPlan).nextMarker
		return &next, nil
	})).ForMain("progress")
	ctx := context.Background()
	original := &businessMain{marker: 1}
	m, err := tflow.NewMain(original, tflow.SubmitOptions{ConcurrencyLimit: 1}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WithTx(ctx, func(tx tflow.Tx) error {
		return tx.CommitPlan(0, m, businessPlan{nextMarker: 2})
	}); err != nil {
		t.Fatal(err)
	}
	current, _ := storage.GetMain(ctx)
	if current.Task.(*businessMain).marker != 2 || original.marker != 1 {
		t.Fatal("initial progress mutated the submitted task")
	}
	sentinel := errors.New("rollback after progress")
	err = storage.WithTx(ctx, func(tx tflow.Tx) error {
		next := current
		next.Version++
		if err := tx.CommitPlan(current.Version, next, businessPlan{nextMarker: 3}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	current, _ = storage.GetMain(ctx)
	if current.Task.(*businessMain).marker != 2 || current.Version != 1 {
		t.Fatal("rolled-back business progress persisted")
	}
	before := calls
	err = storage.WithTx(ctx, func(tx tflow.Tx) error {
		return tx.CommitPlan(99, current, businessPlan{nextMarker: 4})
	})
	if !errors.Is(err, tflow.ErrConflict) || calls != before {
		t.Fatal("adapter interpreted a plan before checking CAS")
	}
}
