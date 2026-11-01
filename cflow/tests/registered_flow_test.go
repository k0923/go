package tests

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/debugger"
	"github.com/k0923/go/cflow/memory"
)

type registeredClass struct {
	FlowBase[int, int]
	Values []int
}

func (c registeredClass) Run(_ *FlowContext, in int) (int, error) {
	return in + c.Values[0], nil
}

type wrongBridge struct{ FlowBase[int, int] }

func (wrongBridge) Run(*FlowContext, string) (int, error) { return 0, nil }

type wrappedFunction struct {
	*FunctionFlow[int, int]
	Values []int
}

func TestFlowRegistry(t *testing.T) {
	first := FlowFunc[int, int](func(_ *FlowContext, n int) (int, error) { return n + 1, nil })
	second := FlowFunc[int, int](func(_ *FlowContext, n int) (int, error) { return n + 2, nil })
	for _, opts := range []Options{
		{Flows: map[string]FlowDefinition{"bad-key": first}},
		{Flows: map[string]FlowDefinition{"nil": (*FunctionFlow[int, int])(nil)}},
		{Flows: map[string]FlowDefinition{"nil": FlowFunc[int, int](nil)}},
		{Flows: map[string]FlowDefinition{"wrong": wrongBridge{}}},
		{Flows: map[string]FlowDefinition{"missing": FlowBase[int, int]{}}},
		{Flows: map[string]FlowDefinition{"a": first, "b": first}},
		{Flows: map[string]FlowDefinition{"a": registeredClass{}, "b": registeredClass{}}},
		{Flows: map[string]FlowDefinition{"a": registeredClass{}, "b": &registeredClass{}}},
		{Flows: map[string]FlowDefinition{"a": wrappedFunction{FunctionFlow: first}, "b": wrappedFunction{FunctionFlow: second}}},
	} {
		if _, err := NewEngine(memory.New(), opts); !errors.Is(err, ErrInvalid) {
			t.Fatalf("registry accepted %+v: %v", opts, err)
		}
	}
	if _, err := NewEngine(memory.New(), Options{Flows: map[string]FlowDefinition{
		"wrapped": wrappedFunction{FunctionFlow: first, Values: []int{1}},
	}}); err != nil {
		t.Fatalf("non-comparable wrapper: %v", err)
	}
	// Version is optional and defaults to DefaultVersion.
	if _, err := NewEngine(memory.New(), Options{Flows: map[string]FlowDefinition{"first": first}}); err != nil {
		t.Fatalf("default version: %v", err)
	}
	registry := map[string]FlowDefinition{"first": first, "second": second, "class": registeredClass{Values: []int{10}}}
	engine := testEngine(t, memory.New(), Options{Flows: registry})
	delete(registry, "class")
	name, err := engine.FlowName(registeredClass{Values: []int{99}})
	if err != nil || name != "class" {
		t.Fatalf("class name: %s %v", name, err)
	}
	result, err := engine.Start(context.Background(), registeredClass{Values: []int{99}}, 2)
	if err != nil || result.Output != 12 {
		t.Fatalf("registered dependencies: %+v %v", result, err)
	}
	if _, err := engine.FlowName(FlowFunc[int, int](nil)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestRegisteredFlowNestedResume(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, reopen func() loopBackend) {
		var rootCalls, outerCalls, leafCalls int
		leaf := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			leafCalls++
			return id + ":" + f.WaitFor[string]("approval", id), nil
		})
		outer := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			outerCalls++
			return f.RunFlow("", leaf, id)
		})
		root := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			rootCalls++
			return f.RunFlow("item", outer, id)
		})
		opts := Options{Flows: map[string]FlowDefinition{"root": root, "outer": outer, "leaf": leaf}}
		ctx := context.Background()
		engine := testEngine(t, store, opts)
		if r, err := engine.Start(ctx, root, "a"); err != nil || r.Status != Suspended {
			t.Fatalf("root: %+v %v", r, err)
		}
		if r, err := engine.Resume(ctx, "item"); err != nil || r.Status != Suspended {
			t.Fatalf("outer: %+v %v", r, err)
		}
		if r, err := engine.Resume(ctx, "item|leaf"); err != nil || r.Status != Suspended {
			t.Fatalf("leaf: %+v %v", r, err)
		}
		// ACK initial wakes so only callback and completion routing remain.
		messages, err := store.Pending(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			if message.Purpose == memory.WakeNotification {
				if err := store.Ack(ctx, message.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
		store = reopen()
		engine = testEngine(t, store, opts)
		if ok, err := engine.Signal(ctx, "item|leaf|approval", "yes"); !ok || err != nil {
			t.Fatalf("signal: %t %v", ok, err)
		}
		messages, err = store.Pending(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			if message.Purpose == memory.WakeNotification && message.ExecutionKey != "item|leaf" {
				t.Fatalf("callback woke ancestor: %+v", message)
			}
		}
		if r, err := engine.Resume(ctx, "item|leaf"); err != nil || r.Status != Completed {
			t.Fatalf("leaf completion: %+v %v", r, err)
		}
		if rootCalls != 1 || outerCalls != 1 || leafCalls != 2 {
			t.Fatalf("ancestor replay: %d %d %d", rootCalls, outerCalls, leafCalls)
		}
		if r, err := engine.Resume(ctx, "item"); err != nil || r.Status != Completed {
			t.Fatalf("outer completion: %+v %v", r, err)
		}
		result, err := engine.Start(ctx, root, "a")
		if err != nil || result.Output != "a:yes" || rootCalls != 2 || outerCalls != 2 || leafCalls != 2 {
			t.Fatalf("result: %+v %v calls=%d/%d/%d", result, err, rootCalls, outerCalls, leafCalls)
		}
	})
}

