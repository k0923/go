package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

func TestInfrastructure(t *testing.T) {
	t.Run("fencing expiry CAS and deep copies", func(t *testing.T) {
		now := time.Now()
		m := memory.New()
		m.SetClock(func() time.Time { return now })
		ctx := context.Background()
		first, err := m.Claim(ctx, "step", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		r, err := m.Commit(ctx, *first, &Record{Key: "step", Kind: ActionKind, Status: Running, Input: []byte(`"input"`)}, 0)
		if err != nil || r.Version != 1 {
			t.Fatalf("%+v %v", r, err)
		}
		r.Input[1] = 'X'
		fresh, _ := m.Get(ctx, "step")
		if string(fresh.Input) != `"input"` {
			t.Fatal("stored data mutated")
		}
		now = now.Add(time.Minute)
		fresh.Status = Completed
		fresh.Result = []byte(`7`)
		if _, err := m.Commit(ctx, *first, fresh, 1); !errors.Is(err, ErrConflict) {
			t.Fatal("expired owner committed", err)
		}
		second, err := m.Claim(ctx, "step", time.Minute)
		if err != nil || second.Generation == first.Generation {
			t.Fatalf("%+v %v", second, err)
		}
		if _, err := m.Commit(ctx, *first, fresh, 1); !errors.Is(err, ErrConflict) {
			t.Fatal("zombie committed", err)
		}
		if err := m.Release(ctx, *first); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		if _, err := m.Renew(ctx, *first, time.Minute); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		renewed, err := m.Renew(ctx, *second, time.Hour)
		if err != nil || !renewed.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("%+v %v", renewed, err)
		}
		if _, err := m.Commit(ctx, *second, fresh, 0); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		committed, err := m.Commit(ctx, *second, fresh, 1)
		if err != nil || committed.Version != 2 || committed.Status != Completed {
			t.Fatalf("%+v %v", committed, err)
		}
		if _, err := m.Commit(ctx, *second, committed, 2); !errors.Is(err, ErrConflict) {
			t.Fatal("terminal rewritten", err)
		}
		if err := m.Release(ctx, *second); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("resolve racing registration returns resolved snapshot", func(t *testing.T) {
		m := memory.New()
		ctx := context.Background()
		request := &Record{Key: "callback", Kind: SignalKind, Status: Suspended, Input: []byte(`"request"`)}
		stored, err := m.Put(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = m.Resolve(ctx, "callback", stored.Version, []byte(`"approved"`)); err != nil {
			t.Fatal(err)
		}
		existing, err := m.Put(ctx, request)
		if err != nil || existing.Status != Completed || string(existing.Result) != `"approved"` {
			t.Fatalf("%+v %v", existing, err)
		}
		messages, _ := m.Pending(ctx, time.Now())
		if len(messages) != 2 {
			t.Fatal(messages)
		}
		if _, err := m.Put(ctx, &Record{Key: "callback", Kind: SleepKind, Status: Suspended}); !errors.Is(err, ErrChanged) {
			t.Fatal(err)
		}
	})
	t.Run("context cancellation rejects operations", func(t *testing.T) {
		m := memory.New()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, getErr := m.Get(ctx, "x")
		_, claimErr := m.Claim(ctx, "x", time.Second)
		_, renewErr := m.Renew(ctx, Lease{}, time.Second)
		releaseErr := m.Release(ctx, Lease{})
		_, commitErr := m.Commit(ctx, Lease{}, &Record{}, 0)
		_, putErr := m.Put(ctx, &Record{})
		_, resolveErr := m.Resolve(ctx, "x", 1, nil)
		_, pendingErr := m.Pending(ctx, time.Now())
		ackErr := m.Ack(ctx, "x")
		for _, err := range []error{getErr, claimErr, renewErr, releaseErr, commitErr, putErr, resolveErr, pendingErr, ackErr} {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel lost: %v", err)
			}
		}
	})
}
