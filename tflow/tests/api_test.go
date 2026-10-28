package tflow_test

import (
	"context"
	"testing"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type unit string

func (u unit) Key() string { return string(u) }

type staticMain struct{ name string }

func (m *staticMain) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return page{task: m, units: []tflow.SubTask{unit(m.name)}}, nil
}

type page struct {
	task  tflow.MainTask
	units []tflow.SubTask
}

func (p page) Tasks() []tflow.SubTask { return p.units }
func (p page) HasMore() bool          { return false }

type secondMain struct{}

func (m *secondMain) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return page{task: m, units: []tflow.SubTask{unit("second")}}, nil
}

type execution struct{}

func (execution) Start(_ context.Context, task tflow.SubTask, ref tflow.Execution) (tflow.DispatchResult, error) {
	if task.Key() == "" || ref.Attempt != 1 {
		return tflow.DispatchResult{}, tflow.ErrInvalid
	}
	return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
}
func (execution) Resume(context.Context, tflow.SubTask, tflow.Execution, tflow.Event) (tflow.DispatchResult, error) {
	return tflow.DispatchResult{}, tflow.ErrInvalid
}
func (execution) Cancel(context.Context, tflow.SubTask, tflow.Execution) error { return nil }

func TestPublicAPISubmitsDifferentBusinessTypesWithoutPlannerRegistration(t *testing.T) {
	ctx := context.Background()
	storage := memory.New()
	first, second := &staticMain{name: "first"}, &secondMain{}
	for id, task := range map[string]tflow.MainTask{"first": first, "second": second} {
		store := storage.ForMain(id)
		runner, err := tflow.New(store, tflow.Options{Executor: execution{}})
		if err != nil {
			t.Fatal(err)
		}
		m, err := runner.Submit(ctx, task, tflow.SubmitOptions{ConcurrencyLimit: 1})
		record, _ := store.GetMain(ctx)
		if err != nil || record.Task != task || m.Status != tflow.MainSucceeded {
			t.Fatalf("same immutable instance was rejected: %+v %v", m, err)
		}
	}
	for _, id := range []string{"first", "second"} {
		m, err := storage.ForMain(id).GetMain(ctx)
		if err != nil || m.Status != tflow.MainSucceeded || m.Subs.Succeeded != 1 {
			t.Fatalf("public API: %+v %v", m, err)
		}
	}
}
