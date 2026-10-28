// This demo rehydrates business DTOs and switches Engine/client instances over
// a shared memory Store. For process restarts replace it with a shared database.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type WakeQueue interface {
	Enqueue(context.Context, string, string, string, tflow.MainStatus) error
}

type Batch struct {
	ParentRunID string
	WaitKey     string
	Queue       WakeQueue `json:"-"`
}

type Unit string

func (u Unit) Key() string { return string(u) }

func (*Batch) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return tflow.Page{Items: []tflow.SubTask{Unit("row-1")}}, nil
}

func (b *Batch) OnTerminated(ctx context.Context, event tflow.MainTermination) error {
	return b.Queue.Enqueue(ctx, event.ID, b.ParentRunID, b.WaitKey, event.Main.Status)
}

type DemoQueue struct {
	Name     string
	FailOnce bool
	Inbox    map[string]string
}

func (q *DemoQueue) Enqueue(ctx context.Context, id, run, wait string, status tflow.MainStatus) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Printf("notify via %s: run=%s wait=%s event=%s\n", q.Name, run, wait, id)
	if q.FailOnce {
		q.FailOnce = false
		return errors.New("wake queue temporarily unavailable")
	}
	// Real adapters persist the wake and deduplication together before returning.
	q.Inbox[id] = fmt.Sprintf("%s/%s=%s", run, wait, status)
	return nil
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	ctx := context.Background()
	inbox := make(map[string]string)
	var queue WakeQueue = &DemoQueue{Name: "original-client", Inbox: inbox}
	storage := memory.New(memory.WithMainTaskCopy(func(task tflow.MainTask) tflow.MainTask {
		// A database adapter similarly saves DTO fields and reconstructs clients.
		data, err := json.Marshal(task.(*Batch))
		if err != nil {
			panic(err)
		}
		var restored Batch
		if err := json.Unmarshal(data, &restored); err != nil {
			panic(err)
		}
		restored.Queue = queue
		return &restored
	})).ForMain("batch-42")
	original, err := tflow.New(storage, tflow.Options{})
	if err != nil {
		return err
	}
	_, err = original.Submit(ctx, &Batch{ParentRunID: "workflow-42", WaitKey: "import-step", Queue: queue}, tflow.SubmitOptions{
		ConcurrencyLimit: 1,
	})
	if err != nil {
		return err
	}
	subs, err := original.ListSubTasks(ctx, tflow.SubQuery{Statuses: []tflow.SubStatus{tflow.SubRunning}})
	if err != nil {
		return err
	}
	// Simulate a remote executor's terminal callback before notification delivery.
	if _, err := original.Report(ctx, tflow.Report{Execution: subs[0].Execution("batch-42"), Status: tflow.SubSucceeded}); err != nil {
		return err
	}
	fmt.Println("terminal state and notification committed; replacing engine and client")
	queue = &DemoQueue{Name: "restored-client", FailOnce: true, Inbox: inbox}
	recovered, err := tflow.New(storage, tflow.Options{})
	if err != nil {
		return err
	}
	if _, err := recovered.Drain(ctx, 100); err == nil {
		return errors.New("expected the first notification to fail")
	} else {
		fmt.Printf("retry retained: %v\n", err)
	}
	if _, err := recovered.Drain(ctx, 100); err != nil {
		return err
	}
	if len(inbox) != 1 {
		return fmt.Errorf("expected one idempotent wake, got %d", len(inbox))
	}
	fmt.Println("recovered: one wake recorded, no TerminationHandler registration required")
	return nil
}
