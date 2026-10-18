package main

import "time"

// Everything that crosses a durable boundary (Action input/output, WaitFor
// request/response, workflow input/output) is JSON. Keep these plain structs.
// 所有跨持久化边界的数据（Action 输入输出、WaitFor 请求响应、工作流输入输出）都走 JSON，
// 因此这里只放朴素结构体。

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID   string            `json:"id"`
	Name string            `json:"name"`
	Args map[string]string `json:"args"`
}

// StepKey makes f.Go("tool_call", tools, call) unique per call. The ID comes
// from the persisted LLM result, so it is identical on every replay even when
// a real model generates it randomly.
// StepKey 让 f.Go("tool_call", tools, call) 每次调用唯一。ID 来自已持久化的 LLM 结果，
// 即使真实模型随机生成，每次回放也完全一致。
func (c ToolCall) StepKey() string { return c.ID }

type ToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Sensitive   bool   `json:"sensitive,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// LLMRequest carries the whole conversation; Now comes from a durable
// checkpoint so the "model" sees the same clock on every replay.
// LLMRequest 携带完整对话；Now 来自持久化检查点，回放时模型看到的时间不变。
type LLMRequest struct {
	Agent    string     `json:"agent"`
	Model    string     `json:"model"`
	Messages []Message  `json:"messages"`
	Tools    []ToolSpec `json:"tools"`
	Now      time.Time  `json:"now"`
}

type LLMResponse struct {
	Thought   string     `json:"thought,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Usage     Usage      `json:"usage"`
}

// ApprovalRequest is the durable WaitFor request the host shows to the user.
// ApprovalRequest 是宿主展示给用户的持久化 WaitFor 请求。
type ApprovalRequest struct {
	Tool           string            `json:"tool"`
	Args           map[string]string `json:"args"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

type Approval struct {
	Approved bool   `json:"approved"`
	Note     string `json:"note,omitempty"`
}

// TurnInput is one run's input: the conversation so far, ending with the new
// user message. The host owns the conversation; the workflow owns one turn.
// TurnInput 是单个 run 的输入：截至目前的对话，最后一条是新的用户消息。
// 对话归宿主所有，工作流只负责一轮。
type TurnInput struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

// TurnOutput holds only the messages produced in this turn; the host appends them.
// TurnOutput 只包含本轮新产生的消息，由宿主追加到对话历史。
type TurnOutput struct {
	Messages  []Message `json:"messages"`
	LLMCalls  int       `json:"llm_calls"`
	ToolCalls int       `json:"tool_calls"`
	SubAgents int       `json:"sub_agents"`
}

// ResearchTask is the input of a sub-agent Flow; StepKey keys the mount point
// as sub_agent[<call id>], so parallel sub-agents never collide.
// ResearchTask 是子 Agent Flow 的输入；StepKey 把挂载点命名为 sub_agent[<call id>]，
// 并行的子 Agent 因此不会冲突。
type ResearchTask struct {
	ID    string `json:"id"`
	Topic string `json:"topic"`
}

func (t ResearchTask) StepKey() string { return t.ID }

type ResearchReport struct {
	Topic     string `json:"topic"`
	Summary   string `json:"summary"`
	LLMCalls  int    `json:"llm_calls"`
	ToolCalls int    `json:"tool_calls"`
}
