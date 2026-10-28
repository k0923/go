package tflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
)

type mainTask struct{ page int }

func (m *mainTask) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return nil, tflow.ErrInvalid
}

type subTask string

func (s subTask) Key() string { return string(s) }

type plan struct {
	tasks   []tflow.SubTask
	updated tflow.MainTask
	more    bool
}

func (p *plan) Tasks() []tflow.SubTask { return p.tasks }
func (p *plan) HasMore() bool          { return p.more }

func TestPlanRejectsInvalidPages(t *testing.T) {
	task := &mainTask{}
	m, err := tflow.NewMain(task, tflow.SubmitOptions{ConcurrencyLimit: 2}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var typedNil *plan
	var nilSub *invalidSub
	for _, p := range []tflow.Plan{
		nil, typedNil,
		&plan{updated: task, tasks: []tflow.SubTask{subTask("a"), subTask("a")}},
		&plan{updated: task, tasks: []tflow.SubTask{subTask("")}},
		&plan{updated: task, tasks: []tflow.SubTask{nilSub}},
		&plan{updated: task, tasks: []tflow.SubTask{subTask("a"), subTask("b"), subTask("c")}},
	} {
		if _, _, err := tflow.ApplyPlan(m, p, 2, time.Time{}); !errors.Is(err, tflow.ErrInvalidPlan) {
			t.Fatalf("%+v: %v", p, err)
		}
	}
}

type invalidSub struct{}

func (*invalidSub) Key() string { return "invalid" }

func TestHasMoreFalseExhaustsDispatch(t *testing.T) {
	task := &mainTask{}
	m, _ := tflow.NewMain(task, tflow.SubmitOptions{ConcurrencyLimit: 1}, time.Time{})
	m, _, err := tflow.ApplyPlan(m, &plan{tasks: []tflow.SubTask{subTask("a")}, updated: task}, 1, time.Time{})
	if err != nil || m.DispatchCursor != 1 || !m.DispatchExhausted {
		t.Fatalf("source exhaustion was not captured: %+v %v", m, err)
	}
}

func TestStableExecutionIdentity(t *testing.T) {
	first := tflow.Execution{MainID: "a.b", SubKey: "c", Attempt: 1}
	other := tflow.Execution{MainID: "a", SubKey: "b.c", Attempt: 1}
	if first.Key() == other.Key() {
		t.Fatal("ambiguous sub identity")
	}
	second := first
	second.Attempt++
	if first.Key() == second.Key() {
		t.Fatal("rerun identity reused")
	}
	second = first
	second.ExternalRef = "new observation"
	if first.Key() != second.Key() {
		t.Fatal("external reference changed execution identity")
	}
}

func TestCoreDoesNotInterpretBusinessPlanProgress(t *testing.T) {
	task := &mainTask{page: 1}
	m, _ := tflow.NewMain(task, tflow.SubmitOptions{ConcurrencyLimit: 1}, time.Time{})
	updated := &mainTask{page: 2}
	next, subs, err := tflow.ApplyPlan(m, &plan{updated: updated, more: true}, 1, time.Time{})
	if err != nil || len(subs) != 0 || next.DispatchCursor != 0 || next.Task != task || m.Task != task || task.page != 1 {
		t.Fatalf("business state transition: %+v %v", next, err)
	}
}

func TestPlanMayReturnSameImmutableTask(t *testing.T) {
	task := &mainTask{page: 1}
	m, _ := tflow.NewMain(task, tflow.SubmitOptions{ConcurrencyLimit: 1}, time.Time{})
	next, _, err := tflow.ApplyPlan(m, &plan{updated: task, tasks: []tflow.SubTask{subTask("a")}}, 1, time.Time{})
	if err != nil || next.Task != task || !next.DispatchExhausted {
		t.Fatalf("same task: %+v %v", next, err)
	}
}
