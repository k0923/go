package tests

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/memory"
	"github.com/k0923/go/lease"
)

func TestRegressionMissingInputFailsDurably(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		ctx := context.Background()
		graph := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{
			{ID: "out", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"required": dflow.Input("absent")}},
		}}, nil)
		e, err := dflow.NewEngine(graph, infra, dflow.Options{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = driver.Run(ctx, e, nil)
		if !errors.Is(err, dflow.ErrMissingValue) {
			t.Fatal(err)
		}
		var nodeErr *dflow.NodeError
		var infraErr *dflow.InfrastructureError
		if !errors.As(err, &nodeErr) || errors.As(err, &infraErr) {
			t.Fatal("missing input was not classified as a node failure", err)
		}
		node, _ := infra.GetNode(ctx, "out")
		if node.Status != dflow.Failed {
			t.Fatalf("missing immutable input should fail durably; status=%s err=%v", node.Status, err)
		}
	})
}

func TestRegressionOldTokenDoesNotDriveReadyNewSuspension(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		ctx := context.Background()
		resumes := 0
		registry := asyncRegistry(t, asyncNode{
			run: func(context.Context, map[string]any) (dflow.Result, error) {
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external"}}, nil
			},
			resume: func(context.Context, map[string]any, dflow.ResumeInput) (dflow.Result, error) {
				resumes++
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external"}}, nil
			},
		})
		e, _ := dflow.NewEngine(compile(t, waitDefinition(), registry), infra, dflow.Options{})
		initial, err := driver.Run(ctx, e, nil)
		if err != nil {
			t.Fatal(err)
		}
		old := initial.Nodes["wait"].Suspension.Token
		if _, err := driver.Resume(ctx, e, "wait", old, "first"); err != nil {
			t.Fatal(err)
		}
		current, _ := infra.GetNode(ctx, "wait")
		if accepted, err := e.Wakeup(ctx, "wait", current.Suspension.Token, "second"); !accepted || err != nil {
			t.Fatal(accepted, err)
		}
		current, _ = infra.GetNode(ctx, "wait")
		state, err := driver.Resume(ctx, e, "wait", old, "late")
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Nodes) != 0 || resumes != 1 {
			t.Fatalf("stale token drove current suspension: touched=%d resumes=%d", len(state.Nodes), resumes)
		}
		if state, err := driver.Resume(ctx, e, "wait", old); err != nil || len(state.Nodes) != 0 {
			t.Fatal("stale wake notification executed work", state, err)
		}
		afterStale, err := infra.GetNode(ctx, "wait")
		if err != nil || afterStale.Suspension == nil || afterStale.Suspension.Token != current.Suspension.Token {
			t.Fatal("stale claim replaced the newer suspension", current, afterStale, err)
		}
		response, err := infra.GetResponse(ctx, "wait", current.Suspension.Token)
		if err != nil || response == nil || response.Value != "second" {
			t.Fatal("stale claim lost the current response", response, err)
		}
		if state, err := driver.Resume(ctx, e, "wait", current.Suspension.Token); err != nil || state.Nodes["wait"].Status != dflow.Suspended || resumes != 2 {
			t.Fatal("current persisted callback was lost", state, err)
		}
	})
}

func TestRegressionHooksMatchFailedExecution(t *testing.T) {
	for _, mode := range []string{"panic", "timeout-returning-nil", "invalid-suspension", "unknown-port"} {
		t.Run(mode, func(t *testing.T) {
			finishes := []dflow.NodeEvent{}
			infra := memory.New()
			graph := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{{
				ID: "out", Node: dflow.NodeFunc(func(ctx context.Context, _ map[string]any) (dflow.Result, error) {
					switch mode {
					case "panic":
						panic("boom")
					case "timeout-returning-nil":
						<-ctx.Done()
						return dflow.Result{}, nil
					case "unknown-port":
						return dflow.Result{Ports: []string{"missing"}}, nil
					default:
						return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external"}}, nil
					}
				}),
			}}}, nil)
			e, _ := dflow.NewEngine(graph, infra, dflow.Options{
				NodeTimeout: 10 * time.Millisecond,
				Hooks: dflow.Hooks{OnFinish: func(_ context.Context, event dflow.NodeEvent) {
					finishes = append(finishes, event)
				}},
			})
			_, err := driver.Run(context.Background(), e, nil)
			node, _ := infra.GetNode(context.Background(), "out")
			if err == nil || node.Status != dflow.Failed {
				t.Fatal("probe did not fail execution", node, err)
			}
			if len(finishes) != 1 || finishes[0].Outcome != dflow.Failed || finishes[0].Err == nil {
				t.Fatalf("failed execution has wrong finish event: %+v; stored status=%s", finishes, node.Status)
			}
		})
	}
}

