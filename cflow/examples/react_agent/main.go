// A ReAct chat agent built on github.com/k0923/go/cflow.
//
//	go run ./cflow/examples/react_agent                  # in-memory store per turn
//	go run ./cflow/examples/react_agent -flaky 0.3       # inject retryable LLM/tool failures
//	go run ./cflow/cmd/wfdebug &           # optional: debugger UI on :7788
//	go run ./cflow/examples/react_agent -debugger        # every turn shows up as a run in the UI
//
// Two layers:
//   - The host (this file) owns the SESSION: an endless loop that reads a user
//     message, keeps the conversation history, and exits on /bye or Ctrl+C.
//   - Each user message starts ONE workflow run (TurnFlow) on a fresh
//     Infrastructure instance. The run is a bounded ReAct loop; when it
//     suspends on a tool approval the host asks the terminal and signals back.
//
// 两层结构：
//   - 宿主（本文件）负责会话：无限循环读取用户消息、维护对话历史，/bye 或 Ctrl+C 退出。
//   - 每条用户消息启动一个工作流 run（TurnFlow），使用全新的 Infrastructure 实例。
//     run 是有界的 ReAct 循环；挂起等待工具审批时，宿主在终端询问用户并 Signal 回去。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/debugger"
	"github.com/k0923/go/cflow/memory"
)

var (
	useDebugger     = flag.Bool("debugger", false, "store each turn's records in the wfdebug server")
	server          = flag.String("server", debugger.DefaultServer, "wfdebug server address")
	session         = flag.String("session", "", "session ID; run IDs are <session>-turn-<n>")
	flaky           = flag.Float64("flaky", 0, "probability of a retryable failure per LLM/tool attempt")
	approvalTimeout = flag.Duration("approval-timeout", 30*time.Second, "durable timeout for sensitive tool approval")
	speed           = flag.Duration("speed", 18*time.Millisecond, "streaming delay per character")
	trace           = flag.Bool("trace", false, "print engine events")
)

const model = "mock-react-1"

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, red+"error: "+err.Error()+reset)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	console := &Console{w: os.Stdout, delay: *speed}
	if *session == "" {
		*session = fmt.Sprintf("react-%d", time.Now().Unix())
	}
	llm, tools := &MockLLM{Console: console, Flaky: *flaky}, &Tools{Console: console, Flaky: *flaky}
	definition := cflow.NewWorkflow("react_turn", "1", TurnFlow{
		Agent: Agent{Name: "main", LLM: llm, Tools: tools, Specs: toolSpecs(), MaxSteps: 5},
		// The sub-agent is just another Flow, mounted from inside TurnFlow.
		// 子 Agent 只是另一个 Flow，在 TurnFlow 内部挂载。
		Research:        ResearchFlow{Agent: Agent{Name: "research", LLM: llm, Tools: tools, Specs: researchSpecs(), MaxSteps: 3}},
		ApprovalTimeout: *approvalTimeout,
	})

	console.Println(bold, "ReAct workflow demo — 输入消息开始对话；/history 查看对话，/records 查看上一轮的持久化记录，/bye 或 Ctrl+C 退出")
	history := []Message{{Role: RoleSystem, Content: "You are a helpful assistant. Use tools when needed."}}
	lines := readLines(ctx)
	var last cflow.Infrastructure
	for turn := 1; ; {
		console.Print(bold+blue, "\n👤 [%d] 你：", turn)
		line, ok := next(ctx, lines, time.Time{})
		if !ok {
			console.Println(yellow, "\n会话结束，共 %d 轮", turn-1)
			return nil
		}
		switch text := strings.TrimSpace(line); text {
		case "":
		case "/bye":
			console.Println(green, "再见，共 %d 轮", turn-1)
			return nil
		case "/history":
			printHistory(console, history)
		case "/records":
			printRecords(ctx, last, console)
		default:
			input := TurnInput{Model: model, Messages: append(history, Message{Role: RoleUser, Content: text})}
			store, err := openStore(ctx, fmt.Sprintf("%s-turn-%d", *session, turn))
			if err != nil {
				return err
			}
			last = store
			output, err := driveTurn(ctx, store, definition, input, console, lines)
			switch {
			case ctx.Err() != nil:
				console.Println(yellow, "\n进程退出")
				return nil
			case err != nil:
				// The turn failed terminally; the session survives without it.
				// 本轮终态失败；丢弃本轮，会话继续。
				console.Println(red, "  本轮失败，已丢弃：%v", err)
				continue
			}
			history = append(input.Messages, output.Messages...)
			console.Println(dim, "  ── turn %d 完成：%d 次 LLM 调用，%d 次工具调用，%d 个子 Agent，新增 %d 条消息",
				turn, output.LLMCalls, output.ToolCalls, output.SubAgents, len(output.Messages))
			turn++
		}
	}
}

