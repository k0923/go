package tflow_test

import (
	"errors"
	"testing"

	"github.com/k0923/go/tflow"
)

// Explore event ordering and retries, checking aggregate invariants after every
// operation rather than asserting one particular transition implementation.
func FuzzLifecycleInvariants(f *testing.F) {
	f.Add([]byte{0, 0, 1, 2, 0, 3, 1})
	f.Add([]byte{1, 2, 2, 0, 0, 0})
	f.Add([]byte{3, 2, 0, 4, 1, 2})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 64 {
			operations = operations[:64]
		}
		s := newStore()
		e := newEngine(t, s, newSource(5), tflow.Options{})
		if _, err := e.submit(ctx, spec(2)); err != nil {
			t.Fatal(err)
		}
		attempts := make(map[string]int64)
		for _, operation := range operations {
			var err error
			switch operation % 5 {
			case 0:
				subs, readErr := s.FindSubs(ctx, tflow.SubQuery{})
				if readErr != nil {
					t.Fatal(readErr)
				}
				if len(subs) > 0 {
					sub := subs[int(operation)%len(subs)]
					status := []tflow.SubStatus{tflow.SubSucceeded, tflow.SubFailed, tflow.SubCanceled}[int(operation/5)%3]
					_, err = e.Report(ctx, tflow.Report{Execution: sub.Execution(e.MainID()), Status: status})
				}
			case 1:
				_, err = e.Cancel(ctx)
			case 2:
				_, err = e.Rerun(ctx, tflow.RerunFailedCanceled)
			case 3:
				err = e.Reconcile(ctx)
			case 4:
				_, err = e.Fail(ctx, "operator failure")
			}
			if err != nil && !errors.Is(err, tflow.ErrInvalid) && !errors.Is(err, tflow.ErrAlreadyTerminal) && !errors.Is(err, tflow.ErrNoSubTaskToRerun) {
				t.Fatal(err)
			}
			m, err := s.GetMain(ctx)
			if err != nil || tflow.ValidateMain(m) != nil {
				t.Fatalf("invalid aggregate %+v %v", m, err)
			}
			subs, err := s.FindSubs(ctx, tflow.SubQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(subs)) != m.DispatchCursor {
				t.Fatal("materialized count drift")
			}
			for _, sub := range subs {
				if sub.Attempt < attempts[sub.Key] {
					t.Fatal("attempt moved backwards")
				}
				attempts[sub.Key] = sub.Attempt
			}
		}
	})
}
