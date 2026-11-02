package core_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/lease"
)

type publication struct {
	id           string
	node         dflow.RuntimeNode[dflow.NoContext]
	incoming     []string
	outgoing     []string
	dependencies []string
}

func (p *publication) GetID() string                               { return p.id }
func (p *publication) GetNode() dflow.RuntimeNode[dflow.NoContext] { return p.node }
func (p *publication) GetIncoming() []string                       { return p.incoming }
func (p *publication) GetOutgoing() []string                       { return p.outgoing }
func (p *publication) GetDependencies() []string                   { return p.dependencies }
func (p *publication) LoadNode(context.Context, string) (dflow.PublishedNode[dflow.NoContext], error) {
	return p, nil
}

// A single-node transaction fixture, independent of unmigrated legacy adapters.
type storage struct {
	mu         sync.Mutex
	state      dflow.NodeState
	token      lease.Token
	commitErr  error
	renewErr   error
	renewed    chan lease.Token
	renewCount int
}

func (s *storage) ClaimNode(ctx context.Context, id string, ttl time.Duration) (*dflow.NodeTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if time.Now().Before(s.token.ExpiresAt) || s.state.Status == dflow.Succeeded || s.state.Status == dflow.Skipped {
		return nil, nil
	}
	if s.state.Status == dflow.Pending && !s.state.Ready {
		return nil, nil
	}
	s.state.Version++
	s.state.Generation++
	s.state.Status, s.state.Ready = dflow.Running, false
	s.token = lease.Token{Key: id, Generation: s.state.Generation, ExpiresAt: time.Now().Add(ttl)}
	state := s.state
	state.Input, state.Output = maps.Clone(state.Input), maps.Clone(state.Output)
	state.Incoming = maps.Clone(state.Incoming)
	return &dflow.NodeTask{NodeID: id, State: state, Lease: s.token}, nil
}
func (s *storage) RenewNode(ctx context.Context, token lease.Token, ttl time.Duration) (*lease.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.renewErr != nil {
		return nil, s.renewErr
	}
	if token.Generation != s.token.Generation || !time.Now().Before(s.token.ExpiresAt) {
		return nil, lease.ErrConflict
	}
	s.token.ExpiresAt = time.Now().Add(ttl)
	renewed := s.token
	s.renewCount++
	if s.renewed != nil {
		select {
		case s.renewed <- renewed:
		default:
		}
	}
	return &renewed, nil
}
func (s *storage) ReleaseNode(ctx context.Context, token lease.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if token.Generation != s.token.Generation {
		return lease.ErrConflict
	}
	s.token.ExpiresAt = time.Time{}
	return nil
}
func (s *storage) Commit(ctx context.Context, token lease.Token, commit dflow.NodeCommit) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.commitErr != nil {
		return nil, s.commitErr
	}
	if token.Key != commit.NodeID || token.Generation != s.token.Generation || !time.Now().Before(s.token.ExpiresAt) {
		return nil, lease.ErrConflict
	}
	if err := dflow.ValidateCommit(s.state, commit, token.Generation); err != nil {
		return nil, err
	}
	s.state = dflow.ApplyCommitResult(s.state, commit)
	s.token.ExpiresAt = time.Time{}
	return nil, nil
}

func newEngine(t *testing.T, run dflow.NodeFunc[dflow.NoContext]) (*dflow.Engine[dflow.NoContext], *storage) {
	t.Helper()
	return newEngineWithOptions(t, run, dflow.Options{})
}

func newEngineWithOptions(t *testing.T, run dflow.NodeFunc[dflow.NoContext], options dflow.Options) (*dflow.Engine[dflow.NoContext], *storage) {
	t.Helper()
	s := &storage{state: dflow.NodeState{Status: dflow.Pending, Ready: true, Enabled: true, Version: 1}, renewed: make(chan lease.Token, 16)}
	e, err := dflow.NewEngine[dflow.NoContext](&publication{id: "root", node: run}, s, options)
	if err != nil {
		t.Fatal(err)
	}
	return e, s
}

type panickingResult struct{ stage string }

func (r panickingResult) GetInput() map[string]any {
	if r.stage == "input" {
		panic("input getter")
	}
	return map[string]any{"captured": "input"}
}
func (r panickingResult) GetOutput() map[string]any {
	if r.stage == "output" {
		panic("output getter")
	}
	return map[string]any{"captured": "output"}
}
func (r panickingResult) GetError() error {
	if r.stage == "error" {
		panic("error getter")
	}
	return nil
}

type panickingAsync struct{ panickingResult }

