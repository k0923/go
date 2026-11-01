package tests

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/debugger"
	"github.com/k0923/go/cflow/memory"
)

type loopBackend interface {
	Infrastructure
	FlowInfrastructure
	memory.Outbox
}

func loopStore(t *testing.T, sqlite bool) (loopBackend, func() loopBackend) {
	t.Helper()
	if !sqlite {
		store := memory.New()
		return store, func() loopBackend { return store }
	}
	path := filepath.Join(t.TempDir(), "loop.db")
	var store *debugger.Store
	var server *httptest.Server
	open := func() loopBackend {
		if server != nil {
			server.Close()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		}
		var err error
		store, err = debugger.OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		server = httptest.NewServer(debugger.NewServer(store))
		client, err := debugger.NewClient(server.URL, "loop-test")
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	backend := open()
	t.Cleanup(func() { server.Close(); store.Close() })
	return backend, open
}

func eachLoopStore(t *testing.T, fn func(*testing.T, loopBackend, func() loopBackend)) {
	t.Helper()
	for _, sqlite := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "sqlite_http"}[sqlite], func(t *testing.T) {
			store, reopen := loopStore(t, sqlite)
			fn(t, store, reopen)
		})
	}
}

func identityKey(id string) string { return id }

// Drain durable messages, recreating the engine for every delivery.
func drainLoop[I, O any](t *testing.T, store loopBackend, opts Options, wf *flowFixture[I, O], input I) Result[O] {
	t.Helper()
	for range 100 {
		messages, err := store.Pending(context.Background(), time.Now())
		if err != nil || len(messages) == 0 {
			t.Fatalf("lost wake: %v %v", messages, err)
		}
		for _, msg := range messages {
			engine := testEngine(t, store, opts)
			switch msg.Purpose {
			case memory.RequestNotification:
				if ok, err := engine.Signal(context.Background(), msg.Key, "approved"); !ok || err != nil {
					t.Fatalf("signal: %t %v", ok, err)
				}
			case memory.WakeNotification:
				if msg.ExecutionKey == "" {
					result, err := engine.Start(context.Background(), wf, input)
					if err != nil {
						t.Fatal(err)
					}
					if result.Status == Completed {
						if err := store.Ack(context.Background(), msg.ID); err != nil {
							t.Fatal(err)
						}
						return result
					}
				} else {
					result, err := engine.Resume(context.Background(), msg.ExecutionKey)
					if result.Status == Running || (err != nil && result.Status != Failed) {
						t.Fatalf("child: %+v %v", result, err)
					}
				}
			}
			if err := store.Ack(context.Background(), msg.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("loop did not finish")
	return Result[O]{}
}

type loopOrder struct{ ID string }

func (loopOrder) StepKey() string { panic("ForEach must use the supplied key") }

func TestForEachIndependentResume(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, reopen func() loopBackend) {
		ctx := context.Background()
		var calls []string
		rootCalls, bodyCalls := 0, 0
		body := FlowFunc[loopOrder, string](func(f *FlowContext, order loopOrder) (string, error) {
			bodyCalls++
			value, err := f.Run("submit", AsAction(func(_ context.Context, id string) (string, error) {
				calls = append(calls, id)
				return id, nil
			}), order.ID)
			if err != nil {
				return "", err
			}
			return value + ":" + f.WaitFor[string]("approval", order.ID), nil
		})
		opts := Options{Flows: map[string]FlowDefinition{"order": body}}
		wf := namedDefinition("batch", FlowFunc[[]loopOrder, []string](func(f *FlowContext, in []loopOrder) ([]string, error) {
			rootCalls++
			return f.ForEach("orders", body, in, func(o loopOrder) string { return o.ID })
		}))
		input := []loopOrder{{"订单|[2]"}, {"b"}}
		first, err := testEngine(t, store, opts).Start(ctx, wf, input)
		if err != nil || first.Status != Suspended || bodyCalls != 0 {
			t.Fatalf("initial: %+v %v calls=%d", first, err, bodyCalls)
		}
		if r, err := testEngine(t, store, opts).Resume(ctx, "orders|item[b]"); !errors.Is(err, ErrConflict) || r.Status != Running {
			t.Fatalf("queued: %+v %v", r, err)
		}
		key := DefaultKey(Path{{Name: "orders"}, {Name: "item", ID: input[0].ID}})
		r, err := testEngine(t, store, opts).Resume(ctx, key)
		if err != nil || r.Status != Suspended || len(r.Pending) != 1 || r.Pending[0].ExecutionKey != key {
			t.Fatalf("suspended: %+v %v", r, err)
		}
		store = reopen() // Close and reopen the real SQLite database and HTTP server.
		result := drainLoop(t, store, opts, wf, input)
		if !slices.Equal(result.Output, []string{"订单|[2]:approved", "b:approved"}) ||
			rootCalls != 2 || !slices.Equal(calls, []string{"订单|[2]", "b"}) {
			t.Fatalf("result=%+v root=%d actions=%v", result, rootCalls, calls)
		}
		before := bodyCalls
		for range 3 {
			r, err := testEngine(t, store, opts).Resume(ctx, key)
			if err != nil || r.Status != Completed {
				t.Fatalf("duplicate: %+v %v", r, err)
			}
		}
		state, err := store.GetLoop(ctx, "orders")
		if err != nil || state.CompletedCount != 2 || bodyCalls != before {
			t.Fatalf("recount/replay: %+v %v calls=%d", state, err, bodyCalls)
		}
		messages, err := store.Pending(ctx, time.Now())
		if err != nil || len(messages) != 0 {
			t.Fatalf("duplicate wake: %v %v", messages, err)
		}
	})
}

func TestForEachValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  func(string) string
		mode string
	}{
		{name: "nil key"},
		{name: "empty second", key: func(id string) string {
			if id == "b" {
				return ""
			}
			return id
		}},
		{name: "duplicate", key: func(string) string { return "same" }},
		{name: "oversized", key: func(id string) string {
			if id == "b" {
				return strings.Repeat("x", MaxIDLength+1)
			}
			return id
		}},
		{name: "UTF8", key: func(string) string { return string([]byte{255}) }},
		{name: "panic", key: func(id string) string {
			if id == "b" {
				panic("bad key")
			}
			return id
		}},
		{name: "nil body", key: identityKey, mode: "nil"},
		{name: "raw body", key: identityKey, mode: "raw"},
		{name: "unregistered", key: identityKey, mode: "unregistered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
				raw := FlowFunc[string, string](func(*FlowContext, string) (string, error) { t.Fatal("body ran"); return "", nil })
				def := FlowFunc[string, string](func(f *FlowContext, in string) (string, error) { return raw.Run(f, in) })
				var body Flow[string, string] = def
				opts := Options{Flows: map[string]FlowDefinition{"item": def}}
				switch tc.mode {
				case "nil":
					body = FlowFunc[string, string](nil)
				case "raw":
					body = raw
				case "unregistered":
					opts.Flows = nil
				}
				wf := namedDefinition("batch", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
					return f.ForEach("orders", body, []string{"a", "b"}, tc.key)
				}))
				r, err := testEngine(t, store, opts).Start(context.Background(), wf, 0)
				if r.Status != Failed || !errors.Is(err, ErrInvalid) {
					t.Fatalf("%+v %v", r, err)
				}
				loop, err := store.GetLoop(context.Background(), "orders")
				if err != nil || loop != nil {
					t.Fatalf("partial registration: %+v %v", loop, err)
				}
				item, err := store.GetFlow(context.Background(), "orders|item[a]")
				if err != nil || item != nil {
					t.Fatalf("partial child: %+v %v", item, err)
				}
			})
		})
	}
}

