package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type businessMain struct{ marker int }

func (*businessMain) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return nil, tflow.ErrInvalid
}

type businessSub struct{ value int }

func (*businessSub) Key() string { return "unit" }

type businessEvent struct{ data int }

func (*businessEvent) Key() string { return "response" }

func TestAdapterCopyHooksIsolateBusinessObjects(t *testing.T) {
	s := memory.New(
		memory.WithMainTaskCopy(func(task tflow.MainTask) tflow.MainTask { copy := *task.(*businessMain); return &copy }),
		memory.WithSubTaskCopy(func(task tflow.SubTask) tflow.SubTask { copy := *task.(*businessSub); return &copy }),
		memory.WithEventCopy(func(event tflow.Event) tflow.Event { copy := *event.(*businessEvent); return &copy }),
	).ForMain("business")
	ctx := context.Background()
	task := &businessMain{marker: 1}
	m, _ := tflow.NewMain(task, tflow.SubmitOptions{ConcurrencyLimit: 1}, time.Time{})
	m.DispatchCursor, m.Subs.Running, m.DispatchExhausted = 1, 1, true
	unit := &businessSub{value: 2}
	sub := tflow.SubRecord{Task: unit, SubState: tflow.SubState{Status: tflow.SubRunning, Attempt: 1, Version: 1, Key: unit.Key()}}
	input := &businessEvent{data: 3}
	command := tflow.NewCommand(tflow.ResumeSub, s.MainID(), sub, input)
	if err := s.WithTx(ctx, func(tx tflow.Tx) error {
		if err := tx.CreateMain(m); err != nil {
			return err
		}
		if err := tx.CreateSubs([]tflow.SubRecord{sub}); err != nil {
			return err
		}
		return tx.PutCommand(command)
	}); err != nil {
		t.Fatal(err)
	}
	task.marker, unit.value, input.data = 99, 99, 99
	current, _ := s.GetMain(ctx)
	current.Task.(*businessMain).marker = 50
	abort := errors.New("abort")
	if err := s.WithTx(ctx, func(tx tflow.Tx) error {
		m, _ := tx.Main()
		m.Task.(*businessMain).marker = 100
		m.Version++
		if err := tx.SaveMain(m.Version-1, m.MainState); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	current, _ = s.GetMain(ctx)
	storedSub, _ := s.GetSub(ctx, sub.Key)
	pending, _ := s.PendingCommands(ctx)
	if current.Task.(*businessMain).marker != 1 || storedSub.Task.(*businessSub).value != 2 || pending[0].Command.Event.(*businessEvent).data != 3 {
		t.Fatal("adapter failed to isolate user-owned objects")
	}
}
