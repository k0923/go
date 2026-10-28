package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type Job struct{}
type Unit string

func (u Unit) Key() string {
	return string(u)
}
func (Job) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return tflow.Page{Items: []tflow.SubTask{Unit("left"), Unit("right")}}, nil
}

type barrierExecutor struct {
	total   int
	ready   chan string
	release chan struct{}
}

func newBarrierExecutor(total int) *barrierExecutor {
	return &barrierExecutor{total: total, ready: make(chan string, total), release: make(chan struct{})}
}
func (e *barrierExecutor) Start(ctx context.Context, task tflow.SubTask, ref tflow.Execution) (tflow.DispatchResult, error) {
	fmt.Printf("worker ready: %s attempt=%d\n", task.Key(), ref.Attempt)
	select {
	case e.ready <- task.Key():
	case <-ctx.Done():
		return tflow.DispatchResult{}, ctx.Err()
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return tflow.DispatchResult{}, ctx.Err()
	}
	fmt.Printf("worker succeeded: %s\n", task.Key())
	return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
}
func (e *barrierExecutor) Resume(context.Context, tflow.SubTask, tflow.Execution, tflow.Event) (tflow.DispatchResult, error) {
	return tflow.DispatchResult{}, tflow.ErrResumeUnsupported
}
func (e *barrierExecutor) Cancel(context.Context, tflow.SubTask, tflow.Execution) error {
	return nil
}
func (e *barrierExecutor) waitAll(ctx context.Context) error {
	for i := 0; i < e.total; i++ {
		select {
		case <-e.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	close(e.release)
	return nil
}
func main() {
	if err := run(); err != nil {
		panic(err)
	}
}
func run() error {
	ctx := context.Background()
	// The repository is shared; each process opens a new handle for the same ID.
	repository := memory.New()
	storage := repository.ForMain("parallel-recovery")
	submitter, err := tflow.New(storage, tflow.Options{})
	if err != nil {
		return err
	}
	main, err := submitter.Submit(ctx, Job{}, tflow.SubmitOptions{ConcurrencyLimit: 2})
	if err != nil {
		return err
	}
	fmt.Printf("submitted: %s running=%d succeeded=%d\n", main.Status, main.Subs.Running, main.Subs.Succeeded)
	broker := &memory.FakeDispatcher{}
	recovered, err := tflow.New(repository.ForMain("parallel-recovery"), tflow.Options{Dispatcher: broker})
	if err != nil {
		return err
	}
	if _, err := recovered.Drain(ctx, 100); err != nil {
		return err
	}
	executor := newBarrierExecutor(2)
	consumer, err := tflow.New(repository.ForMain("parallel-recovery"), tflow.Options{})
	if err != nil {
		return err
	}
	commands := broker.Commands()
	var wg sync.WaitGroup
	for _, command := range commands {
		if command.Kind != tflow.StartSub {
			continue
		}
		wg.Add(1)
		go func(command tflow.Command) {
			defer wg.Done()
			if err := consumer.Handle(ctx, command, executor); err != nil {
				panic(err)
			}
		}(command)
	}
	if err := executor.waitAll(ctx); err != nil {
		return err
	}
	wg.Wait()
	current, err := consumer.GetMain(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("completed after recovery: %s succeeded=%d\n", current.Status, current.Subs.Succeeded)
	return nil
}