func TestForEachEmptyAndInputDrift(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		body := FlowFunc[string, string](func(_ *FlowContext, in string) (string, error) { return in, nil })
		opts := Options{Flows: map[string]FlowDefinition{"item": body}}
		items := []string{}
		wf := namedDefinition("batch", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
			return f.ForEach("orders", body, items, identityKey)
		}))
		for _, empty := range [][]string{nil, {}} {
			items = empty
			r, err := testEngine(t, store, opts).Start(context.Background(), wf, 0)
			if err != nil || r.Status != Completed || r.Output == nil || len(r.Output) != 0 {
				t.Fatalf("%+v %v", r, err)
			}
		}
		items = []string{"a"}
		r, err := testEngine(t, store, opts).Start(context.Background(), wf, 0)
		if r.Status != Failed || !errors.Is(err, ErrChanged) {
			t.Fatalf("drift: %+v %v", r, err)
		}
	})
}

func TestForEachFailureAndRetry(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(map[bool]string{false: "retryable", true: "permanent"}[permanent], func(t *testing.T) {
			eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
				calls := map[string]int{}
				cause := errors.New("submit failed")
				body := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
					return f.Run("submit", AsAction(func(_ context.Context, id string) (string, error) {
						calls[id]++
						if id == "b" && (permanent || calls[id] == 1) {
							if permanent {
								return "", Permanent(cause)
							}
							return "", cause
						}
						return id, nil
					}), id)
				})
				var caught error
				opts := Options{Flows: map[string]FlowDefinition{"item": body}}
				wf := namedDefinition("batch", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
					out, err := f.ForEach("orders", body, []string{"a", "b", "c"}, identityKey)
					caught = err
					return out, nil
				}))
				ctx := context.Background()
				engine := testEngine(t, store, opts)
				if r, err := engine.Start(ctx, wf, 0); err != nil || r.Status != Suspended {
					t.Fatalf("%+v %v", r, err)
				}
				if r, err := engine.Resume(ctx, "orders|item[a]"); err != nil || r.Status != Completed {
					t.Fatalf("%+v %v", r, err)
				}
				r, err := engine.Resume(ctx, "orders|item[b]")
				wantStatus := Running
				if permanent {
					wantStatus = Failed
				}
				if r.Status != wantStatus || err == nil {
					t.Fatalf("%+v %v", r, err)
				}
				state, err := store.GetLoop(ctx, "orders")
				if err != nil || state.CompletedCount != 1 || calls["c"] != 0 {
					t.Fatalf("%+v %v %v", state, err, calls)
				}
				result := drainLoop(t, store, opts, wf, 0)
				if permanent {
					var itemErr *ForEachError
					var actionErr *ActionError
					if result.Output != nil || !errors.As(caught, &itemErr) ||
						itemErr.Index != 1 || itemErr.Key != "orders|item[b]" ||
						!errors.As(caught, &actionErr) || actionErr.Key != "orders|item[b]|submit" || calls["c"] != 0 {
						t.Fatalf("%+v caught=%v calls=%v", result, caught, calls)
					}
				} else if caught != nil || !slices.Equal(result.Output, []string{"a", "b", "c"}) || calls["b"] != 2 {
					t.Fatalf("%+v caught=%v calls=%v", result, caught, calls)
				}
				if calls["a"] != 1 {
					t.Fatal(calls)
				}
			})
		})
	}
}

