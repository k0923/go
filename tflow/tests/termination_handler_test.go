package tflow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type notificationSink struct {
	fail    bool
	panics  bool
	calls   int
	records map[string]string
}

type routedMain struct {
	route string
	sink  *notificationSink
}

type reroutedPlan struct {
	tflow.Page
	next *routedMain
}

func (*routedMain) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return tflow.Page{Items: []tflow.SubTask{unit("unit")}}, nil
}

func (m *routedMain) OnTerminated(_ context.Context, event tflow.MainTermination) error {
	m.sink.calls++
	if m.sink.panics {
		panic("notification panic")
	}
	if m.sink.fail {
		return errors.New("wake queue unavailable")
	}
	m.sink.records[event.ID] = m.route
	return nil
}

func TestTaskTerminationRoutingSurvivesNewEngineAndHistoricalRerun(t *testing.T) {
	original := &notificationSink{records: make(map[string]string)}
	current := original
	copies := 0
	repository := memory.New(memory.WithClock(deliveryClock), memory.WithMainTaskCopy(func(task tflow.MainTask) tflow.MainTask {
		m := task.(*routedMain)
		copies++
		return &routedMain{route: m.route, sink: current}
	}), memory.WithPlanCommit(func(task tflow.MainTask, plan tflow.Plan) (tflow.MainTask, error) {
		if next, ok := plan.(reroutedPlan); ok {
			return next.next, nil
		}
		return task, nil
	}))
	storage := repository.ForMain("job")
	first, _ := tflow.New(storage, tflow.Options{})
	if _, err := first.Submit(ctx, &routedMain{route: "workflow-A/wait-1", sink: original}, spec(1)); err != nil {
		t.Fatal(err)
	}
	subs, _ := first.ListSubTasks(ctx, tflow.SubQuery{})
	if _, err := first.Report(ctx, tflow.Report{Execution: subs[0].Execution("job"), Status: tflow.SubFailed}); err != nil {
		t.Fatal(err)
	}
	old := commandsOf(t, storage, tflow.MainTerminated)[0]
	replacement := &notificationSink{fail: true, records: make(map[string]string)}
	current = replacement
	recovered, _ := tflow.New(repository.ForMain("job"), tflow.Options{
		TerminationHandler: tflow.TerminationHandlerFunc(func(context.Context, tflow.MainTermination) error {
			t.Fatal("fallback handler must not duplicate a task-routed notification")
			return nil
		}),
	})
	if _, err := recovered.Drain(ctx, 100); err == nil {
		t.Fatal("notification failure ignored")
	}
	if original.calls != 0 || replacement.calls != 1 || len(commandsOf(t, storage, tflow.MainTerminated)) != 1 {
		t.Fatal("notification used original instance or lost retry intent")
	}
	// Change only the current task's destination. The old command must retain A.
	if err := storage.WithTx(ctx, func(tx tflow.Tx) error {
		m, err := tx.Main()
		if err != nil {
			return err
		}
		version := m.Version
		m.Version++
		return tx.CommitPlan(version, m, reroutedPlan{next: &routedMain{route: "workflow-B/wait-2", sink: replacement}})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.Rerun(ctx, tflow.RerunFailed); err != nil {
		t.Fatal(err)
	}
	subs, _ = recovered.ListSubTasks(ctx, tflow.SubQuery{})
	if _, err := recovered.Report(ctx, tflow.Report{Execution: subs[0].Execution("job"), Status: tflow.SubSucceeded}); err != nil {
		t.Fatal(err)
	}
	replacement.fail = false
	deliveryClock.advance()
	if _, err := recovered.Drain(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if replacement.records[old.ID] != "workflow-A/wait-1" || len(replacement.records) != 2 || original.calls != 0 || copies < 2 {
		t.Fatalf("routing or identity lost: %+v copies=%d", replacement.records, copies)
	}
	for id, route := range replacement.records {
		if id != old.ID && route != "workflow-B/wait-2" {
			t.Fatalf("new notification retained old routing: %s", route)
		}
	}
	if err := recovered.Handle(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	if len(replacement.records) != 2 {
		t.Fatal("duplicate notification changed stable identity")
	}
}

func TestTaskTerminationHandlerPanicRetainsNotification(t *testing.T) {
	sink := &notificationSink{panics: true, records: make(map[string]string)}
	storage := memory.New(memory.WithClock(deliveryClock)).ForMain("job")
	e, _ := tflow.New(storage, tflow.Options{})
	if _, err := e.Submit(ctx, &routedMain{route: "workflow/wait", sink: sink}, spec(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	command := commandsOf(t, storage, tflow.MainTerminated)[0]
	dispatcher := tflow.DispatcherFunc(func(ctx context.Context, command tflow.Command) error {
		if command.Kind == tflow.CancelSub {
			return nil
		}
		return e.Handle(ctx, command, nil)
	})
	if _, err := e.DispatchPending(ctx, 100, dispatcher); err == nil {
		t.Fatal("handler panic ignored")
	}
	if len(commandsOf(t, storage, tflow.MainTerminated)) != 1 {
		t.Fatal("notification discarded after panic")
	}
	sink.panics = false
	deliveryClock.advance()
	if _, err := e.Drain(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if sink.records[command.ID] != "workflow/wait" {
		t.Fatal("retry did not use historical routing")
	}
}