func (panickingAsync) IsDone() bool { panic("done getter") }

type panickingBranch struct{ panickingResult }

func (panickingBranch) GetOutgoing() []string { panic("outgoing getter") }

func TestPanickingResultIsNotExposedAfterCommit(t *testing.T) {
	cases := map[string]dflow.NodeResult{
		"input":     panickingResult{"input"},
		"output":    panickingResult{"output"},
		"error":     panickingResult{"error"},
		"done":      panickingAsync{},
		"outgoing":  panickingBranch{},
		"typed-nil": (*dflow.Result)(nil),
		"nil":       nil,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			run := dflow.NodeFunc[dflow.NoContext](func(context.Context, dflow.NoContext) dflow.NodeResult { return raw })
			e, s := newEngine(t, run)
			step, err := e.Drive(context.Background(), "root", dflow.NoContext{})
			if !errors.Is(err, dflow.ErrInvalidResult) || step.Status != dflow.Failed || step.Result != nil {
				t.Fatalf("invalid result escaped: step=%+v err=%v", step, err)
			}
			if s.state.Status != dflow.Failed || s.state.Error == "" {
				t.Fatalf("failure was not persisted: %+v", s.state)
			}
			if name == "output" && s.state.Input["captured"] != "input" {
				t.Fatal("input captured before output panic was lost")
			}
			if name == "error" && s.state.Output["captured"] != "output" {
				t.Fatal("output captured before error panic was lost")
			}
			e, _ = newEngine(t, run)
			state, err := driver.Run(context.Background(), e, "root", dflow.NoContext{})
			if !errors.Is(err, dflow.ErrInvalidResult) || state.Nodes["root"].Status != dflow.Failed {
				t.Fatalf("driver did not return the recovered node failure: state=%+v err=%v", state, err)
			}
		})
	}
}

type customResult struct {
	dflow.Result
	operationID string
}

func TestSuccessfulResultPreservesBusinessObject(t *testing.T) {
	raw := &customResult{Result: dflow.Result{Output: map[string]any{"answer": 42}}, operationID: "operation-1"}
	e, s := newEngine(t, func(context.Context, dflow.NoContext) dflow.NodeResult { return raw })
	step, err := e.Drive(context.Background(), "root", dflow.NoContext{})
	if err != nil || step.Status != dflow.Succeeded || step.Result != raw || s.state.Output["answer"] != 42 {
		t.Fatalf("business result was not preserved: step=%+v state=%+v err=%v", step, s.state, err)
	}
}

func TestCommitFailureDoesNotReportCommittedResult(t *testing.T) {
	e, s := newEngine(t, func(context.Context, dflow.NoContext) dflow.NodeResult {
		return dflow.Result{Output: map[string]any{"answer": 42}}
	})
	s.commitErr = errors.New("storage unavailable")
	step, err := e.Drive(context.Background(), "root", dflow.NoContext{})
	var infraErr *dflow.InfrastructureError
	if !errors.As(err, &infraErr) || step.Status != "" || step.Result != nil || s.state.Status != dflow.Running {
		t.Fatalf("uncommitted result was reported: step=%+v state=%+v err=%v", step, s.state, err)
	}
}

func TestLeaseRenewal(t *testing.T) {
	t.Run("covers long running node", func(t *testing.T) {
		var store *storage
		e, s := newEngineWithOptions(t, func(ctx context.Context, _ dflow.NoContext) dflow.NodeResult {
			for i := 0; i < 2; i++ {
				select {
				case renewed := <-store.renewed:
					if renewed.Key != "root" || renewed.Generation == 0 {
						t.Fatalf("invalid renewal token: %+v", renewed)
					}
				case <-time.After(time.Second):
					t.Fatal("node lease was not renewed")
				}
			}
			return dflow.Result{Output: map[string]any{"renewed": true}}
		}, dflow.Options{Lease: lease.Options{TTL: 30 * time.Millisecond}})
		store = s
		step, err := e.Drive(context.Background(), "root", dflow.NoContext{})
		if err != nil || step.Status != dflow.Succeeded || s.renewCount < 2 || s.state.Output["renewed"] != true {
			t.Fatalf("long running node was not protected by renewals: step=%+v state=%+v renews=%d err=%v", step, s.state, s.renewCount, err)
		}
	})

	t.Run("failure prevents commit", func(t *testing.T) {
		e, s := newEngineWithOptions(t, func(ctx context.Context, _ dflow.NoContext) dflow.NodeResult {
			<-ctx.Done()
			return dflow.Result{Output: map[string]any{"shouldCommit": false}}
		}, dflow.Options{Lease: lease.Options{TTL: 30 * time.Millisecond}})
		s.renewErr = lease.ErrConflict
		step, err := e.Drive(context.Background(), "root", dflow.NoContext{})
		var infraErr *dflow.InfrastructureError
		if !errors.As(err, &infraErr) || !errors.Is(err, lease.ErrConflict) || step.Status != "" {
			t.Fatalf("renewal loss was not reported as uncertain infrastructure failure: step=%+v err=%v", step, err)
		}
		if s.state.Status != dflow.Running || s.state.Output != nil {
			t.Fatalf("lost lease committed a node result: %+v", s.state)
		}
	})
}

