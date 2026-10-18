package cflow

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTransactionCommitsAndRollsBack(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := context.Background()

	t.Run("commit keeps writes and exposes read-your-writes", func(t *testing.T) {
		err := infrastructure.Transaction(ctx, func(api Infrastructure) error {
			event := &CheckpointEvent{Key: "kept", Status: FlowStatusCompleted, StartedAt: time.Now()}
			if _, err := api.AddEvent(ctx, event); err != nil {
				return err
			}
			visible, err := api.GetEvent(ctx, "kept")
			if err != nil {
				return err
			}
			if visible == nil {
				t.Error("expected write to be visible inside the transaction")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("transaction failed: %v", err)
		}
		event, err := infrastructure.GetEvent(ctx, "kept")
		if err != nil {
			t.Fatalf("get kept failed: %v", err)
		}
		if event == nil {
			t.Fatal("expected committed event")
		}
	})

	t.Run("returned error rolls back writes", func(t *testing.T) {
		boom := errors.New("boom")
		err := infrastructure.Transaction(ctx, func(api Infrastructure) error {
			if _, err := api.AddEvent(ctx, &CheckpointEvent{Key: "rolled-back", Status: FlowStatusCompleted}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("expected boom, got %v", err)
		}
		event, err := infrastructure.GetEvent(ctx, "rolled-back")
		if err != nil {
			t.Fatalf("get rolled-back failed: %v", err)
		}
		if event != nil {
			t.Fatalf("expected rolled back event to be absent, got %+v", event)
		}
	})

	t.Run("panic rolls back writes", func(t *testing.T) {
		func() {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("expected panic to propagate")
				}
			}()
			_ = infrastructure.Transaction(ctx, func(api Infrastructure) error {
				if _, err := api.AddEvent(ctx, &CheckpointEvent{Key: "panicked", Status: FlowStatusCompleted}); err != nil {
					return err
				}
				panic("boom")
			})
		}()
		event, err := infrastructure.GetEvent(ctx, "panicked")
		if err != nil {
			t.Fatalf("get panicked failed: %v", err)
		}
		if event != nil {
			t.Fatalf("expected rolled back event to be absent, got %+v", event)
		}
	})

	t.Run("returned error rolls back lease claim", func(t *testing.T) {
		boom := errors.New("boom")
		err := infrastructure.Transaction(ctx, func(api Infrastructure) error {
			if _, err := api.ClaimLease(ctx, "tx-lease", time.Minute); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("expected boom, got %v", err)
		}
		lease, err := infrastructure.ClaimLease(ctx, "tx-lease", time.Minute)
		if err != nil {
			t.Fatalf("claim lease failed: %v", err)
		}
		if lease == nil {
			t.Fatal("expected rolled back lease to be claimable")
		}
	})

	t.Run("tx view exposes the full infrastructure surface", func(t *testing.T) {
		err := infrastructure.Transaction(ctx, func(api Infrastructure) error {
			lease, err := api.ClaimLease(ctx, "surface-lease", time.Minute)
			if err != nil {
				return err
			}
			if lease == nil {
				return errors.New("expected lease inside transaction")
			}
			renewed, err := api.RenewLease(ctx, *lease, time.Minute)
			if err != nil {
				return err
			}
			if renewed == nil {
				return errors.New("expected renewed lease inside transaction")
			}
			if _, err := api.AddEvent(ctx, &CheckpointEvent{Key: "surface", Status: FlowStatusCompleted}); err != nil {
				return err
			}
			event, err := api.GetEvent(ctx, "surface")
			if err != nil {
				return err
			}
			if event == nil {
				return errors.New("expected event inside transaction")
			}
			event.SetError(errors.New("boom"))
			if _, err := api.UpdateEvent(ctx, event); err != nil {
				return err
			}
			if err := api.Suspend(ctx, &WaitForSignalEvent{
				Key:       "surface|signal",
				Status:    FlowStatusSuspended,
				StartedAt: time.Now(),
			}); err != nil {
				return err
			}
			api.OnError(errors.New("observed inside transaction"))
			released, err := api.ReleaseLease(ctx, *lease)
			if err != nil {
				return err
			}
			if !released {
				return errors.New("expected lease release inside transaction")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("transaction failed: %v", err)
		}
	})

	t.Run("nested transaction is rejected", func(t *testing.T) {
		err := infrastructure.Transaction(ctx, func(api Infrastructure) error {
			return api.Transaction(ctx, func(inner Infrastructure) error {
				return nil
			})
		})
		if err == nil {
			t.Fatal("expected nested transaction to be rejected")
		}
	})

	t.Run("canceled context is rejected", func(t *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		err := infrastructure.Transaction(canceled, func(api Infrastructure) error {
			t.Fatal("handler must not run")
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled, got %v", err)
		}
	})
}
