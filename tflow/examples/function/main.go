package main

import (
	"context"
	"fmt"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/examples/internal/executor"
	"github.com/k0923/go/tflow/memory"
)

type ImportTask struct{ Rows, Position int64 }

type ImportPlan struct {
	Items        []tflow.SubTask
	NextPosition int64
	More         bool
}

func (p *ImportPlan) Tasks() []tflow.SubTask { return p.Items }
func (p *ImportPlan) HasMore() bool          { return p.More }

type RowTask struct{ Row int64 }

func (r *RowTask) Key() string { return fmt.Sprint(r.Row) }

func (p *ImportTask) Plan(_ context.Context, req tflow.PlanRequest) (tflow.Plan, error) {
	end := min(p.Rows, p.Position+int64(req.Limit()))
	result := &ImportPlan{NextPosition: end, More: end < p.Rows}
	for row := p.Position; row < end; row++ {
		result.Items = append(result.Items, &RowTask{Row: row})
	}
	return result, nil
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	ctx := context.Background()
	s := memory.New(memory.WithPlanCommit(func(task tflow.MainTask, plan tflow.Plan) (tflow.MainTask, error) {
		copy := *task.(*ImportTask)
		copy.Position = plan.(*ImportPlan).NextPosition
		return &copy, nil
	})).ForMain("double-rows")
	results := make(map[string]int64)
	e, err := tflow.New(s, tflow.Options{
		Executor: executor.Func(func(ctx context.Context, task tflow.SubTask, execution tflow.Execution) (tflow.DispatchResult, error) {
			if err := ctx.Err(); err != nil {
				return tflow.DispatchResult{}, err
			}
			// Business results use the stable execution key across retries.
			results[execution.Key()] = task.(*RowTask).Row * 2
			return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
		}),
	})
	if err != nil {
		return err
	}
	m, err := e.Submit(ctx, &ImportTask{Rows: 5}, tflow.SubmitOptions{
		ConcurrencyLimit: 2,
	})
	if err != nil {
		return err
	}
	for !m.Status.Terminal() {
		if _, err := e.Drain(ctx, 100); err != nil {
			return err
		}
		m, err = e.GetMain(ctx)
		if err != nil {
			return err
		}
	}
	fmt.Printf("completed: status=%s succeeded=%d active=%d results=%d\n", m.Status, m.Subs.Succeeded, m.Subs.Running, len(results))
	return nil
}