func TestHooks(t *testing.T) {
	t.Run("finish classifies outcomes", func(t *testing.T) {
		cases := []struct {
			name string
			run  dflow.NodeFunc[dflow.NoContext]
			want dflow.Status
		}{
			{
				name: "succeeded",
				run:  func(context.Context, dflow.NoContext) dflow.NodeResult { return dflow.Result{} },
				want: dflow.Succeeded,
			},
			{
				name: "failed",
				run: func(context.Context, dflow.NoContext) dflow.NodeResult {
					return dflow.ResultWithError(errors.New("business failure"))
				},
				want: dflow.Failed,
			},
			{
				name: "suspended",
				run:  func(context.Context, dflow.NoContext) dflow.NodeResult { return dflow.AsyncResult{} },
				want: dflow.Suspended,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var events []dflow.NodeEvent
				e, _ := newEngineWithOptions(t, tc.run, dflow.Options{Hooks: dflow.Hooks{
					OnFinish: func(_ context.Context, event dflow.NodeEvent) {
						events = append(events, event)
					},
				}})
				_, _ = e.Drive(context.Background(), "root", dflow.NoContext{})
				if len(events) != 1 || events[0].NodeID != "root" || events[0].Attempt != 1 || events[0].Outcome != tc.want || events[0].Duration <= 0 {
					t.Fatalf("unexpected finish event: %+v", events)
				}
				if tc.want == dflow.Failed && events[0].Err == nil {
					t.Fatalf("failed finish event did not carry error: %+v", events[0])
				}
			})
		}
	})

	t.Run("panics are ignored", func(t *testing.T) {
		e, s := newEngineWithOptions(t, func(context.Context, dflow.NoContext) dflow.NodeResult {
			return dflow.Result{Output: map[string]any{"ok": true}}
		}, dflow.Options{Hooks: dflow.Hooks{
			OnClaim:  func(context.Context, dflow.NodeEvent) { panic("claim hook") },
			OnStart:  func(context.Context, dflow.NodeEvent) { panic("start hook") },
			OnFinish: func(context.Context, dflow.NodeEvent) { panic("finish hook") },
		}})
		step, err := e.Drive(context.Background(), "root", dflow.NoContext{})
		if err != nil || step.Status != dflow.Succeeded || s.state.Output["ok"] != true {
			t.Fatalf("panicking hooks affected execution: step=%+v state=%+v err=%v", step, s.state, err)
		}
	})
}

func TestAsyncContinuationPreservesNilAndReplacesEmptyMaps(t *testing.T) {
	calls := 0
	e, s := newEngine(t, func(context.Context, dflow.NoContext) dflow.NodeResult {
		calls++
		if calls == 1 {
			return dflow.AsyncResult{Result: dflow.Result{
				Input: map[string]any{"taskID": "task-1"}, Output: map[string]any{"progress": 1},
			}}
		}
		return dflow.AsyncResult{Result: dflow.Result{Output: map[string]any{}}, Done: true}
	})
	step, err := e.Drive(context.Background(), "root", dflow.NoContext{})
	if err != nil || step.Status != dflow.Suspended || len(step.Ready) != 0 {
		t.Fatalf("unfinished async step propagated: %+v %v", step, err)
	}
	step, err = e.Drive(context.Background(), "root", dflow.NoContext{})
	if err != nil || step.Status != dflow.Succeeded || calls != 2 {
		t.Fatalf("continuation did not finish: %+v %v", step, err)
	}
	if s.state.Input["taskID"] != "task-1" || s.state.Output == nil || len(s.state.Output) != 0 {
		t.Fatalf("nil/empty map semantics changed: %+v", s.state)
	}
}