type delayedRenewal struct {
	dflow.Infrastructure
	entered, release, commitEntered chan struct{}
	once                            sync.Once
	inFlight                        atomic.Bool
	commitOverlapped                atomic.Bool
	failure                         error
	invalid                         bool
}

func (s *delayedRenewal) RenewNode(ctx context.Context, token lease.Token, ttl time.Duration) (*lease.Token, error) {
	s.inFlight.Store(true)
	defer s.inFlight.Store(false)
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.failure != nil || s.invalid {
		return nil, s.failure
	}
	return s.Infrastructure.RenewNode(ctx, token, ttl)
}

func (s *delayedRenewal) Commit(ctx context.Context, token lease.Token, commit dflow.NodeCommit) ([]string, error) {
	s.commitOverlapped.Store(s.inFlight.Load())
	close(s.commitEntered)
	return s.Infrastructure.Commit(ctx, token, commit)
}

func TestRegressionRenewalFinishesBeforeCommit(t *testing.T) {
	for _, mode := range []string{"success", "storage-failure", "invalid-token"} {
		t.Run(mode, func(t *testing.T) {
			backends(t, func(t *testing.T, infra dflow.Infrastructure) {
				store := &delayedRenewal{
					Infrastructure: infra, entered: make(chan struct{}), release: make(chan struct{}),
					commitEntered: make(chan struct{}),
				}
				if mode == "storage-failure" {
					store.failure = errors.New("renewal unavailable")
				}
				store.invalid = mode == "invalid-token"
				finished := make(chan struct{})
				graph := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{{
					ID: "node", Node: dflow.NodeFunc(func(ctx context.Context, _ map[string]any) (dflow.Result, error) {
						select {
						case <-store.entered:
							return dflow.Result{Output: "done"}, nil
						case <-ctx.Done():
							return dflow.Result{}, ctx.Err()
						}
					}),
				}}}, nil)
				e, err := dflow.NewEngine(graph, store, dflow.Options{
					Lease: lease.Options{TTL: 300 * time.Millisecond},
					Hooks: dflow.Hooks{OnFinish: func(context.Context, dflow.NodeEvent) { close(finished) }},
				})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				id, err := e.Start(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				type answer struct {
					executed dflow.Executed
					err      error
				}
				result := make(chan answer, 1)
				go func() { r, err := e.Drive(ctx, id); result <- answer{r, err} }()
				select {
				case <-finished:
				case <-ctx.Done():
					t.Fatal("node did not return", ctx.Err())
				}
				// Keep the renewal in flight while Drive advances towards Commit.
				select {
				case <-store.commitEntered:
				case <-time.After(20 * time.Millisecond):
				}
				close(store.release)
				got := <-result
				if store.commitOverlapped.Load() {
					t.Fatal("Commit overlapped an in-flight RenewNode")
				}
				current, err := infra.GetNode(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "success" {
					if got.err != nil || current.Status != dflow.Succeeded || got.executed.Status != dflow.Succeeded {
						t.Fatal(got, current)
					}
				} else {
					var infraErr *dflow.InfrastructureError
					if got.err == nil || !errors.As(got.err, &infraErr) || current.Status != dflow.Running || got.executed.Status != "" {
						t.Fatal("renewal failure was swallowed or committed", got, current)
					}
					select {
					case <-store.commitEntered:
						t.Fatal("Commit called after a failed renewal")
					default:
					}
				}
			})
		})
	}
}

type unavailableInput struct {
	dflow.Infrastructure
	failure error
}

func (s unavailableInput) GetInput(context.Context, ...string) (any, error) { return nil, s.failure }