func TestRegisteredGoFlowJoinAndFailure(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		var calls atomic.Int32
		child := FlowFunc[string, string](func(_ *FlowContext, id string) (string, error) {
			calls.Add(1)
			if id == "bad" {
				return "", errors.New("declined")
			}
			return id, nil
		})
		root := FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			a := f.GoFlow("a", child, "good")
			b := f.GoFlow("b", child, "bad")
			f.GoFlow("", child, "joined") // default name and implicit join
			out, err := a.Wait()
			if err != nil {
				return "", err
			}
			if _, err := b.Wait(); err == nil {
				return "", errors.New("failure was lost")
			}
			return out, nil
		})
		opts := Options{Flows: map[string]FlowDefinition{"root": root, "child": child}}
		ctx := context.Background()
		engine := testEngine(t, store, opts)
		if r, err := engine.Start(ctx, root, 0); err != nil || r.Status != Suspended || len(r.Pending) != 3 {
			t.Fatalf("parallel registration: %+v %v", r, err)
		}
		for _, key := range []string{"a", "b", "child"} {
			r, err := engine.Resume(ctx, key)
			if (key == "b" && (r.Status != Failed || err == nil)) || (key != "b" && (r.Status != Completed || err != nil)) {
				t.Fatalf("%s: %+v %v", key, r, err)
			}
		}
		messages, err := store.Pending(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		completions := 0
		for _, message := range messages {
			if message.Purpose == memory.WakeNotification && message.ExecutionKey == "" {
				completions++
			}
		}
		if completions != 3 {
			t.Fatalf("sibling completion wakes collided: %+v", messages)
		}
		result, err := engine.Start(ctx, root, 0)
		if err != nil || result.Status != Completed || result.Output != "good" || calls.Load() != 3 {
			t.Fatalf("joined: %+v %v calls=%d", result, err, calls.Load())
		}
	})
}