func TestArrivalWaitsForInactiveAndActivePredecessors(t *testing.T) {
	state, err := dflow.ApplyArrival(dflow.NodeState{}, "left", []string{"left", "right"}, false)
	if err != nil || state.Ready {
		t.Fatalf("partial arrival became ready: %+v %v", state, err)
	}
	state, err = dflow.ApplyArrival(state, "right", []string{"left", "right"}, true)
	if err != nil || !state.Ready || !state.Enabled {
		t.Fatalf("mixed arrivals did not enable node: %+v %v", state, err)
	}
	again, err := dflow.ApplyArrival(state, "right", []string{"left", "right"}, true)
	if err != nil || !reflect.DeepEqual(again, state) {
		t.Fatalf("duplicate arrival changed state: %+v %v", again, err)
	}
	if _, err := dflow.ApplyArrival(state, "right", []string{"left", "right"}, false); !errors.Is(err, dflow.ErrConflict) {
		t.Fatalf("conflicting arrival accepted: %v", err)
	}
	if _, err := dflow.ApplyArrival(dflow.NodeState{}, "left", []string{"left", "left"}, true); !errors.Is(err, dflow.ErrInvalidDefinition) {
		t.Fatalf("duplicate topology accepted: %v", err)
	}
}

func TestCommitRetainsVersionGenerationAndPropagationChecks(t *testing.T) {
	state := dflow.NodeState{Status: dflow.Running, Version: 7, Generation: 3, Enabled: true}
	for _, tc := range []struct {
		name       string
		commit     dflow.NodeCommit
		generation int64
	}{
		{"stale-version", dflow.NodeCommit{NodeID: "root", ExpectedVersion: 6, Status: dflow.Succeeded}, 3},
		{"stale-generation", dflow.NodeCommit{NodeID: "root", ExpectedVersion: 7, Status: dflow.Succeeded}, 2},
		{"self-arrival", dflow.NodeCommit{NodeID: "root", ExpectedVersion: 7, Status: dflow.Succeeded, Arrivals: map[string]bool{"root": true}}, 3},
		{"failed-propagation", dflow.NodeCommit{NodeID: "root", ExpectedVersion: 7, Status: dflow.Failed, Arrivals: map[string]bool{"next": false}}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := dflow.ValidateCommit(state, tc.commit, tc.generation); !errors.Is(err, dflow.ErrConflict) {
				t.Fatalf("invalid commit accepted: %v", err)
			}
		})
	}
	valid := dflow.NodeCommit{NodeID: "root", ExpectedVersion: 7, Status: dflow.Succeeded, Arrivals: map[string]bool{"next": true}}
	if err := dflow.ValidateCommit(state, valid, 3); err != nil {
		t.Fatalf("valid commit rejected: %v", err)
	}
}

type spec struct{ record *publication }

func (s spec) GetID() string                                 { return s.record.id }
func (spec) Validate(context.Context, dflow.NoContext) error { return nil }
func (s spec) Build(context.Context, dflow.NoContext) (dflow.PublishedNode[dflow.NoContext], error) {
	return s.record, nil
}

type edge struct{ source, target string }

func (e edge) GetSourceNodeID() string { return e.source }
func (e edge) GetTargetNodeID() string { return e.target }

type definition struct {
	nodes []dflow.NodeSpec[dflow.NoContext, dflow.NoContext]
	edges []dflow.Edge
}

func (d definition) GetNodes() []dflow.NodeSpec[dflow.NoContext, dflow.NoContext] { return d.nodes }
func (d definition) GetEdges() []dflow.Edge                                       { return d.edges }

func TestPublishedDependenciesRemainValidated(t *testing.T) {
	for _, deps := range [][]string{{"root"}, {"root", "root"}, {"missing"}, {"child"}, {""}} {
		t.Run(fmt.Sprint(deps), func(t *testing.T) {
			node := dflow.NodeFunc[dflow.NoContext](func(context.Context, dflow.NoContext) dflow.NodeResult { return dflow.Result{} })
			root := &publication{id: "root", node: node, outgoing: []string{"child"}}
			child := &publication{id: "child", node: node, incoming: []string{"root"}, dependencies: deps}
			records, err := dflow.Compile(context.Background(), definition{
				nodes: []dflow.NodeSpec[dflow.NoContext, dflow.NoContext]{spec{root}, spec{child}},
				edges: []dflow.Edge{edge{"root", "child"}},
			}, dflow.NoContext{})
			if len(deps) == 1 && deps[0] == "root" {
				if err != nil || len(records) != 2 || records[0] != root || records[1] != child {
					t.Fatalf("valid publications were not preserved: %v %v", records, err)
				}
			} else if !errors.Is(err, dflow.ErrInvalidReference) {
				t.Fatalf("invalid dependencies accepted: %v", err)
			}
		})
	}
}
