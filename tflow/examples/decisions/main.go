package main

import (
	"context"
	"fmt"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/examples/internal/executor"
	"github.com/k0923/go/tflow/memory"
)

type Import struct{ AllowedFailures int64 }
type Row string

func (r Row) Key() string { return string(r) }

func (*Import) Plan(_ context.Context, req tflow.PlanRequest) (tflow.Plan, error) {
	if req.Limit() < 2 {
		return nil, tflow.Permanent(fmt.Errorf("this fixed page needs a limit of at least 2"))
	}
	return tflow.Page{Items: []tflow.SubTask{Row("valid"), Row("invalid")}}, nil
}

func (task *Import) DecideMain(_ context.Context, view tflow.MainDecisionView) (tflow.MainDecision, error) {
	if view.ReadyToComplete() {
		if view.Summary().Failed <= task.AllowedFailures {
			return tflow.MainDecision{Action: tflow.Succeed, Reason: "within allowed failure limit"}, nil
		}
		return tflow.MainDecision{Action: tflow.Fail, Reason: "too many failed rows"}, nil
	}
	return tflow.MainDecision{Action: tflow.Continue}, nil
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	store := memory.New().ForMain("tolerant-import")
	runner, err := tflow.New(store, tflow.Options{
		Executor: executor.Func(func(_ context.Context, task tflow.SubTask, _ tflow.Execution) (tflow.DispatchResult, error) {
			if task.Key() == "invalid" {
				return tflow.DispatchResult{Status: tflow.ExecutionFailed, Error: "invalid row"}, nil
			}
			return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
		}),
	})
	if err != nil {
		return err
	}
	progress, err := runner.Submit(context.Background(), &Import{AllowedFailures: 1}, tflow.SubmitOptions{ConcurrencyLimit: 2})
	if err != nil {
		return err
	}
	fmt.Printf("main=%s succeeded=%d failed=%d reason=%s\n",
		progress.Status, progress.Subs.Succeeded, progress.Subs.Failed, progress.Error)
	return nil
}
