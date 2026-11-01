package tests

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"sync/atomic"

	. "github.com/k0923/go/cflow"
)

// Fixtures bundle a root and its registry for tests of Actions and waits.
// They are test data; production registration remains immutable in Options.
type flowFixture[I, O any] struct {
	FlowBase[I, O]
	Name     string
	Body     Flow[I, O]
	Bindings map[string]FlowDefinition
}

func namedDefinition[I, O any](name string, body Flow[I, O]) *flowFixture[I, O] {
	return &flowFixture[I, O]{Name: name, Body: body}
}

func (f *flowFixture[I, O]) Run(ctx *FlowContext, input I) (O, error) {
	return f.Body.Run(ctx, input)
}

func (f *flowFixture[I, O]) registry() (string, map[string]FlowDefinition) {
	return f.Name, f.Bindings
}

type fixtureKey struct {
	flow FlowDefinition
	name string
}
type testSubscription struct {
	listener EventListener
	types    []EventType
	active   atomic.Bool
}

// testDriver creates the immutable registry before driving each fixture.
// Tests with explicit Options.Flows use the configured Engine directly.
type testDriver struct {
	*Engine
	store         Infrastructure
	options       Options
	mu            sync.Mutex
	active        *Engine
	engines       map[fixtureKey]*Engine
	subscriptions []*testSubscription
}

func (d *testDriver) current() *Engine {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active
}

func (d *testDriver) Start[I, O any](ctx context.Context, flow Flow[I, O], input I) (Result[O], error) {
	fixture, ok := flow.(interface {
		registry() (string, map[string]FlowDefinition)
	})
	if !ok {
		return d.Engine.Start(ctx, flow, input)
	}
	name, bindings := fixture.registry()
	key := fixtureKey{flow: flow, name: name}
	d.mu.Lock()
	engine := d.engines[key]
	if engine == nil {
		opts := d.options
		opts.Flows = make(map[string]FlowDefinition)
		for k, v := range d.options.Flows {
			opts.Flows[k] = v
		}
		for k, v := range bindings {
			opts.Flows[k] = v
		}
		opts.Flows[name] = flow
		var err error
		engine, err = NewEngine(d.store, opts)
		if err != nil {
			d.mu.Unlock()
			return Result[O]{Status: Running}, err
		}
		for _, sub := range d.subscriptions {
			installSubscription(engine, sub)
		}
		d.engines[key] = engine
	}
	d.active = engine
	d.mu.Unlock()
	if len(bindings) == 0 {
		return engine.Start(ctx, flow, input)
	}
	return scheduleFixture(ctx, engine, flow, input)
}

// The test scheduler drives each durable child independently. Production
// Engine.Start/Resume still perform exactly one execution boundary.
func scheduleFixture[I, O any](ctx context.Context, engine *Engine, flow Flow[I, O], input I) (Result[O], error) {
	return scheduleBoundary(func() (Result[O], error) { return engine.Start(ctx, flow, input) }, func(key string) (Result[json.RawMessage], error) {
		return scheduleChild(ctx, engine, key)
	})
}

func scheduleChild(ctx context.Context, engine *Engine, key string) (Result[json.RawMessage], error) {
	return scheduleBoundary(func() (Result[json.RawMessage], error) { return engine.Resume(ctx, key) }, func(key string) (Result[json.RawMessage], error) {
		return scheduleChild(ctx, engine, key)
	})
}

func scheduleBoundary[O any](drive func() (Result[O], error), child func(string) (Result[json.RawMessage], error)) (Result[O], error) {
	for {
		result, err := drive()
		if err != nil || result.Status != Suspended {
			return result, err
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		var pending []Pending
		var faults []error
		progress := false
		for _, p := range result.Pending {
			if p.Kind != FlowKind {
				mu.Lock()
				pending = append(pending, p)
				mu.Unlock()
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := child(p.Key)
				mu.Lock()
				defer mu.Unlock()
				if r.Status == Completed || r.Status == Failed {
					progress = true
				} else {
					pending = append(pending, r.Pending...)
					if err != nil {
						faults = append(faults, err)
					}
				}
			}()
		}
		wg.Wait()
		sort.Slice(pending, func(i, j int) bool { return pending[i].Key < pending[j].Key })
		if len(faults) > 0 {
			return Result[O]{Status: Running, Pending: pending}, errors.Join(faults...)
		}
		if progress {
			continue
		}
		result.Pending = pending
		return result, nil
	}
}

func installSubscription(engine *Engine, sub *testSubscription) {
	engine.Subscribe(func(ctx context.Context, event Event) {
		if sub.active.Load() {
			sub.listener(ctx, event)
		}
	}, sub.types...)
}

func (d *testDriver) Subscribe(listener EventListener, types ...EventType) func() {
	if listener == nil {
		return func() {}
	}
	sub := &testSubscription{listener: listener, types: types}
	sub.active.Store(true)
	d.mu.Lock()
	d.subscriptions = append(d.subscriptions, sub)
	installSubscription(d.Engine, sub)
	for _, engine := range d.engines {
		installSubscription(engine, sub)
	}
	d.mu.Unlock()
	return func() { sub.active.Store(false) }
}

func (d *testDriver) Resume(ctx context.Context, key string) (Result[json.RawMessage], error) {
	return d.current().Resume(ctx, key)
}

func (d *testDriver) Signal[T any](ctx context.Context, key string, response T) (bool, error) {
	return d.current().Signal(ctx, key, response)
}

func (d *testDriver) ResolveSignal[T any](ctx context.Context, key string, version int64, response T) (bool, error) {
	return d.current().ResolveSignal(ctx, key, version, response)
}

func (d *testDriver) FailWait(ctx context.Context, key string, version int64, err error) (bool, error) {
	return d.current().FailWait(ctx, key, version, err)
}
