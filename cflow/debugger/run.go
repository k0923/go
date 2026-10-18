package debugger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

// Options configures Run. Zero values are usable.
// Options 配置 Run。零值可直接使用。
type Options struct {
	// Server is the debugger base URL; empty uses DefaultServer.
	// Server 是 debugger 的基础 URL；为空时使用 DefaultServer。
	Server string
	// RunID attaches to a run. Empty generates "<workflow>-<random>"; reuse an
	// ID to resume a run after restarting your process.
	// RunID 绑定到一个运行。为空时生成 "<workflow>-<random>"；复用 ID 可在进程重启后恢复运行。
	RunID string
	// Engine options. Observer and Now are respected; LeaseTTL defaults to 30s.
	// Engine 选项。Observer 和 Now 会被保留；LeaseTTL 默认 30s。
	Engine cflow.Options
	// RetryDelay paces drives after a retryable error. Default 2s.
	// RetryDelay 控制可重试错误后的 drive 间隔。默认 2s。
	RetryDelay time.Duration
	// Logf receives progress lines; nil uses log.Printf. Use a no-op to silence.
	// Logf 接收进度日志；nil 时使用 log.Printf。可传入空操作以静默输出。
	Logf func(format string, args ...any)
	// OnAttach is called once the run is registered, e.g. to open the UI.
	// OnAttach 会在运行注册后调用，例如用于打开 UI。
	OnAttach func(runURL string)
}

