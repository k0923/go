package main

import (
	"context"
	"fmt"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/examples/internal/executor"
	"github.com/k0923/go/tflow/memory"
)

type Job struct{}
type Unit string

func (u Unit) Key() string { return string(u) }

func (Job) Plan(_ context.Context, req tflow.PlanRequest) (tflow.Plan, error) {
	if req.Limit() < 3 {
		return nil, tflow.Permanent(fmt.Errorf("this fixed page requires a limit of at least 3"))
	}
	return tflow.Page{Items: []tflow.SubTask{Unit("a"), Unit("b"), Unit("c")}}, nil
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	ctx := context.Background()
	runner, err := tflow.New(memory.New().ForMain("demo"), tflow.Options{
		Executor: executor.Func(func(ctx context.Context, task tflow.SubTask, ref tflow.Execution) (tflow.DispatchResult, error) {
			if err := ctx.Err(); err != nil {
				return tflow.DispatchResult{}, err
			}
			fmt.Printf("execute: %s attempt=%d\n", task.Key(), ref.Attempt)
			return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
		}),
	})
	if err != nil {
		return err
	}
	main, err := runner.Submit(ctx, Job{}, tflow.SubmitOptions{ConcurrencyLimit: 3})
	if err != nil {
		return err
	}
	fmt.Printf("completed: %s succeeded=%d\n", main.Status, main.Subs.Succeeded)
	return nil
}