// driveTurn drives one run to a terminal state. Between drives it answers
// the approval the run is suspended on (or waits for the durable timer).
// driveTurn 把一个 run 驱动到终态。两次 Drive 之间，宿主回答 run 正在等待的审批（或等待持久化计时器）。
func driveTurn(ctx context.Context, store cflow.Infrastructure, definition cflow.Workflow[TurnInput, TurnOutput],
	input TurnInput, console *Console, lines <-chan string) (TurnOutput, error) {
	engine, err := cflow.NewEngine(store, cflow.Options{})
	if err != nil {
		return TurnOutput{}, err
	}
	stats := observe(engine, console)
	for drive := 1; ; drive++ {
		stats.reset()
		started := time.Now()
		result, err := engine.Drive(ctx, definition, input)
		report(ctx, store, result, err)
		console.Println(dim, "  ── drive #%d %s：执行 %d 个 action，回放 %d 个，耗时 %s",
			drive, result.Status, stats.executed.Load(), stats.replayed.Load(), time.Since(started).Round(time.Millisecond))
		switch {
		case ctx.Err() != nil:
			return result.Output, ctx.Err()
		case result.Status == cflow.Completed:
			return result.Output, nil
		case result.Status == cflow.Failed:
			return result.Output, err
		case err != nil:
			// RUNNING: a retryable failure aborted this drive. Back off and re-drive;
			// committed steps are replayed, not re-executed.
			// RUNNING：可重试失败中止了本次 drive。退避后重新 Drive；已提交的步骤会回放而非重新执行。
			console.Println(red, "  drive 中止（可重试）：%v，1s 后重新 Drive", err)
			_ = sleepCtx(ctx, time.Second)
			continue
		}
		if err := approve(ctx, engine, store, console, lines, result.Pending); err != nil {
			return result.Output, err
		}
	}
}

// approve handles one suspension: it reads the durable approval request behind
// the pending signal and resolves it from the terminal before the timer fires.
// approve 处理一次挂起：读取 pending 信号背后的持久化审批请求，在计时器到期前用终端输入回答。
func approve(ctx context.Context, engine *cflow.Engine, store cflow.Infrastructure, console *Console, lines <-chan string, pending []cflow.Pending) error {
	var deadline time.Time
	for _, p := range pending {
		if p.Kind == cflow.SleepKind && (deadline.IsZero() || p.Until.Before(deadline)) {
			deadline = p.Until
		}
	}
	for _, p := range pending {
		if p.Kind != cflow.SignalKind {
			continue
		}
		record, err := store.Get(ctx, p.Key)
		if err != nil {
			return err
		}
		var req ApprovalRequest
		if err = json.Unmarshal(record.Input, &req); err != nil {
			return err
		}
		console.Println(bold+yellow, "  🔐 敏感操作 %s(%s) 需要确认 [y/N]（%ds 内未确认自动取消）：",
			req.Tool, formatArgs(req.Args), int(time.Until(deadline).Round(time.Second).Seconds()))
		console.Print(bold+yellow, "  > ")
		line, ok := next(ctx, lines, deadline)
		if !ok {
			// Nothing to signal: the durable timer is due and the next Drive
			// lets it win the WaitAny.
			// 无需 Signal：持久化计时器已到期，下一次 Drive 会让它赢得 WaitAny。
			console.Println(yellow, "\n  ⏰ 超时")
			return ctx.Err()
		}
		line = strings.ToLower(strings.TrimSpace(line))
		decision := Approval{Approved: line == "y" || line == "yes" || line == "是"}
		if !decision.Approved && line != "" && line != "n" && line != "no" {
			decision.Note = line
		}
		// A late answer after the timer already won returns false; that is fine.
		// 计时器已经获胜后的迟到回答会返回 false，这是安全的。
		_, err = engine.Signal(ctx, p.Key, decision)
		return err
	}
	// Only timers are pending (or someone answered elsewhere, e.g. the UI).
	// 只剩计时器（或者已在别处回答，例如 debugger UI）。
	if !deadline.IsZero() {
		return sleepCtx(ctx, time.Until(deadline))
	}
	return sleepCtx(ctx, 500*time.Millisecond)
}

