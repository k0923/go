package debugger

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/k0923/go/workflow"
)

// Options configures Run. Zero values are usable.
type Options struct {
	// Server is the debugger base URL; empty uses DefaultServer.
	Server string
	// RunID attaches to a run. Empty generates "<workflow>-<random>"; reuse an
	// ID to resume a run after restarting your process.
	RunID string
	// Engine options. Observer and Now are respected; LeaseTTL defaults to 30s.
	Engine workflow.Options
	// RetryDelay paces drives after a retryable error. Default 2s.
	RetryDelay time.Duration
	// Logf receives progress lines; nil uses log.Printf. Use a no-op to silence.
	Logf func(format string, args ...any)
	// OnAttach is called once the run is registered, e.g. to open the UI.
	OnAttach func(runURL string)
}

// Run attaches a workflow to the debugger and keeps driving it until it
// COMPLETES, FAILS, or ctx is cancelled. Suspended runs wait for a WAKE: a
// WaitFor answered in the UI, a due Sleep, or a manual "Re-drive".
//
// Replays happen on every Drive, so it is expected to see completed actions
// marked as replayed in the timeline.
func Run[I, O any](ctx context.Context, definition workflow.Workflow[I, O], input I, options Options) (workflow.Result[O], error) {
	var zero workflow.Result[O]
	logf := options.Logf
	if logf == nil {
		logf = log.Printf
	}
	if options.RetryDelay <= 0 {
		options.RetryDelay = 2 * time.Second
	}
	runID := options.RunID
	if runID == "" {
		runID = fmt.Sprintf("%s-%d", definition.Name, time.Now().UnixMilli())
	}
	client, err := NewClient(options.Server, runID)
	if err != nil {
		return zero, err
	}
	rawInput, err := json.Marshal(input)
	if err != nil {
		return zero, err
	}
	if _, err = client.register(ctx, RunSpec{Workflow: definition.Name, Version: definition.Version, Input: rawInput}); err != nil {
		return zero, fmt.Errorf("attach to debugger %s: %w", client.base, err)
	}
	logf("[wfdebug] run %s attached: %s", runID, client.URL())
	if options.OnAttach != nil {
		options.OnAttach(client.URL())
	}

	engine, err := workflow.NewEngine(client, options.Engine)
	if err != nil {
		return zero, err
	}
	events := newForwarder(client)
	unsubscribe := engine.Subscribe(events.add)
	defer unsubscribe()
	defer events.close()

	for {
		result, driveErr := engine.Drive(ctx, definition, input)
		events.flush()
		report := Report{Status: string(result.Status), Pending: result.Pending}
		if driveErr != nil {
			report.Error = driveErr.Error()
			if result.Status != workflow.Failed {
				report.Status = RunRetrying
			}
		}
		if result.Status == workflow.Completed {
			report.Output, _ = json.Marshal(result.Output)
		}
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := client.Report(reportCtx, report); err != nil {
			logf("[wfdebug] report failed: %v", err)
		}
		cancel()

		switch {
		case ctx.Err() != nil:
			return result, ctx.Err()
		case result.Status == workflow.Completed:
			logf("[wfdebug] run %s COMPLETED", runID)
			return result, nil
		case result.Status == workflow.Failed:
			logf("[wfdebug] run %s FAILED: %v", runID, driveErr)
			return result, driveErr
		case driveErr != nil:
			logf("[wfdebug] run %s retryable error, re-driving in %s: %v", runID, options.RetryDelay, driveErr)
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(options.RetryDelay):
			}
			continue
		}
		keys := make([]string, len(result.Pending))
		for i, pending := range result.Pending {
			keys[i] = string(pending.Kind) + ":" + pending.Key
		}
		logf("[wfdebug] run %s SUSPENDED on %v", runID, keys)
		if err := waitForWake(ctx, client); err != nil {
			return result, err
		}
	}
}

// waitForWake blocks until at least one WAKE is due, then acknowledges every
// due wake: a single Drive replays the whole definition and consumes them all.
func waitForWake(ctx context.Context, client *Client) error {
	for {
		notifications, err := client.WaitWake(ctx, 20*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if len(notifications) == 0 {
			continue
		}
		for _, notification := range notifications {
			if err = client.Ack(ctx, notification.ID); err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
		return nil
	}
}

// forwarder batches engine events to the server off the Drive path, because
// listeners are synchronous and must return promptly.
type forwarder struct {
	client *Client
	mu     sync.Mutex
	buffer []json.RawMessage
	signal chan chan struct{}
	done   chan struct{}
	wg     sync.WaitGroup
}

func newForwarder(client *Client) *forwarder {
	f := &forwarder{client: client, signal: make(chan chan struct{}, 1), done: make(chan struct{})}
	f.wg.Add(1)
	go f.loop()
	return f
}

func (f *forwarder) add(_ context.Context, event workflow.Event) {
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.buffer = append(f.buffer, raw)
	f.mu.Unlock()
}

func (f *forwarder) send() {
	f.mu.Lock()
	batch := f.buffer
	f.buffer = nil
	f.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = f.client.PushEvents(ctx, batch)
}

func (f *forwarder) loop() {
	defer f.wg.Done()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-f.done:
			f.send()
			return
		case ack := <-f.signal:
			f.send()
			close(ack)
		case <-ticker.C:
			f.send()
		}
	}
}

// flush sends buffered events and waits, so a Drive's events precede its report.
func (f *forwarder) flush() {
	ack := make(chan struct{})
	select {
	case f.signal <- ack:
		<-ack
	case <-f.done:
	}
}

func (f *forwarder) close() {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	f.wg.Wait()
}