func TestRegisteredFlowFatalChildFailsParent(t *testing.T) {
	for _, mode := range []string{"RunFlow", "GoFlow", "ForEach"} {
		t.Run(mode, func(t *testing.T) {
			eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
				child := FlowFunc[string, string](func(*FlowContext, string) (string, error) { panic("definition bug") })
				root := FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
					var err error
					switch mode {
					case "RunFlow":
						_, err = f.RunFlow("child", child, "a")
					case "GoFlow":
						_, err = f.GoFlow("child", child, "a").Wait()
					default:
						_, err = f.ForEach("child", child, []string{"a"}, identityKey)
					}
					if err != nil {
						return "fallback", nil // must not swallow a child framework failure
					}
					return "ok", nil
				})
				engine := testEngine(t, store, Options{Flows: map[string]FlowDefinition{"root": root, "child": child}})
				ctx := context.Background()
				if r, err := engine.Start(ctx, root, 0); err != nil || r.Status != Suspended {
					t.Fatalf("parent: %+v %v", r, err)
				}
				key := "child"
				if mode == "ForEach" {
					key = "child|item[a]"
				}
				var panicErr *PanicError
				if r, err := engine.Resume(ctx, key); r.Status != Failed || !errors.As(err, &panicErr) {
					t.Fatalf("child: %+v %v", r, err)
				}
				result, err := engine.Start(ctx, root, 0)
				if result.Status != Failed || err == nil || !strings.Contains(err.Error(), "definition bug") {
					t.Fatalf("parent swallowed fatal child: %+v %v", result, err)
				}
			})
		})
	}
}

// A child registered under a deeper MaxFlowDepth fails deterministically after
// the limit is lowered, and so does its owner, instead of retrying forever.
func TestRegisteredChildDepthDriftFailsParent(t *testing.T) {
	for _, mode := range []string{"RunFlow", "ForEach"} {
		t.Run(mode, func(t *testing.T) {
			eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
				leaf := FlowFunc[string, string](func(_ *FlowContext, in string) (string, error) { return in, nil })
				middle := FlowFunc[string, string](func(f *FlowContext, in string) (string, error) {
					if mode == "RunFlow" {
						return f.RunFlow("leaf", leaf, in)
					}
					out, err := f.ForEach("leaf", leaf, []string{in}, identityKey)
					if err != nil {
						return "fallback", nil // must not swallow a framework failure
					}
					return out[0], nil
				})
				root := FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
					return f.RunFlow("middle", middle, "a")
				})
				flows := map[string]FlowDefinition{"root": root, "middle": middle, "leaf": leaf}
				ctx := context.Background()
				deep := testEngine(t, store, Options{Flows: flows})
				if r, err := deep.Start(ctx, root, 0); err != nil || r.Status != Suspended {
					t.Fatalf("root: %+v %v", r, err)
				}
				if r, err := deep.Resume(ctx, "middle"); err != nil || r.Status != Suspended {
					t.Fatalf("middle: %+v %v", r, err)
				}
				// Redeploy with a limit the registered leaf (depth 2) already exceeds.
				shallow := testEngine(t, store, Options{Flows: flows, MaxFlowDepth: 1})
				leafKey := "middle|leaf"
				if mode == "ForEach" {
					leafKey = "middle|leaf|item[a]"
				}
				if r, err := shallow.Resume(ctx, leafKey); r.Status != Failed || !errors.Is(err, ErrInvalid) {
					t.Fatalf("leaf must fail, not retry: %+v %v", r, err)
				}
				r, err := shallow.Resume(ctx, "middle")
				if r.Status != Failed || err == nil || !strings.Contains(err.Error(), "MaxFlowDepth") {
					t.Fatalf("middle swallowed fatal leaf: %+v %v", r, err)
				}
				if r, err := shallow.Resume(ctx, ""); r.Status != Failed || err == nil {
					t.Fatalf("root: %+v %v", r, err)
				}
			})
		})
	}
}

