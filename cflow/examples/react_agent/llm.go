package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// MockLLM imitates a function-calling chat model. It is STATELESS: like a
// real API, every call receives the whole conversation and decides the next
// step from it. Text is streamed to the console while it is "generated";
// because it is an Action, a replayed call is never streamed again.
// MockLLM 模拟支持函数调用的对话模型。它是无状态的：和真实 API 一样，每次调用都拿到
// 完整对话并据此决定下一步。生成时流式输出到终端；由于它是 Action，回放时不会再次输出。
type MockLLM struct {
	Console *Console
	Flaky   float64 // probability that one attempt fails mid-stream
}

var (
	cities       = []string{"北京", "上海", "广州", "深圳", "杭州", "成都", "武汉", "西安", "南京", "重庆", "东京", "纽约", "伦敦"}
	emailPattern = regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.]+`)
	exprPattern  = regexp.MustCompile(`\(*\s*[0-9.]+(?:\s*[-+*/x×÷^%]\s*\(*\s*[0-9.]+\s*\)*)+`)
	pathPattern  = regexp.MustCompile(`(?:/[\w.\-]+)+|[\w\-]+\.\w{1,5}`)
)

func (m *MockLLM) Run(ctx context.Context, req LLMRequest) (LLMResponse, error) {
	// Time to first token.
	// 首 token 延迟。
	if err := sleepCtx(ctx, 300*time.Millisecond+time.Duration(rand.IntN(400))*time.Millisecond); err != nil {
		return LLMResponse{}, err
	}
	resp := m.think(req)
	indent := "  "
	speaker := "🤖 "
	if req.Agent != "main" {
		// Sub-agents stream on their own strand; tag lines so they can be told apart.
		// 子 Agent 在独立 strand 上流式输出；给行加标签以便区分。
		indent = "    [" + req.Agent + "] "
		speaker = indent + "📝 "
	}
	if resp.Thought != "" {
		if err := m.Console.Stream(ctx, dim, indent+"💭 ", resp.Thought); err != nil {
			return LLMResponse{}, err
		}
	}
	if m.Flaky > 0 && rand.Float64() < m.Flaky {
		// A plain error is retryable: RetryPolicy re-runs the Action.
		// 普通 error 属于可重试失败：RetryPolicy 会重新执行 Action。
		m.Console.Println(red, "  ⚠ [llm] 模拟网络抖动：stream reset by peer（将由 RetryPolicy 重试）")
		return LLMResponse{}, errors.New("stream reset by peer")
	}
	for _, call := range resp.ToolCalls {
		m.Console.Println(yellow, "%s🛠  Action: %s(%s)", indent, call.Name, formatArgs(call.Args))
	}
	if resp.Content != "" {
		if err := m.Console.Stream(ctx, cyan, speaker, resp.Content); err != nil {
			return LLMResponse{}, err
		}
	}
	prompt := 0
	for _, msg := range req.Messages {
		prompt += utf8.RuneCountInString(msg.Content)
	}
	resp.Usage = Usage{PromptTokens: prompt / 2, CompletionTokens: utf8.RuneCountInString(resp.Thought+resp.Content) / 2}
	return resp, nil
}

// think decides the next ReAct step from the messages after the last user turn.
// think 根据最后一条用户消息之后的消息决定下一步 ReAct 动作。
func (m *MockLLM) think(req LLMRequest) LLMResponse {
	last := 0
	for i, msg := range req.Messages {
		if msg.Role == RoleUser {
			last = i
		}
	}
	user := req.Messages[last].Content
	var observations []Message
	called := map[string]bool{}
	for _, msg := range req.Messages[last+1:] {
		if msg.Role == RoleTool {
			observations = append(observations, msg)
			called[msg.Name] = true
		}
	}

	gather, act := plan(user, req.Now)
	// Step 1: gather information with read-only tools (possibly in parallel).
	// 第 1 步：用只读工具收集信息（可能并行）。
	if len(gather) > 0 && !called[gather[0].Name] {
		return LLMResponse{
			Thought:   fmt.Sprintf("用户的问题需要外部信息，我先调用 %s 收集数据。", toolNames(gather)),
			ToolCalls: withIDs(gather, len(req.Messages)),
		}
	}
	// Step 2: side-effecting tools, built from what step 1 observed.
	// 第 2 步：有副作用的工具，参数基于第 1 步的观察结果。
	if len(act) > 0 && !called[act[0].Name] {
		for i := range act {
			if act[i].Name == "send_email" {
				act[i].Args["body"] = summarize(observations, "")
			}
		}
		thought := "信息已经收集完毕，接下来执行用户要求的操作。"
		if len(gather) == 0 {
			thought = "这是一个有副作用的操作，需要调用工具执行。"
		}
		return LLMResponse{Thought: thought, ToolCalls: withIDs(act, len(req.Messages))}
	}
	if len(observations) > 0 {
		return LLMResponse{Thought: "工具都已返回结果，整理成最终回答。", Content: summarize(observations, user)}
	}
	return LLMResponse{Content: chat(user, req)}
}

func plan(user string, now time.Time) (gather, act []ToolCall) {
	lower := strings.ToLower(user)
	if topics, ok := strings.CutPrefix(user, "调研"); ok {
		// One sub-agent per topic, run in parallel by the workflow.
		// 每个主题一个子 Agent，由工作流并行执行。
		for _, topic := range strings.FieldsFunc(topics, func(r rune) bool { return strings.ContainsRune("和、,， ", r) }) {
			gather = append(gather, ToolCall{Name: "research", Args: map[string]string{"topic": topic}})
		}
		return gather, nil
	}
	if strings.Contains(user, "天气") || strings.Contains(lower, "weather") || strings.Contains(user, "下雨") {
		found := false
		for _, city := range cities {
			if strings.Contains(user, city) {
				gather = append(gather, ToolCall{Name: "get_weather", Args: map[string]string{"city": city, "date": now.Format("2006-01-02")}})
				found = true
			}
		}
		if !found {
			gather = append(gather, ToolCall{Name: "get_weather", Args: map[string]string{"city": "北京", "date": now.Format("2006-01-02")}})
		}
	}
	if expr := exprPattern.FindString(user); expr != "" {
		gather = append(gather, ToolCall{Name: "calculate", Args: map[string]string{"expression": strings.TrimSpace(expr)}})
	}
	if strings.Contains(user, "几点") || strings.Contains(user, "时间") || strings.Contains(lower, "time") {
		gather = append(gather, ToolCall{Name: "current_time", Args: map[string]string{"timezone": "Asia/Shanghai"}})
	}
	if q, ok := strings.CutPrefix(user, "搜索"); ok {
		gather = append(gather, ToolCall{Name: "web_search", Args: map[string]string{"query": strings.TrimSpace(q)}})
	}
	if strings.Contains(user, "邮件") || strings.Contains(lower, "email") {
		to := emailPattern.FindString(user)
		if to == "" {
			to = "boss@example.com"
		}
		act = append(act, ToolCall{Name: "send_email", Args: map[string]string{"to": to, "subject": "来自助手的提醒"}})
	}
	if strings.Contains(user, "删除") || strings.Contains(lower, "delete") || strings.Contains(lower, "rm ") {
		path := pathPattern.FindString(user)
		if path == "" {
			path = "/tmp/unknown"
		}
		act = append(act, ToolCall{Name: "delete_file", Args: map[string]string{"path": path}})
	}
	return gather, act
}

func chat(user string, req LLMRequest) string {
	turns := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleUser {
			turns++
		}
	}
	switch {
	case strings.Contains(user, "你好") || strings.EqualFold(user, "hi") || strings.EqualFold(user, "hello"):
		return "你好！我是一个跑在 durable workflow 上的 ReAct 助手。可以试试：「北京和上海天气怎么样」「帮我算 (12+8)*3」「查一下杭州天气然后发邮件给 boss@example.com」「删除 /tmp/demo.log」「调研 Temporal 和 LangGraph」（并行子 Agent）。输入 /bye 结束会话。"
	case strings.Contains(user, "刚才") || strings.Contains(user, "之前") || strings.Contains(user, "历史"):
		var asked []string
		for _, msg := range req.Messages {
			if msg.Role == RoleUser && msg.Content != user {
				asked = append(asked, "「"+msg.Content+"」")
			}
		}
		if len(asked) == 0 {
			return "这是我们的第一轮对话，之前还没有聊过别的。"
		}
		return fmt.Sprintf("我们已经聊了 %d 轮，你之前说过：%s。完整对话历史保存在工作流的持久化记录里。", turns, strings.Join(asked, "、"))
	case strings.Contains(user, "工具") || strings.Contains(user, "能做什么"):
		var lines []string
		for _, spec := range req.Tools {
			mark := ""
			if spec.Sensitive {
				mark = "（敏感，需要你确认）"
			}
			lines = append(lines, fmt.Sprintf("%s：%s%s", spec.Name, spec.Description, mark))
		}
		return "我可以调用这些工具：" + strings.Join(lines, "；") + "。"
	default:
		return fmt.Sprintf("收到：「%s」。这是第 %d 轮对话。我是一个模拟模型，没有识别到需要调用工具的意图，所以直接回答。你可以问我天气、时间、算式，或让我发邮件/删除文件（会先征求你的同意）。", user, turns)
	}
}

func summarize(observations []Message, user string) string {
	if len(observations) == 0 {
		return "（无工具结果）"
	}
	var parts []string
	for _, o := range observations {
		parts = append(parts, o.Content)
	}
	text := strings.Join(parts, "；")
	if user == "" {
		return text
	}
	return "根据工具返回的结果：" + text + "。"
}

func withIDs(calls []ToolCall, seq int) []ToolCall {
	for i := range calls {
		calls[i].ID = fmt.Sprintf("call_%d_%d", seq, i)
	}
	return calls
}

func toolNames(calls []ToolCall) string {
	var names []string
	for _, c := range calls {
		names = append(names, c.Name)
	}
	return strings.Join(names, "、")
}

func formatArgs(args map[string]string) string {
	var parts []string
	for _, k := range []string{"city", "date", "expression", "timezone", "query", "to", "subject", "body", "path"} {
		if v, ok := args[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%q", k, v))
		}
	}
	return strings.Join(parts, ", ")
}