func readLines(ctx context.Context) <-chan string {
	lines := make(chan string)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	return lines
}

// next returns the next line, or false on EOF, cancellation or deadline.
// next 返回下一行；EOF、取消或到达截止时间时返回 false。
func next(ctx context.Context, lines <-chan string, deadline time.Time) (string, bool) {
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case line, ok := <-lines:
		return line, ok
	case <-timeout:
		return "", false
	case <-ctx.Done():
		return "", false
	}
}

type driveStats struct{ executed, replayed atomic.Int64 }

func (s *driveStats) reset() { s.executed.Store(0); s.replayed.Store(0) }

func observe(engine *cflow.Engine, console *Console) *driveStats {
	stats := &driveStats{}
	engine.Subscribe(func(_ context.Context, e cflow.Event) {
		switch e.Type {
		case cflow.ActionStarted:
			stats.executed.Add(1)
		case cflow.ActionReplayed:
			stats.replayed.Add(1)
		}
		if *trace && e.Type != cflow.ActionReplayed {
			msg := ""
			if e.Error != nil {
				msg = " " + e.Error.Category + ": " + e.Error.Message
			}
			console.Println(dim, "    · %s %s %s%s", e.Type, e.Key, e.Status, msg)
		}
	})
	return stats
}

func printHistory(console *Console, history []Message) {
	for _, m := range history {
		text := m.Content
		for _, call := range m.ToolCalls {
			text += fmt.Sprintf(" →%s(%s)", call.Name, formatArgs(call.Args))
		}
		console.Println(dim, "  %-9s %s", m.Role, text)
	}
}

func printRecords(ctx context.Context, store cflow.Infrastructure, console *Console) {
	lister, ok := store.(cflow.RecordLister)
	if !ok {
		console.Println(dim, "  还没有完成的轮次，或当前存储不支持列出记录（请在 debugger UI 中查看）")
		return
	}
	records, err := lister.Records(ctx)
	if err != nil {
		console.Println(red, "  %v", err)
		return
	}
	sort.Slice(records, func(i, j int) bool { return records[i].StartedAt.Before(records[j].StartedAt) })
	for _, r := range records {
		// Render the structured Path segment by segment instead of splitting the key.
		// 逐段渲染结构化 Path，而不是拆分 key。
		var parts []string
		for _, seg := range r.Path {
			if seg.ID != "" {
				parts = append(parts, seg.Name+"["+seg.ID+"]")
			} else {
				parts = append(parts, seg.Name)
			}
		}
		console.Println(dim, "  %-58s %-10s %-9s %s", strings.Join(parts, " › "), r.Kind, r.Status, r.Type)
	}
	console.Println(dim, "  共 %d 条记录", len(records))
}

// ---- store selection ----

// openStore returns a fresh Infrastructure bound to ONE run. This is what
// keeps fixed keys like llm_0 from colliding across turns.
// openStore 返回一个只绑定单个 run 的全新 Infrastructure，这正是 llm_0 这类固定 key
// 在不同轮次之间不会冲突的原因。
func openStore(ctx context.Context, runID string) (cflow.Infrastructure, error) {
	if !*useDebugger {
		return memory.New(), nil
	}
	client, err := debugger.NewClient(*server, runID)
	if err != nil {
		return nil, err
	}
	// Register the run so the UI shows its workflow name; records are created on demand anyway.
	// 注册 run，让 UI 显示工作流名称；记录本身会按需创建。
	spec, _ := json.Marshal(debugger.RunSpec{ID: runID, Workflow: "react_turn", Version: "1"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(*server, "/")+"/api/runs", bytes.NewReader(spec))
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect debugger %s (start it with `go run ./cflow/cmd/wfdebug`): %w", *server, err)
	}
	resp.Body.Close()
	fmt.Printf("%s  run %s → %s%s\n", dim, runID, client.URL(), reset)
	return client, nil
}

func report(ctx context.Context, store cflow.Infrastructure, result cflow.Result[TurnOutput], err error) {
	client, ok := store.(*debugger.Client)
	if !ok {
		return
	}
	r := debugger.Report{Status: string(result.Status), Pending: result.Pending}
	if err != nil {
		r.Error = err.Error()
	}
	if result.Status == cflow.Completed {
		r.Output, _ = json.Marshal(result.Output)
	}
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = client.Report(reportCtx, r)
}
