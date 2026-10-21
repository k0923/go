package fsm_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/k0923/go/fsm"
)

func TestConcurrentTransitions(t *testing.T) {
	event := fsm.NewEvent[orderState, int]("increment")
	b := fsm.NewBuilder[orderState, *int]()
	var m *fsm.Machine[orderState, *int]
	b.On(event).From(pending).To(pending).
		Action(func(_ context.Context, count *int, delta int) error {
			// State must remain callable from a callback without deadlocking.
			if m.State() != pending {
				return errors.New("unexpected state")
			}
			*count += delta
			return nil
		})
	count := 0
	m = mustMachine(t, mustBuild(t, b), pending, &count)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if err := m.Fire(ctx, event, 1); err != nil {
				t.Errorf("Fire: %v", err)
			}
		})
	}
	wg.Wait()
	if count != 100 {
		t.Fatalf("lost updates: %d", count)
	}
	t.Run("queued calls recheck committed state", func(t *testing.T) {
		var actions int
		b := fsm.NewBuilder[orderState, struct{}]()
		b.On(event).From(pending).To(paid).
			Action(func(context.Context, struct{}, int) error {
				actions++
				return nil
			})
		m := mustMachine(t, mustBuild(t, b), pending, struct{}{})
		results := make(chan error, 50)
		start := make(chan struct{})
		for range cap(results) {
			wg.Go(func() {
				<-start
				results <- m.Fire(ctx, event, 1)
			})
		}
		close(start)
		wg.Wait()
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			} else if !errors.Is(err, fsm.ErrTransitionNotAllowed) {
				t.Fatalf("unexpected queued transition error: %v", err)
			}
		}
		if successes != 1 || actions != 1 || m.State() != paid {
			t.Fatalf("successes=%d actions=%d state=%v", successes, actions, m.State())
		}
	})
}

func TestCancellationAndSharedDefinition(t *testing.T) {
	event := fsm.NewEvent[orderState, struct{}]("finish")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	b := fsm.NewBuilder[orderState, bool]()
	b.On(event).From(pending).To(paid).
		Action(func(_ context.Context, block bool, _ struct{}) error {
			if block {
				close(entered)
				<-release
			}
			return nil
		})
	d := mustBuild(t, b)
	m := mustMachine(t, d, pending, true)
	done := make(chan error, 1)
	go func() { done <- m.Fire(context.Background(), event, struct{}{}) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("action did not start")
	}
	if m.State() != pending {
		t.Fatal("state committed before action completed")
	}
	// Another machine sharing the definition must not wait for this action.
	other := mustMachine(t, d, pending, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := other.Fire(ctx, event, struct{}{}); err != nil {
		t.Fatalf("independent machine blocked: %v", err)
	}
	// Waiting for the same machine respects cancellation.
	waitCtx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if err := m.Fire(waitCtx, event, struct{}{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait did not respect deadline: %v", err)
	}
	unblock()
	select {
	case err := <-done:
		if err != nil || m.State() != paid {
			t.Fatalf("first transition failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("action did not finish")
	}
}

func TestCancellationAroundCallbacks(t *testing.T) {
	for _, phase := range []string{"before", "guard", "action-success", "action-error"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			event := fsm.NewEvent[orderState, struct{}]("finish")
			b := fsm.NewBuilder[orderState, struct{}]()
			called := false
			b.On(event).From(pending).To(paid).
				Guard(func(context.Context, struct{}, struct{}) (bool, error) {
					if phase == "guard" {
						cancel()
					}
					return true, nil
				}).
				Action(func(context.Context, struct{}, struct{}) error {
					called = true
					cancel()
					if phase == "action-error" {
						return ctx.Err()
					}
					return nil
				})
			m := mustMachine(t, mustBuild(t, b), pending, struct{}{})
			if phase == "before" {
				cancel()
			}
			err := m.Fire(ctx, event, struct{}{})
			if phase == "action-success" {
				if err != nil || m.State() != paid || !called {
					t.Fatalf("successful action was not committed: %v", err)
				}
			} else {
				if !errors.Is(err, context.Canceled) || m.State() != pending {
					t.Fatalf("cancelled transition changed state: %v", err)
				}
				if called != (phase == "action-error") {
					t.Fatalf("unexpected action execution: %v", called)
				}
			}
		})
	}
}

func TestPanicDoesNotCommitOrLockMachine(t *testing.T) {
	for _, phase := range []string{"guard", "action"} {
		t.Run(phase, func(t *testing.T) {
			event := fsm.NewEvent[orderState, bool]("finish")
			b := fsm.NewBuilder[orderState, struct{}]()
			b.On(event).From(pending).To(paid).
				Guard(func(_ context.Context, _ struct{}, crash bool) (bool, error) {
					if crash && phase == "guard" {
						panic("callback panic")
					}
					return true, nil
				}).
				Action(func(_ context.Context, _ struct{}, crash bool) error {
					if crash && phase == "action" {
						panic("callback panic")
					}
					return nil
				})
			m := mustMachine(t, mustBuild(t, b), pending, struct{}{})
			func() {
				defer func() {
					if got := recover(); got != "callback panic" {
						t.Errorf("panic was not propagated: %v", got)
					}
				}()
				_ = m.Fire(context.Background(), event, true)
			}()
			if m.State() != pending {
				t.Fatal("panic committed target state")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := m.Fire(ctx, event, false); err != nil || m.State() != paid {
				t.Fatalf("machine remained locked after panic: %v", err)
			}
		})
	}
}
