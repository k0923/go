// This is an application-owned adapter example. The facade below can wrap cflow
// or any other runtime without introducing its SDK types into tflow.
package main

import (
	"context"
	"fmt"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type RunResult struct {
	ID     string
	Done   bool
	Output string
}

type FlowRuntime interface {
	StartRun(context.Context, string, *RowTask) (RunResult, error)
	ResumeRun(context.Context, string, *WakeEvent) (RunResult, error)
	CancelRun(context.Context, string, string) error
}

type CFlowSubTaskExecutor struct{ Runtime FlowRuntime }

func (x CFlowSubTaskExecutor) Start(ctx context.Context, task tflow.SubTask, execution tflow.Execution) (tflow.DispatchResult, error) {
	result, err := x.Runtime.StartRun(ctx, execution.Key(), task.(*RowTask))
	if err != nil {
		return tflow.DispatchResult{}, err
	}
	return dispatchResult(result), nil
}

func (x CFlowSubTaskExecutor) Resume(ctx context.Context, task tflow.SubTask, execution tflow.Execution, event tflow.Event) (tflow.DispatchResult, error) {
	ref := execution.ExternalRef
	if ref == "" {
		// An early callback can race acceptance persistence. Reattach by the same
		// idempotency key instead of assuming an ExternalRef is already available.
		result, err := x.Runtime.StartRun(ctx, execution.Key(), task.(*RowTask))
		if err != nil {
			return tflow.DispatchResult{}, err
		}
		ref = result.ID
	}
	result, err := x.Runtime.ResumeRun(ctx, ref, event.(*WakeEvent))
	if err != nil {
		return tflow.DispatchResult{}, err
	}
	return dispatchResult(result), nil
}

func (x CFlowSubTaskExecutor) Cancel(ctx context.Context, task tflow.SubTask, execution tflow.Execution) error {
	return x.Runtime.CancelRun(ctx, execution.ExternalRef, execution.Key())
}

func dispatchResult(run RunResult) tflow.DispatchResult {
	status := tflow.Waiting
	if run.Done {
		status = tflow.ExecutionSucceeded
	}
	return tflow.DispatchResult{Status: status, ExternalRef: run.ID}
}

type WorkflowTask struct{ Rows, Position int64 }
type RowTask struct{ Row int64 }

func (r *RowTask) Key() string { return fmt.Sprint(r.Row) }

type WakeEvent struct{ ID, Kind string }

func (e *WakeEvent) Key() string { return e.ID }

type RowPlan struct {
	Items        []tflow.SubTask
	NextPosition int64
	More         bool
}

func (p *RowPlan) Tasks() []tflow.SubTask { return p.Items }
func (p *RowPlan) HasMore() bool          { return p.More }
func (t *WorkflowTask) Plan(_ context.Context, req tflow.PlanRequest) (tflow.Plan, error) {
	end := min(t.Rows, t.Position+int64(req.Limit()))
	p := &RowPlan{NextPosition: end, More: end < t.Rows}
	for row := t.Position; row < end; row++ {
		p.Items = append(p.Items, &RowTask{Row: row})
	}
	return p, nil
}

// DemoRuntime simulates a suspended runtime. Replace it with an adapter that
// binds cflow Infrastructure/Drive/Signal in your own application.
type DemoRuntime struct{ runs map[string]RunResult }

func (r *DemoRuntime) StartRun(_ context.Context, key string, _ *RowTask) (RunResult, error) {
	if run, ok := r.runs[key]; ok {
		return run, nil
	}
	run := RunResult{ID: key}
	r.runs[key] = run
	return run, nil
}

func (r *DemoRuntime) ResumeRun(_ context.Context, id string, _ *WakeEvent) (RunResult, error) {
	run, ok := r.runs[id]
	if !ok {
		return RunResult{}, tflow.ErrNotFound
	}
	run.Done, run.Output = true, "workflow output"
	r.runs[id] = run
	return run, nil
}

func (r *DemoRuntime) CancelRun(_ context.Context, id, key string) error {
	if id == "" {
		id = key
	}
	delete(r.runs, id)
	return nil
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	ctx := context.Background()
	s := memory.New(memory.WithPlanCommit(func(task tflow.MainTask, plan tflow.Plan) (tflow.MainTask, error) {
		copy := *task.(*WorkflowTask)
		copy.Position = plan.(*RowPlan).NextPosition
		return &copy, nil
	})).ForMain("workflow-rows")
	e, err := tflow.New(s, tflow.Options{})
	if err != nil {
		return err
	}
	m, err := e.Submit(ctx, &WorkflowTask{Rows: 5}, tflow.SubmitOptions{ConcurrencyLimit: 2})
	if err != nil {
		return err
	}
	dispatcher, err := tflow.NewExecutorDispatcher(e, CFlowSubTaskExecutor{Runtime: &DemoRuntime{runs: make(map[string]RunResult)}})
	if err != nil {
		return err
	}
	if _, err := e.DispatchPending(ctx, 100, dispatcher); err != nil {
		return err
	}
	fmt.Printf("waiting: status=%s active=%d discovered=%d\n", m.Status, m.Subs.Running, m.DispatchCursor)
	for {
		m, err = e.GetMain(ctx)
		if err != nil {
			return err
		}
		if m.Status.Terminal() {
			break
		}
		subs, err := e.ListSubTasks(ctx, tflow.SubQuery{Statuses: []tflow.SubStatus{tflow.SubRunning}})
		if err != nil {
			return err
		}
		for _, sub := range subs {
			if err := e.Resume(ctx, sub.Execution("workflow-rows"), &WakeEvent{ID: "ready", Kind: "workflow_wakeup"}); err != nil {
				return err
			}
		}
		if _, err := e.DispatchPending(ctx, 100, dispatcher); err != nil {
			return err
		}
	}
	fmt.Printf("completed: status=%s discovered=%d succeeded=%d\n", m.Status, m.DispatchCursor, m.Subs.Succeeded)
	return nil
}