func TestForEachNestedAndAsync(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		done := make(chan struct{})
		leaf := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			f.Go("notify", AsAction(func(context.Context, string) (string, error) { close(done); return "ok", nil }), id)
			return f.WaitFor[string]("approval", id), nil
		})
		outer := FlowFunc[string, []string](func(f *FlowContext, id string) ([]string, error) {
			return f.ForEach("inner", leaf, []string{id}, identityKey)
		})
		opts := Options{Flows: map[string]FlowDefinition{"leaf": leaf, "outer": outer}, MaxFlowDepth: 2}
		wf := namedDefinition("batch", FlowFunc[int, [][]string](func(f *FlowContext, _ int) ([][]string, error) {
			return f.ForEach("outer", outer, []string{"a"}, identityKey)
		}))
		r, err := testEngine(t, store, opts).Start(context.Background(), wf, 0)
		if err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		result := drainLoop(t, store, opts, wf, 0)
		if len(result.Output) != 1 || !slices.Equal(result.Output[0], []string{"approved"}) {
			t.Fatalf("%+v", result)
		}
		select {
		case <-done:
		default:
			t.Fatal("async branch not joined")
		}
	})
}

func TestForEachDepthLimit(t *testing.T) {
	store := memory.New()
	leaf := FlowFunc[string, string](func(_ *FlowContext, id string) (string, error) { return id, nil })
	outer := FlowFunc[string, []string](func(f *FlowContext, id string) ([]string, error) {
		return f.ForEach("inner", leaf, []string{id}, identityKey)
	})
	opts := Options{Flows: map[string]FlowDefinition{"leaf": leaf, "outer": outer}, MaxFlowDepth: 1}
	wf := namedDefinition("batch", FlowFunc[int, [][]string](func(f *FlowContext, _ int) ([][]string, error) {
		return f.ForEach("outer", outer, []string{"a"}, identityKey)
	}))
	engine := testEngine(t, store, opts)
	if r, err := engine.Start(context.Background(), wf, 0); err != nil || r.Status != Suspended {
		t.Fatalf("%+v %v", r, err)
	}
	r, err := engine.Resume(context.Background(), "outer|item[a]")
	if r.Status != Failed || !errors.Is(err, ErrInvalid) {
		t.Fatalf("%+v %v", r, err)
	}
}
