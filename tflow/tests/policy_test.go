package tflow_test

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
)

func limits(src *source) []int {
	src.state.mu.Lock()
	defer src.state.mu.Unlock()
	return append([]int(nil), src.state.limits...)
}

func planCounts(src *source) []int {
	src.state.mu.Lock()
	defer src.state.mu.Unlock()
	return append([]int(nil), src.state.counts...)
}

func TestPlanningStrategiesFor22UnitsAnd5Slots(t *testing.T) {
	fillLimits := []int{5}
	for i := 0; i < 17; i++ {
		fillLimits = append(fillLimits, 1)
	}
	for _, test := range []struct {
		name    string
		policy  tflow.PlanningPolicy
		running int64
		pending int64
		cursor  int64
		limits  []int
		counts  []int
	}{
		{"wave", tflow.WavePolicy{}, 4, 0, 5, []int{5, 5, 5, 5, 5}, []int{5, 5, 5, 5, 2}},
		{"buffer", tflow.BufferPolicy{TargetPending: 4, MaxPlanSize: 5}, 5, 4, 10, []int{5, 5, 5, 5, 5}, []int{5, 5, 5, 5, 2}},
		{"fill", tflow.FillPolicy{}, 5, 0, 6, fillLimits, fillLimits},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage, src := newStore(), newSource(22)
			e := newEngine(t, storage, src, tflow.Options{})
			input := spec(5)
			input.PlanningPolicy = test.policy
			m, err := e.submit(ctx, input)
			if err != nil || m.Subs.Running != 5 || m.Subs.Pending != 0 || m.DispatchCursor != 5 {
				t.Fatalf("initial wave: %+v %v", m, err)
			}
			r := report(t, e, list(t, storage, tflow.SubRunning)[0], tflow.SubSucceeded)
			if r.Main.Subs.Running != test.running || r.Main.Subs.Pending != test.pending || r.Main.DispatchCursor != test.cursor {
				t.Fatalf("first completion: %+v", r.Main)
			}
			maxPending := r.Main.Subs.Pending
			// Serial completions make the number and size of Plan calls deterministic.
			for i := 0; i < 21; i++ {
				r = report(t, e, list(t, storage, tflow.SubRunning)[0], tflow.SubSucceeded)
				maxPending = max(maxPending, r.Main.Subs.Pending)
			}
			if r.Main.Status != tflow.MainSucceeded || r.Main.Subs.Succeeded != 22 ||
				r.Main.Subs.Pending != 0 || !reflect.DeepEqual(limits(src), test.limits) ||
				!reflect.DeepEqual(planCounts(src), test.counts) {
				t.Fatalf("final: %+v limits=%v counts=%v", r.Main, limits(src), planCounts(src))
			}
			if test.name == "buffer" && maxPending > 4 {
				t.Fatalf("unbounded cache: %d", maxPending)
			}
		})
	}
}