// Resume needs neither the root Flow nor its input: both come from Start.
func TestResumeRootFromDefinition(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, reopen func() loopBackend) {
		ctx := context.Background()
		root := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			return id + ":" + f.WaitFor[string]("approval", id), nil
		})
		opts := Options{Flows: map[string]FlowDefinition{"root": root}}
		engine := testEngine(t, store, opts)
		if r, err := engine.Resume(ctx, ""); !errors.Is(err, ErrNotFound) || r.Status != Running {
			t.Fatalf("before start: %+v %v", r, err)
		}
		if r, err := engine.Start(ctx, root, "o1"); err != nil || r.Status != Suspended {
			t.Fatalf("start: %+v %v", r, err)
		}
		if ok, err := engine.Signal(ctx, "approval", "yes"); !ok || err != nil {
			t.Fatalf("signal: %t %v", ok, err)
		}
		store = reopen()
		r, err := testEngine(t, store, opts).Resume(ctx, "")
		if err != nil || r.Status != Completed || string(r.Output) != `"o1:yes"` {
			t.Fatalf("resume: %+v %v", r, err)
		}
		// Starting again with another input is drift, not a second run.
		if r, err := testEngine(t, store, opts).Start(ctx, root, "o2"); !errors.Is(err, ErrChanged) || r.Status != Failed {
			t.Fatalf("restart: %+v %v", r, err)
		}
		// A worker missing the root registration cannot resume it.
		bare := testEngine(t, store, Options{Flows: map[string]FlowDefinition{"other": FlowFunc[int, int](func(*FlowContext, int) (int, error) { return 0, nil })}})
		if _, err := bare.Resume(ctx, ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unregistered root: %v", err)
		}
	})
}

// Actions inside a resumed child are fenced by the owner's lease instead of their own.
func TestRegisteredFlowActionsUseOwnerLease(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		ctx := context.Background()
		child := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			return f.Run("step", Pure(func(id string) string { return id + "!" }), id)
		})
		root := FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) { return f.RunFlow("child", child, "a") })
		engine := testEngine(t, store, Options{Flows: map[string]FlowDefinition{"root": root, "child": child}})
		if r, err := engine.Start(ctx, root, 0); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		// Another worker's Action lease on the step does not block the owner.
		other, err := store.Claim(ctx, "child|step", time.Minute)
		if err != nil || other == nil {
			t.Fatalf("%+v %v", other, err)
		}
		if r, err := engine.Resume(ctx, "child"); err != nil || r.Status != Completed {
			t.Fatalf("owner lease: %+v %v", r, err)
		}
		if r, err := engine.Start(ctx, root, 0); err != nil || r.Output != "a!" {
			t.Fatalf("%+v %v", r, err)
		}
		// A lease for some other key cannot commit the step.
		forged := &Record{Key: "child|x", ExecutionKey: "child", Kind: ActionKind, Status: Running}
		if _, err := store.Commit(ctx, *other, forged, 0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unrelated lease committed: %v", err)
		}
		// A stale owner lease is rejected at the storage boundary.
		owner, err := store.Claim(ctx, "child", time.Minute)
		if err != nil || owner == nil {
			t.Fatalf("%+v %v", owner, err)
		}
		if err := store.Release(ctx, *owner); err != nil {
			t.Fatal(err)
		}
		stale := &Record{Key: "child|y", ExecutionKey: "child", Kind: ActionKind, Status: Running}
		if _, err := store.Commit(ctx, *owner, stale, 0); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale owner lease committed: %v", err)
		}
	})
}

type lostFlowCommit struct {
	loopBackend
	lose bool
}

func (s *lostFlowCommit) CommitFlow(ctx context.Context, lease Lease, item *FlowExecution, expected int64) (*FlowExecution, error) {
	result, err := s.loopBackend.CommitFlow(ctx, lease, item, expected)
	if err == nil && s.lose {
		s.lose = false
		return nil, errors.New("response lost")
	}
	return result, err
}

