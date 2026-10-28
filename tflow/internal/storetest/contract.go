// Package storetest verifies adapters using application-owned business fixtures.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
)

// Fixture must contain an empty Store and a plan containing exactly two units.
// Advance waits or advances the adapter clock; it must not affect Engine clocks.
type Fixture struct {
	Store   tflow.Store
	Main    tflow.MainTask
	Plan    tflow.Plan
	Advance func(time.Duration)
}

func Run(t *testing.T, factory func(*testing.T) Fixture) {
	t.Helper()
	seed := func(t *testing.T) (tflow.Store, tflow.MainRecord, []tflow.SubRecord, func(time.Duration)) {
		t.Helper()
		f := factory(t)
		s := f.Store
		ctx := context.Background()
		m, err := tflow.NewMain(f.Main, tflow.SubmitOptions{ConcurrencyLimit: 1}, time.Unix(1, 0))
		if err != nil {
			t.Fatal(err)
		}
		m, subs, err := tflow.ApplyPlan(m, f.Plan, 2, time.Unix(1, 0))
		if err != nil {
			t.Fatal(err)
		}
		err = s.WithTx(ctx, func(tx tflow.Tx) error {
			if err := tx.CommitPlan(0, m, f.Plan); err != nil {
				return err
			}
			return tx.CreateSubs(subs)
		})
		if err != nil {
			t.Fatal(err)
		}
		return s, m, subs, f.Advance
	}
	t.Run("rollback_and_cas", func(t *testing.T) {
		s, m, _, _ := seed(t)
		ctx := context.Background()
		abort := errors.New("abort")
		err := s.WithTx(ctx, func(tx tflow.Tx) error {
			m.Version++
			m.Error = "uncommitted"
			if err := tx.SaveMain(m.Version-1, m.MainState); err != nil {
				return err
			}
			if err := tx.PutCommand(tflow.ReconcileCommand(s.MainID(), m.MainState)); err != nil {
				return err
			}
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatalf("rollback error: %v", err)
		}
		current, err := s.GetMain(ctx)
		if err != nil || current.Version != 1 || current.Error != "" {
			t.Fatalf("rollback: %+v %v", current, err)
		}
		commands, err := s.PendingCommands(ctx)
		if err != nil || len(commands) != 0 {
			t.Fatalf("rolled-back command persisted: %v %v", commands, err)
		}
		err = s.WithTx(ctx, func(tx tflow.Tx) error { return tx.SaveMain(0, current.MainState) })
		if !errors.Is(err, tflow.ErrConflict) {
			t.Fatalf("stale CAS: %v", err)
		}
	})
	t.Run("isolated_snapshots", func(t *testing.T) {
		s, _, subs, _ := seed(t)
		sub, err := s.GetSub(context.Background(), subs[0].Key)
		if err != nil {
			t.Fatal(err)
		}
		sub.Error = "changed local snapshot"
		sub.Version++
		sub, err = s.GetSub(context.Background(), subs[0].Key)
		if err != nil || sub.Error != "" || sub.Version != 1 || sub.Task.Key() != subs[0].Key {
			t.Fatalf("snapshot alias: %+v %v", sub, err)
		}
	})
	t.Run("read_only_sub_queries", func(t *testing.T) {
		s, _, subs, _ := seed(t)
		ctx := context.Background()
		all, err := s.FindSubs(ctx, tflow.SubQuery{Keys: []string{subs[1].Key, subs[0].Key}})
		if err != nil || len(all) != 2 || all[0].Key != subs[0].Key || all[1].Key != subs[1].Key {
			t.Fatalf("query order: %+v %v", all, err)
		}
		all[0].Error = "local change"
		current, err := s.GetSub(ctx, all[0].Key)
		if err != nil || current.Error != "" {
			t.Fatalf("query state alias: %+v %v", current, err)
		}
		pending, err := s.FindSubs(ctx, tflow.SubQuery{Statuses: []tflow.SubStatus{tflow.SubPending}, Limit: 1})
		if err != nil || len(pending) != 1 || pending[0].Key != subs[1].Key {
			t.Fatalf("query filter: %+v %v", pending, err)
		}
		limited, err := s.FindSubs(ctx, tflow.SubQuery{Limit: 1})
		if err != nil || len(limited) != 1 || limited[0].Key != subs[0].Key {
			t.Fatalf("query limit: %+v %v", limited, err)
		}
		if _, err := s.FindSubs(ctx, tflow.SubQuery{Limit: -1}); !errors.Is(err, tflow.ErrInvalid) {
			t.Fatalf("negative query limit: %v", err)
		}
		after := subs[0].Cursor
		page, err := s.FindSubs(ctx, tflow.SubQuery{AfterCursor: &after, Limit: 1})
		if err != nil || len(page) != 1 || page[0].Key != subs[1].Key {
			t.Fatalf("cursor pagination: %+v %v", page, err)
		}
	})
	t.Run("delivery_fencing_and_dedup", func(t *testing.T) {
		s, m, _, advance := seed(t)
		ctx := context.Background()
		command := tflow.ReconcileCommand(s.MainID(), m.MainState)
		err := s.WithTx(ctx, func(tx tflow.Tx) error { return tx.PutCommand(command) })
		if err != nil {
			t.Fatal(err)
		}
		ttl := 20 * time.Millisecond
		first, err := s.ClaimCommand(ctx, ttl)
		if err != nil || first == nil {
			t.Fatalf("claim: %v %v", first, err)
		}
		duplicate, err := s.ClaimCommand(ctx, ttl)
		if err != nil || duplicate != nil {
			t.Fatalf("double claim: %v %v", duplicate, err)
		}
		advance(ttl)
		if err := s.AckCommand(ctx, *first); !errors.Is(err, tflow.ErrDeliveryLeaseLost) {
			t.Fatalf("expired owner ack: %v", err)
		}
		second, err := s.ClaimCommand(ctx, time.Minute)
		if err != nil || second == nil || second.Generation <= first.Generation {
			t.Fatalf("reclaim: %v %v", second, err)
		}
		if err := s.AckCommand(ctx, *first); !errors.Is(err, tflow.ErrDeliveryLeaseLost) {
			t.Fatalf("old ack: %v", err)
		}
		if err := s.ReleaseCommand(ctx, *first, 0, "stale"); !errors.Is(err, tflow.ErrDeliveryLeaseLost) {
			t.Fatalf("old release: %v", err)
		}
		if err := s.AckCommand(ctx, *second); err != nil {
			t.Fatal(err)
		}
		if err := s.WithTx(ctx, func(tx tflow.Tx) error { return tx.PutCommand(command) }); err != nil {
			t.Fatal(err)
		}
		remaining, err := s.PendingCommands(ctx)
		if err != nil || len(remaining) != 0 {
			t.Fatalf("acked command resurrected: %v %v", remaining, err)
		}
	})
	t.Run("canceled_context", func(t *testing.T) {
		s, _, _, _ := seed(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.WithTx(ctx, func(tx tflow.Tx) error { t.Fatal("called canceled transaction"); return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled context: %v", err)
		}
		if _, err := s.FindSubs(ctx, tflow.SubQuery{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled query: %v", err)
		}
	})
	t.Run("consistent_summary_and_rollback", func(t *testing.T) {
		s, m, subs, _ := seed(t)
		ctx := context.Background()
		record, err := s.GetMain(ctx)
		if err != nil || record.Subs.Running != 1 || record.Subs.Pending != 1 {
			t.Fatalf("initial summary: %+v %v", record.Subs, err)
		}
		abort := errors.New("abort child transition")
		transition := func(tx tflow.Tx) error {
			sub, err := tx.Sub(subs[0].Key)
			if err != nil {
				return err
			}
			sub.Version++
			sub.Status = tflow.SubSucceeded
			if err := tx.SaveSub(sub.Version-1, sub.SubState); err != nil {
				return err
			}
			current, err := tx.Main()
			if err != nil || current.Subs.Running != 0 || current.Subs.Pending != 1 || current.Subs.Succeeded != 1 {
				t.Fatalf("candidate summary must reflect child write: %+v %v", current.Subs, err)
			}
			current.Version++
			return tx.SaveMain(m.Version, current.MainState)
		}
		err = s.WithTx(ctx, func(tx tflow.Tx) error {
			if err := transition(tx); err != nil {
				return err
			}
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatal(err)
		}
		record, _ = s.GetMain(ctx)
		if record.Subs.Running != 1 || record.Subs.Succeeded != 0 || record.Version != m.Version {
			t.Fatal("rolled-back summary leaked")
		}
		if err := s.WithTx(ctx, transition); err != nil {
			t.Fatal(err)
		}
		record, _ = s.GetMain(ctx)
		if record.Subs.Running != 0 || record.Subs.Succeeded != 1 || record.Version != m.Version+1 {
			t.Fatal("committed summary missing")
		}
	})
	t.Run("identity_version_and_summary_guards", func(t *testing.T) {
		for _, name := range []string{"duplicate_key", "duplicate_cursor", "sub_version", "sub_cursor", "main_version", "summary_without_version"} {
			t.Run(name, func(t *testing.T) {
				s, m, subs, _ := seed(t)
				err := s.WithTx(context.Background(), func(tx tflow.Tx) error {
					sub := subs[0]
					switch name {
					case "duplicate_key":
						sub.Cursor = 100
						return tx.CreateSubs([]tflow.SubRecord{sub})
					case "duplicate_cursor":
						sub.Key = "another-key"
						return tx.CreateSubs([]tflow.SubRecord{sub})
					case "sub_version":
						return tx.SaveSub(sub.Version, sub.SubState)
					case "sub_cursor":
						sub.Version++
						sub.Cursor++
						return tx.SaveSub(sub.Version-1, sub.SubState)
					case "main_version":
						return tx.SaveMain(m.Version, m.MainState)
					case "summary_without_version":
						sub.Version++
						sub.Status = tflow.SubSucceeded
						return tx.SaveSub(sub.Version-1, sub.SubState)
					}
					return nil
				})
				if !errors.Is(err, tflow.ErrInvalid) && !errors.Is(err, tflow.ErrInvalidPlan) {
					t.Fatalf("guard %s: %v", name, err)
				}
				current, err := s.GetMain(context.Background())
				if err != nil || current.Version != m.Version || current.Subs != m.Subs {
					t.Fatalf("guard failed to roll back %+v %v", current, err)
				}
			})
		}
	})
	t.Run("state_preserves_business_and_config", func(t *testing.T) {
		s, m, _, _ := seed(t)
		err := s.WithTx(context.Background(), func(tx tflow.Tx) error {
			current, err := tx.Main()
			if err != nil {
				return err
			}
			current.ConcurrencyLimit = 999
			current.PlanningPolicy = tflow.WavePolicy{}
			current.Version++
			return tx.SaveMain(m.Version, current.MainState)
		})
		if err != nil {
			t.Fatal(err)
		}
		current, err := s.GetMain(context.Background())
		if err != nil || current.ConcurrencyLimit != m.ConcurrencyLimit {
			t.Fatal("config overwritten", err)
		}
		if _, ok := current.PlanningPolicy.(tflow.FillPolicy); !ok {
			t.Fatal("policy overwritten")
		}
	})
	t.Run("save_task_cas_and_wake_rollback", func(t *testing.T) {
		s, m, _, _ := seed(t)
		abort := errors.New("abort business update")
		err := s.WithTx(context.Background(), func(tx tflow.Tx) error {
			if err := tx.SaveTask(m.Version, m.Task, time.Unix(10, 0)); err != nil {
				return err
			}
			next, err := tx.Main()
			if err != nil {
				return err
			}
			if next.Version != m.Version+1 {
				t.Fatal("SaveTask did not advance version")
			}
			if err := tx.PutCommand(tflow.ReconcileCommand(s.MainID(), next.MainState)); err != nil {
				return err
			}
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatal(err)
		}
		current, err := s.GetMain(context.Background())
		if err != nil || current.Version != m.Version {
			t.Fatal("partial business update")
		}
		err = s.WithTx(context.Background(), func(tx tflow.Tx) error {
			return tx.SaveTask(m.Version+1, m.Task, time.Unix(10, 0))
		})
		if !errors.Is(err, tflow.ErrConflict) {
			t.Fatal("stale business update accepted", err)
		}
	})
}
