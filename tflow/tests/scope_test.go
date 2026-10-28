package tflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

func TestRepositoryReopensOneLogicalMainWithoutSharingOtherMains(t *testing.T) {
	repository := memory.New()
	stores := make(map[string]*memory.Store)
	engines := make(map[string]*tflow.Engine)
	for _, id := range []string{"a", "b"} {
		store := repository.ForMain(id)
		engine, err := tflow.New(store, tflow.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Submit(ctx, &staticMain{name: "same-key"}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err != nil {
			t.Fatal(err)
		}
		stores[id], engines[id] = store, engine
	}
	opened, err := repository.Open(ctx, "a")
	if err != nil || opened.MainID() != "a" || opened == stores["a"] {
		t.Fatalf("open did not return an independent bound handle: %v %v", opened, err)
	}
	recovered, _ := tflow.New(opened, tflow.Options{})
	subs, _ := recovered.ListSubTasks(ctx, tflow.SubQuery{})
	ref := subs[0].Execution(recovered.MainID())
	if ref.SubKey != "same-key" {
		t.Fatalf("local key was replaced by a global ID: %+v", ref)
	}
	if _, err := engines["b"].Report(ctx, tflow.Report{Execution: ref, Status: tflow.SubSucceeded}); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatalf("cross-scope report accepted: %v", err)
	}
	if err := engines["b"].Resume(ctx, ref, event{id: "wake"}); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatalf("cross-scope resume accepted: %v", err)
	}
	commands, _ := stores["a"].PendingCommands(ctx)
	for _, c := range commands {
		if err := engines["b"].Handle(ctx, c.Command, execution{}); !errors.Is(err, tflow.ErrInvalid) {
			t.Fatalf("cross-scope command accepted: %s %v", c.Command.Kind, err)
		}
	}
	if _, err := recovered.Report(ctx, tflow.Report{Execution: ref, Status: tflow.SubSucceeded}); err != nil {
		t.Fatal(err)
	}
	a, _ := engines["a"].GetMain(ctx)
	b, _ := engines["b"].GetMain(ctx)
	if a.Status != tflow.MainSucceeded || b.Status != tflow.MainRunning || b.Subs.Succeeded != 0 {
		t.Fatalf("scope state leaked: a=%+v b=%+v", a, b)
	}
	if _, err := repository.Open(ctx, ""); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repository.Open(canceled, "a"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := tflow.New(repository.ForMain(""), tflow.Options{}); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatal("empty scope accepted")
	}
}

func TestBoundHandlesShareLeasesAndRejectAnotherScopesDelivery(t *testing.T) {
	repository := memory.New()
	a, secondA, b := repository.ForMain("a"), repository.ForMain("a"), repository.ForMain("b")
	for _, store := range []*memory.Store{a, b} {
		engine, _ := tflow.New(store, tflow.Options{})
		if _, err := engine.Submit(ctx, &staticMain{name: "unit"}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := a.ClaimCommand(ctx, time.Minute)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	if err := b.AckCommand(ctx, *first); !errors.Is(err, tflow.ErrDeliveryLeaseLost) {
		t.Fatalf("another scope acked delivery: %v", err)
	}
	if err := b.ReleaseCommand(ctx, *first, 0, "wrong scope"); !errors.Is(err, tflow.ErrDeliveryLeaseLost) {
		t.Fatalf("another scope released delivery: %v", err)
	}
	next, err := secondA.ClaimCommand(ctx, time.Minute)
	if err != nil || (next != nil && next.Command.ID == first.Command.ID) {
		t.Fatal("independent handles leased the same command")
	}
	if err := secondA.AckCommand(ctx, *first); err != nil {
		t.Fatalf("reopened scope could not acknowledge original lease: %v", err)
	}
}

func TestTransactionsOnDifferentMainTasksDoNotShareALock(t *testing.T) {
	repository := memory.New()
	a, b := repository.ForMain("a"), repository.ForMain("b")
	for _, store := range []*memory.Store{a, b} {
		engine, _ := tflow.New(store, tflow.Options{})
		if _, err := engine.Submit(ctx, &staticMain{name: "unit"}, tflow.SubmitOptions{ConcurrencyLimit: 1}); err != nil {
			t.Fatal(err)
		}
	}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- a.WithTx(ctx, func(tflow.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	read := make(chan error, 1)
	go func() { _, err := b.GetMain(ctx); read <- err }()
	var failure string
	select {
	case err := <-read:
		if err != nil {
			failure = err.Error()
		}
	case <-time.After(time.Second):
		failure = "another main's transaction blocked the read"
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if failure != "" {
		t.Fatal(failure)
	}
}

func TestStateUpdatesPreserveBusinessTaskAndPersistentConfig(t *testing.T) {
	store := memory.New().ForMain("state")
	task := &staticMain{name: "unit"}
	engine, _ := tflow.New(store, tflow.Options{PlanningPolicy: tflow.WavePolicy{}})
	if _, err := engine.Submit(ctx, task, tflow.SubmitOptions{ConcurrencyLimit: 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(ctx, func(tx tflow.Tx) error {
		m, _ := tx.Main()
		expected := m.Version
		m.Task = &secondMain{}
		m.ConcurrencyLimit = 100
		m.PlanningPolicy = tflow.FillPolicy{}
		m.Version++
		m.Error = "state change"
		return tx.SaveMain(expected, m.MainState)
	}); err != nil {
		t.Fatal(err)
	}
	record, _ := store.GetMain(ctx)
	if record.Task != task || record.ConcurrencyLimit != 2 || record.Error != "state change" {
		t.Fatalf("state write replaced business/config: %+v", record)
	}
	if _, ok := record.PlanningPolicy.(tflow.WavePolicy); !ok {
		t.Fatal("state write replaced persisted policy")
	}
}

type malformedMain struct{}

func (malformedMain) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return tflow.Page{Items: []tflow.SubTask{unit("duplicate"), unit("duplicate")}}, nil
}

func TestPlanningErrorUsesBoundMainIdentity(t *testing.T) {
	engine, _ := tflow.New(memory.New().ForMain("diagnostic"), tflow.Options{})
	_, err := engine.Submit(ctx, malformedMain{}, tflow.SubmitOptions{ConcurrencyLimit: 2})
	var detail *tflow.PlanError
	if !errors.Is(err, tflow.ErrInvalidPlan) || !errors.As(err, &detail) || detail.MainID != "diagnostic" {
		t.Fatalf("bound error context lost: %+v %v", detail, err)
	}
	if _, err := engine.GetMain(ctx); !errors.Is(err, tflow.ErrNotFound) {
		t.Fatal("invalid initial plan created a main")
	}
}