func TestRegisteredFlowCommitFencingAndRecovery(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, reopen func() loopBackend) {
		ctx := context.Background()
		calls := 0
		child := FlowFunc[string, string](func(_ *FlowContext, in string) (string, error) {
			calls++
			return in, nil
		})
		input := "first"
		root := FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) { return f.RunFlow("child", child, input) })
		opts := Options{Flows: map[string]FlowDefinition{"root": root, "child": child}}
		engine := testEngine(t, store, opts)
		if r, err := engine.Start(ctx, root, 0); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		input = "changed"
		if r, err := engine.Start(ctx, root, 0); !errors.Is(err, ErrChanged) || r.Status != Failed {
			t.Fatalf("input drift: %+v %v", r, err)
		}
		input = "first"
		item, err := store.GetFlow(ctx, "child")
		if err != nil {
			t.Fatal(err)
		}
		old, err := store.Claim(ctx, item.Key, time.Minute)
		if err != nil || old == nil {
			t.Fatalf("%+v %v", old, err)
		}
		if err := store.Release(ctx, *old); err != nil {
			t.Fatal(err)
		}
		current, err := store.Claim(ctx, item.Key, time.Minute)
		if err != nil || current == nil {
			t.Fatalf("%+v %v", current, err)
		}
		item.Status, item.Output = Completed, json.RawMessage(`"forged"`)
		if _, err := store.CommitFlow(ctx, *old, item, item.Version); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale lease: %v", err)
		}
		if _, err := store.CommitFlow(ctx, *current, item, item.Version+1); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale version: %v", err)
		}
		if err := store.Release(ctx, *current); err != nil {
			t.Fatal(err)
		}
		lost := &lostFlowCommit{loopBackend: store, lose: true}
		if r, err := testEngine(t, lost, opts).Resume(ctx, "child"); err == nil || r.Status != Running {
			t.Fatalf("lost commit: %+v %v", r, err)
		}
		store = reopen()
		engine = testEngine(t, store, opts)
		for range 3 {
			if r, err := engine.Resume(ctx, "child"); err != nil || r.Status != Completed || string(r.Output) != `"first"` {
				t.Fatalf("recovery: %+v %v", r, err)
			}
		}
		if calls != 1 {
			t.Fatalf("committed child reran %d times", calls)
		}
		messages, err := store.Pending(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, message := range messages {
			if strings.Contains(message.ID, "|FLOW_PARENT_WAKE|") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("duplicate completion: %+v", messages)
		}
		if r, err := engine.Start(ctx, root, 0); err != nil || r.Output != "first" {
			t.Fatalf("parent: %+v %v", r, err)
		}
	})
}

func TestDebuggerRegisteredStructRoot(t *testing.T) {
	store, _ := loopStore(t, true)
	client := store.(*debugger.Client)
	server, _, _ := strings.Cut(client.URL(), "/#/")
	result, err := debugger.Run(context.Background(), registeredClass{Values: []int{99}}, 1, debugger.Options{
		Server: server,
		RunID:  "registered-class",
		Engine: Options{Flows: map[string]FlowDefinition{"class": registeredClass{Values: []int{10}}}},
		Logf:   func(string, ...any) {},
	})
	if err != nil || result.Output != 11 {
		t.Fatalf("%+v %v", result, err)
	}
}

type customFlowKeys struct{ loopBackend }

func (*customFlowKeys) BuildKey(path Path) string {
	if len(path) == 1 && path[0].Name == "second" {
		return "first|$PARENT"
	}
	return DefaultKey(path)
}

func TestRegisteredFlowCustomKeysDoNotReserveParentAddresses(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, backend loopBackend, _ func() loopBackend) {
		store := &customFlowKeys{loopBackend: backend}
		child := FlowFunc[int, int](func(_ *FlowContext, n int) (int, error) { return n, nil })
		root := FlowFunc[int, int](func(f *FlowContext, n int) (int, error) {
			a, err := f.RunFlow("first", child, n)
			if err != nil {
				return 0, err
			}
			b, err := f.RunFlow("second", child, n+1)
			return a + b, err
		})
		engine := testEngine(t, store, Options{Flows: map[string]FlowDefinition{"root": root, "child": child}})
		ctx := context.Background()
		for _, key := range []string{"first", "first|$PARENT"} {
			if r, err := engine.Start(ctx, root, 1); err != nil || r.Status != Suspended {
				t.Fatalf("parent: %+v %v", r, err)
			}
			if r, err := engine.Resume(ctx, key); err != nil || r.Status != Completed {
				t.Fatalf("child: %+v %v", r, err)
			}
		}
		if r, err := engine.Start(ctx, root, 1); err != nil || r.Output != 3 {
			t.Fatalf("result: %+v %v", r, err)
		}
	})
}