func TestWaveWaitsForRunningBeforeActivatingPending(t *testing.T) {
	storage, src := newStore(), newSource(6)
	m, err := tflow.NewMain(src, tflow.SubmitOptions{
		ConcurrencyLimit: 2,
		PlanningPolicy:   tflow.WavePolicy{},
	}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	page := &batch{updated: &source{total: 6, position: 6, state: src.state}}
	for i := int64(0); i < 6; i++ {
		page.tasks = append(page.tasks, &rowTask{row: i})
	}
	m, subs, err := tflow.ApplyPlan(m, page, 6, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WithTx(ctx, func(tx tflow.Tx) error {
		if err := tx.CommitPlan(0, m, page); err != nil {
			return err
		}
		return tx.CreateSubs(subs)
	}); err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, storage, nil, tflow.Options{})
	r := report(t, e, subs[0], tflow.SubSucceeded)
	if r.Main.Subs.Running != 1 || r.Main.Subs.Pending != 4 {
		t.Fatalf("wave activated pending early: %+v", r.Main)
	}
	r = report(t, e, subs[1], tflow.SubSucceeded)
	if r.Main.Subs.Running != 2 || r.Main.Subs.Pending != 2 || len(limits(src)) != 0 {
		t.Fatalf("next pending wave: %+v", r.Main)
	}
	if m := finishAll(t, e, storage); m.Subs.Succeeded != 6 {
		t.Fatalf("pending waves did not converge: %+v", m)
	}
}

func TestRecoveryUsesPersistedPolicy(t *testing.T) {
	storage, src := newStore(), newSource(22)
	original := newEngine(t, storage, src, tflow.Options{PlanningPolicy: tflow.WavePolicy{}})
	_, err := original.submit(ctx, spec(5))
	if err != nil {
		t.Fatal(err)
	}
	// This engine defaults to Fill, but the persisted main remains Wave.
	recovered := newEngine(t, storage, nil, tflow.Options{})
	r := report(t, recovered, list(t, storage, tflow.SubRunning)[0], tflow.SubSucceeded)
	if r.Main.Subs.Running != 4 || r.Main.DispatchCursor != 5 || len(limits(src)) != 1 {
		t.Fatalf("recovery replaced policy: %+v", r.Main)
	}
	record, _ := storage.GetMain(ctx)
	if _, ok := record.PlanningPolicy.(tflow.WavePolicy); !ok {
		t.Fatal("engine default was not persisted")
	}
}

func TestConcurrentBufferReportsPreserveCacheAndQuota(t *testing.T) {
	storage, src := newStore(), newSource(30)
	e := newEngine(t, storage, src, tflow.Options{})
	input := spec(5)
	input.PlanningPolicy = tflow.BufferPolicy{TargetPending: 4, MaxPlanSize: 5}
	if _, err := e.submit(ctx, input); err != nil {
		t.Fatal(err)
	}
	subs := list(t, storage, tflow.SubRunning)
	other := newEngine(t, storage, nil, tflow.Options{})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		sub := subs[i%5]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := other.Report(ctx, tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	m, err := storage.GetMain(ctx)
	if err != nil || m.Subs.Succeeded != 5 || m.Subs.Running != 5 || m.DispatchCursor != 10 || m.Subs.Pending != 0 {
		t.Fatalf("concurrent buffer: %+v %v", m, err)
	}
	if m := finishAll(t, e, storage); m.Subs.Succeeded != 30 {
		t.Fatalf("concurrent cache did not converge: %+v", m)
	}
}

type invalidPolicy struct{}

func (invalidPolicy) Decide(tflow.PlanningView) tflow.PlanningDecision {
	return tflow.PlanningDecision{ActivateLimit: -1}
}

func TestInvalidPlanningPolicyDoesNotCreateMain(t *testing.T) {
	for _, policy := range []tflow.PlanningPolicy{
		tflow.WavePolicy{Size: -1}, tflow.BufferPolicy{MinPending: -1}, tflow.BufferPolicy{MinPending: 2, TargetPending: 1},
	} {
		if _, err := tflow.New(newStore(), tflow.Options{PlanningPolicy: policy}); !errors.Is(err, tflow.ErrInvalid) {
			t.Fatalf("invalid config accepted: %T %v", policy, err)
		}
	}
	storage := newStore()
	e := newEngine(t, storage, newSource(1), tflow.Options{PlanningPolicy: invalidPolicy{}})
	if _, err := e.submit(ctx, spec(1)); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatalf("invalid decision accepted: %v", err)
	}
	if _, err := storage.GetMain(ctx); !errors.Is(err, tflow.ErrNotFound) {
		t.Fatal("invalid policy created main")
	}
}

func TestPositiveBufferWatermarkPrefetchesAndRefillsAtThreshold(t *testing.T) {
	for _, watermark := range []int64{0, 2} {
		t.Run(fmt.Sprint(watermark), func(t *testing.T) {
			s, src := newStore(), newSource(30)
			e := newEngine(t, s, src, tflow.Options{})
			input := spec(5)
			input.PlanningPolicy = tflow.BufferPolicy{MinPending: watermark, TargetPending: 4, MaxPlanSize: 5}
			m, err := e.submit(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			wantPending := int64(0)
			if watermark > 0 {
				wantPending = 4
			}
			if m.Subs.Pending != wantPending || m.Subs.Running != 5 {
				t.Fatalf("initial cache: %+v", m)
			}
			if watermark > 0 {
				first := report(t, e, list(t, s, tflow.SubRunning)[0], tflow.SubSucceeded)
				if first.Main.Subs.Pending != 3 {
					t.Fatalf("refilled above threshold: %+v", first.Main)
				}
				second := report(t, e, list(t, s, tflow.SubRunning)[0], tflow.SubSucceeded)
				if second.Main.Subs.Pending != 4 || second.Main.Subs.Running != 5 {
					t.Fatalf("did not refill at threshold: %+v", second.Main)
				}
			}
			if final := finishAll(t, e, s); final.Subs.Succeeded != 30 || final.Subs.Pending != 0 {
				t.Fatalf("did not converge: %+v", final)
			}
		})
	}
}

func TestPositiveBufferConcurrentReportsAcrossRecoveredEngines(t *testing.T) {
	s, src := newStore(), newSource(40)
	e := newEngine(t, s, src, tflow.Options{})
	input := spec(5)
	input.PlanningPolicy = tflow.BufferPolicy{MinPending: 2, TargetPending: 4, MaxPlanSize: 5}
	if _, err := e.submit(ctx, input); err != nil {
		t.Fatal(err)
	}
	subs := list(t, s, tflow.SubRunning)
	recovered := newEngine(t, s, nil, tflow.Options{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		sub := subs[i%len(subs)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := recovered.Report(ctx, tflow.Report{Execution: sub.Execution("job"), Status: tflow.SubSucceeded}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	m, _ := recovered.GetMain(ctx)
	if m.Subs.Succeeded != 5 || m.Subs.Running != 5 || m.Subs.Pending < 0 || m.Subs.Pending > 4 {
		t.Fatalf("cache or quota drift: %+v", m)
	}
	if final := finishAll(t, recovered, s); final.Subs.Succeeded != 40 {
		t.Fatalf("recovered policy did not converge: %+v", final)
	}
}