func TestRegressionInputStorageFailureStaysRetryable(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		failure := errors.New("database unavailable")
		e := engine(t, dflow.Definition{Nodes: []dflow.NodeSpec{{
			ID: "node", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"x": dflow.Input("x")},
		}}}, nil, infra, dflow.Options{}).Engine
		id, err := e.Start(context.Background(), map[string]any{"x": "ok"})
		if err != nil {
			t.Fatal(err)
		}
		graph := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{{
			ID: "node", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"x": dflow.Input("x")},
		}}}, nil)
		e, err = dflow.NewEngine(graph, unavailableInput{infra, failure}, dflow.Options{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.Drive(context.Background(), id)
		var infraErr *dflow.InfrastructureError
		if !errors.Is(err, failure) || !errors.As(err, &infraErr) {
			t.Fatal(err)
		}
		current, err := infra.GetNode(context.Background(), id)
		if err != nil || current.Status != dflow.Running {
			t.Fatal(current, err)
		}
	})
}

// This adapter intentionally exposes only the original Infrastructure methods.
// A newer suspension becomes ready precisely before its ClaimNode is called.
type resumeClaimRace struct {
	dflow.Infrastructure
	before func()
}

func (s *resumeClaimRace) ClaimNode(ctx context.Context, id string, ttl time.Duration) (*dflow.NodeTask, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		before()
	}
	return s.Infrastructure.ClaimNode(ctx, id, ttl)
}

func TestRegressionResumeChecksTheClaimedSnapshot(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		ctx := context.Background()
		var resumes int
		node := asyncNode{
			run: func(context.Context, map[string]any) (dflow.Result, error) {
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "job"}}, nil
			},
			resume: func(context.Context, map[string]any, dflow.ResumeInput) (dflow.Result, error) {
				resumes++
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "job"}}, nil
			},
		}
		graph := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{{ID: "wait", Node: &node}}}, nil)
		e, err := dflow.NewEngine(graph, infra, dflow.Options{})
		if err != nil {
			t.Fatal(err)
		}
		initial, err := driver.Run(ctx, e, nil)
		if err != nil {
			t.Fatal(err)
		}
		old := initial.Nodes["wait"].Suspension.Token
		if accepted, err := e.Wakeup(ctx, "wait", old, "one"); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
		wrapper := &resumeClaimRace{Infrastructure: infra}
		wrapper.before = func() {
			// Another worker consumes the old response and creates a fresh wait.
			executed, err := e.Resume(ctx, "wait", old)
			if err != nil || executed.Status != dflow.Suspended {
				t.Fatal(executed, err)
			}
			if accepted, err := e.Wakeup(ctx, "wait", executed.Token, "two"); err != nil || !accepted {
				t.Fatal(accepted, err)
			}
		}
		legacy, err := dflow.NewEngine(graph, wrapper, dflow.Options{})
		if err != nil {
			t.Fatal(err)
		}
		executed, err := legacy.Resume(ctx, "wait", old)
		if err != nil || executed.Status != "" || resumes != 1 {
			t.Fatal("stale request ran a newer suspension", executed, err, resumes)
		}
		current, err := infra.GetNode(ctx, "wait")
		if err != nil || current.Suspension.Token == old {
			t.Fatal(current, err)
		}
		if _, err := legacy.Resume(ctx, "wait", current.Suspension.Token); err != nil || resumes != 2 {
			t.Fatal("fallback failed to release the lease", resumes, err)
		}
	})
}

func TestRegressionResumePanicReportsOneFinish(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		var events []dflow.NodeEvent
		node := asyncNode{
			run: func(context.Context, map[string]any) (dflow.Result, error) {
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "job"}}, nil
			},
			resume: func(context.Context, map[string]any, dflow.ResumeInput) (dflow.Result, error) {
				panic("resume failed")
			},
		}
		graph := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{{ID: "wait", Node: &node}}}, nil)
		e, err := dflow.NewEngine(graph, infra, dflow.Options{Hooks: dflow.Hooks{
			OnFinish: func(_ context.Context, event dflow.NodeEvent) { events = append(events, event) },
		}})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		initial, err := driver.Run(ctx, e, nil)
		if err != nil {
			t.Fatal(err)
		}
		token := initial.Nodes["wait"].Suspension.Token
		if _, err := driver.Resume(ctx, e, "wait", token, nil); err == nil {
			t.Fatal("resume panic did not fail")
		}
		if len(events) != 2 || events[0].Outcome != dflow.Suspended || events[0].IsResume ||
			events[1].Outcome != dflow.Failed || !events[1].IsResume || events[1].Token != token ||
			events[1].Err == nil || events[1].Duration <= 0 {
			t.Fatal("incorrect resume lifecycle events", events)
		}
	})
}
