package main

import (
	"context"
	"fmt"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type Job struct{}
type Unit struct{}

func (Unit) Key() string { return "unit-1" }

type JobPlan struct{ task *Job }

func (p JobPlan) Tasks() []tflow.SubTask { return []tflow.SubTask{Unit{}} }
func (p JobPlan) HasMore() bool          { return false }
func (j *Job) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return JobPlan{task: j}, nil
}

type Executor struct{}

func (Executor) Start(_ context.Context, task tflow.SubTask, ref tflow.Execution) (tflow.DispatchResult, error) {
	fmt.Printf("execute: %s attempt=%d\n", task.Key(), ref.Attempt)
	return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
}
func (Executor) Resume(context.Context, tflow.SubTask, tflow.Execution, tflow.Event) (tflow.DispatchResult, error) {
	return tflow.DispatchResult{}, tflow.ErrInvalid
}
func (Executor) Cancel(context.Context, tflow.SubTask, tflow.Execution) error { return nil }

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	ctx := context.Background()
	storage := memory.New().ForMain("job")
	runner, err := tflow.New(storage, tflow.Options{
		TerminationHandler: tflow.TerminationHandlerFunc(func(_ context.Context, event tflow.MainTermination) error {
			// Persist deduplication by event.ID in a real observer.
			fmt.Printf("notification: %s succeeded=%d\n", event.Main.Status, event.Main.Subs.Succeeded)
			return nil
		}),
	})
	if err != nil {
		return err
	}
	_, err = runner.Submit(ctx, &Job{}, tflow.SubmitOptions{ConcurrencyLimit: 1})
	if err != nil {
		return err
	}
	// FakeDispatcher stands in for a reliable broker here. Production transport
	// persists/accepts each command before Dispatch returns nil.
	transport := &memory.FakeDispatcher{}
	consumed := 0
	for {
		if _, err := runner.DispatchPending(ctx, 100, transport); err != nil {
			return err
		}
		queued := transport.Commands()
		if consumed == len(queued) {
			break
		}
		for _, command := range queued[consumed:] {
			fmt.Printf("consume: %s\n", command.Kind)
			if err := runner.Handle(ctx, command, Executor{}); err != nil {
				return err
			}
			// A real consumer Ack follows successful Handle, never before it.
			consumed++
		}
	}
	return nil
}