// Run attaches a workflow to the debugger and keeps driving it until it
// COMPLETES, FAILS, or ctx is cancelled. Suspended runs wait for a WAKE: a
// WaitFor answered in the UI, a due Sleep, or a manual "Re-drive".
//
// Replays happen on every Drive, so it is expected to see completed actions
// marked as replayed in the timeline.
// Run 将工作流连接到 debugger，并持续驱动它，直到 COMPLETED、FAILED 或 ctx 被取消。
// 挂起的运行会等待 WAKE：UI 中回答的 WaitFor、到期的 Sleep，或手动 "Re-drive"。
//
// 每次 Drive 都会发生回放，因此在时间线中看到已完成的 action 被标记为 replayed
// 是预期行为。
func Run[I, O any](ctx context.Context, definition cflow.Workflow[I, O], input I, options Options) (cflow.Result[O], error) {
	var zero cflow.Result[O]
	logf := options.Logf
	if logf == nil {
		logf = log.Printf
	}
	if options.RetryDelay <= 0 {
		options.RetryDelay = 2 * time.Second
	}
	runID := options.RunID
	if runID == "" {
		runID = definition.Name + "-" + uuid.NewString()
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

	engine, err := cflow.NewEngine(client, options.Engine)
	if err != nil {
		return zero, err
	}
	events := newForwarder(client, logf)
	unsubscribe := engine.Subscribe(events.add)
	defer unsubscribe()
	defer events.close()

	var wakes []memory.Notification
	for {
		due, pollErr := client.WaitWake(ctx, time.Nanosecond)
		if errors.Is(pollErr, cflow.ErrNotFound) {
			return zero, pollErr
		}
		seen := make(map[string]bool, len(wakes))
		for _, notification := range wakes {
			seen[notification.ID] = true
		}
		for _, notification := range due {
			if !seen[notification.ID] {
				wakes = append(wakes, notification)
				seen[notification.ID] = true
			}
		}
		result, driveErr := engine.Drive(ctx, definition, input)
		events.flush()
		var ackErr error
		if ctx.Err() == nil && (driveErr == nil || result.Status == cflow.Failed) {
			var remaining []memory.Notification
			for _, notification := range wakes {
				ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := client.Ack(ackCtx, notification.ID)
				cancel()
				if err != nil {
					remaining = append(remaining, notification)
					ackErr = errors.Join(ackErr, err)
				}
			}
			wakes = remaining
		}
		report := Report{Status: string(result.Status), Pending: result.Pending}
		if driveErr != nil {
			report.Error = driveErr.Error()
			if result.Status != cflow.Failed {
				report.Status = RunRetrying
			}
		}
		if result.Status == cflow.Completed {
			report.Output, _ = json.Marshal(result.Output)
		}
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := client.Report(reportCtx, report); err != nil {
			logf("[wfdebug] report failed: %v", err)
		}
		cancel()
		if ackErr != nil && ctx.Err() == nil {
			if errors.Is(ackErr, cflow.ErrNotFound) {
				return result, ackErr
			}
			logf("[wfdebug] wake acknowledgement failed, retrying: %v", ackErr)
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(options.RetryDelay):
			}
			continue
		}

		switch {
		case ctx.Err() != nil:
			return result, ctx.Err()
		case result.Status == cflow.Completed:
			logf("[wfdebug] run %s COMPLETED", runID)
			return result, nil
		case result.Status == cflow.Failed:
			logf("[wfdebug] run %s FAILED: %v", runID, driveErr)
			return result, driveErr
		case errors.Is(driveErr, cflow.ErrNotFound):
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
		wakes, err = waitForWake(ctx, client, result.Pending)
		if err != nil {
			return result, err
		}
	}
}

// Return wakes without acknowledging them; the next successful Drive owns ACK.
// Recheck durable waits so a redundant worker cannot retain a stale suspension.
// 返回唤醒但不确认，由下一次成功 Drive 确认。复核持久等待，
// 避免冗余 worker 一直持有陈旧的挂起快照。
func waitForWake(ctx context.Context, client *Client, pending []cflow.Pending) ([]memory.Notification, error) {
	for {
		for _, wait := range pending {
			record, err := client.Get(ctx, wait.Key)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, cflow.ErrNotFound) {
				return nil, err
			}
			if err == nil && record != nil && record.Status != cflow.Suspended {
				return nil, nil
			}
		}
		notifications, err := client.WaitWake(ctx, 2*time.Second)
		if err != nil {
			if errors.Is(err, cflow.ErrNotFound) {
				return nil, err
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if len(notifications) == 0 {
			continue
		}
		return notifications, nil
	}
}

// forwarder batches engine events to the server off the Drive path, because
// listeners are synchronous and must return promptly.
// forwarder 会在 Drive 路径之外批量发送引擎事件到服务端，因为 listener 是同步的，
// 且必须快速返回。
type forwarder struct {
	client *Client
	logf   func(string, ...any)
	mu     sync.Mutex
	buffer []json.RawMessage
	signal chan chan struct{}
	done   chan struct{}
	wg     sync.WaitGroup
}

func newForwarder(client *Client, logf func(string, ...any)) *forwarder {
	f := &forwarder{client: client, logf: logf, signal: make(chan chan struct{}, 1), done: make(chan struct{})}
	f.wg.Add(1)
	go f.loop()
	return f
}

func (f *forwarder) add(_ context.Context, event cflow.Event) {
	raw, err := json.Marshal(event)
	if err != nil {
		f.logf("[wfdebug] event encoding failed, timeline incomplete: %v", err)
		return
	}
	f.mu.Lock()
	if len(f.buffer) >= 4096 {
		f.mu.Unlock()
		f.logf("[wfdebug] event buffer full, timeline incomplete")
		return
	}
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
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = f.client.PushEvents(ctx, batch)
		cancel()
		if err == nil {
			return
		}
		f.logf("[wfdebug] event forwarding failed: %v", err)
		if errors.Is(err, cflow.ErrNotFound) {
			return
		}
	}
	f.mu.Lock()
	f.buffer = append(batch, f.buffer...)
	overflow := len(f.buffer) > 4096
	if overflow {
		f.buffer = f.buffer[:4096]
	}
	f.mu.Unlock()
	if overflow {
		f.logf("[wfdebug] event buffer full, timeline incomplete")
	}
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
// flush 会发送缓冲事件并等待完成，确保一次 Drive 的事件先于其 report。
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
	f.mu.Lock()
	pending := len(f.buffer)
	f.mu.Unlock()
	if pending > 0 {
		f.logf("[wfdebug] %d events could not be forwarded, timeline incomplete", pending)
	}
}
