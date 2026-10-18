package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/k0923/go/cflow"
)

// TurnFlow is ONE durable ReAct turn: reason → act → observe until the model
// answers. It is orchestration code, replayed from the start on every Drive,
// so it must stay deterministic and must not print or touch the outside world.
//
// Every step name below is fixed text; identity comes from data:
//
//	step[0]|llm                              Scope("step", i)
//	step[0]|tool_call[call_3_0]              ToolCall.StepKey()
//	step[0]|call[call_3_1]|approve|decision  Scope("call", id) + fixed WaitAny names
//	step[0]|sub_agent[call_3_2]|step[0]|llm  nested Flow, keyed by ResearchTask.StepKey()
//
// TurnFlow 是一轮持久化的 ReAct：推理 → 行动 → 观察，直到模型给出回答。它是编排代码，
// 每次 Drive 都从头回放，因此必须确定、不能打印、不能访问外部世界。
// 下面所有步骤名都是固定文本，身份来自数据（见上表）。
type TurnFlow struct {
	Agent           Agent
	Research        cflow.Flow[ResearchTask, ResearchReport]
	ApprovalTimeout time.Duration
}

// Agent is the reusable ReAct core shared by the main agent and sub-agents.
// Agent 是主 Agent 与子 Agent 共用的 ReAct 核心。
type Agent struct {
	Name     string
	LLM      cflow.Action[LLMRequest, LLMResponse]
	Tools    cflow.Action[ToolCall, ToolResult]
	Specs    []ToolSpec
	MaxSteps int
}

func (c TurnFlow) Run(f *cflow.FlowContext, in TurnInput) (TurnOutput, error) {
	return c.Agent.react(f, in.Model, in.Messages, c.act)
}

// actFunc executes one batch of tool calls and returns their observations.
// actFunc 执行一批工具调用并返回观察结果。
type actFunc func(s *cflow.FlowContext, calls []ToolCall, out *TurnOutput) []Message

func (a Agent) react(f *cflow.FlowContext, model string, history []Message, act actFunc) (TurnOutput, error) {
	var out TurnOutput
	now := f.Now("now")
	for step := 0; step < a.MaxSteps; step++ {
		s := f.Scope("step", strconv.Itoa(step))
		resp, err := s.Run("llm", a.LLM, LLMRequest{Agent: a.Name, Model: model, Messages: history, Tools: a.Specs, Now: now},
			cflow.ActionOptions{
				Timeout: 30 * time.Second,
				Retry:   cflow.RetryPolicy{MaxAttempts: 3, InitialInterval: 500 * time.Millisecond},
			})
		if err != nil {
			// Committed LLM failure: fail this turn; the host drops it and the
			// conversation goes on.
			// 已提交的 LLM 失败：本轮失败，由宿主丢弃本轮，对话继续。
			return out, err
		}
		out.LLMCalls++
		reply := Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls}
		history, out.Messages = append(history, reply), append(out.Messages, reply)
		if len(resp.ToolCalls) == 0 {
			return out, nil
		}
		observations := act(s, resp.ToolCalls, &out)
		history, out.Messages = append(history, observations...), append(out.Messages, observations...)
	}
	stop := Message{Role: RoleAssistant, Content: fmt.Sprintf("（已达到单轮最大推理步数 %d，停止本轮）", a.MaxSteps)}
	out.Messages = append(out.Messages, stop)
	return out, nil
}

// act runs one batch for the main agent: approvals first, then tools and
// sub-agents in parallel, each on its own strand.
// act 为主 Agent 执行一批调用：先审批，再并行执行工具和子 Agent，各自运行在独立 strand 上。
func (c TurnFlow) act(s *cflow.FlowContext, calls []ToolCall, out *TurnOutput) []Message {
	// 1. Sensitive tools need a human decision. The WaitAny has no input of its
	//    own, so a Scope keyed by the call ID gives its fixed names an identity.
	// 1. 敏感工具需要人工确认。WaitAny 自身没有输入，用以 call ID 为键的 Scope 赋予固定名字身份。
	denied := make(map[string]string)
	for _, call := range calls {
		if !sensitive(c.Agent.Specs, call.Name) {
			continue
		}
		decision := cflow.OnSignal[Approval]("decision", ApprovalRequest{
			Tool: call.Name, Args: call.Args, TimeoutSeconds: int(c.ApprovalTimeout.Seconds()),
		})
		timeout := cflow.OnSleep("timeout", c.ApprovalTimeout)
		switch s.Scope("call", call.ID).WaitAny("approve", decision, timeout) {
		case timeout:
			denied[call.ID] = fmt.Sprintf("用户未在 %s 内确认，操作已自动取消", c.ApprovalTimeout)
		case decision:
			if v := decision.Value(); !v.Approved {
				denied[call.ID] = "用户拒绝执行该操作" + suffix(v.Note)
			}
		}
	}

	// 2. Fan out: tools are Actions, research calls mount a sub-agent Flow.
	//    ToolCall / ResearchTask implement StepKey, so fixed names stay unique.
	// 2. 扇出：工具是 Action，research 调用挂载子 Agent Flow。ToolCall / ResearchTask 实现了
	//    StepKey，固定名字因此保持唯一。
	tools := make(map[string]*cflow.Future[ToolResult])
	agents := make(map[string]*cflow.Future[ResearchReport])
	for _, call := range calls {
		switch {
		case denied[call.ID] != "":
		case call.Name == "research":
			agents[call.ID] = s.GoFlow("sub_agent", c.Research, ResearchTask{ID: call.ID, Topic: call.Args["topic"]})
		default:
			tools[call.ID] = s.Go("tool_call", c.Agent.Tools, call, cflow.ActionOptions{
				Timeout: 10 * time.Second,
				Retry:   cflow.RetryPolicy{MaxAttempts: 3, InitialInterval: 200 * time.Millisecond},
			})
		}
	}

	var observations []Message
	for _, call := range calls {
		content := denied[call.ID]
		if future, ok := tools[call.ID]; ok {
			if r, err := future.Wait(); err != nil {
				content = "工具执行失败：" + err.Error()
			} else {
				content = r.Content
				out.ToolCalls++
			}
		}
		if future, ok := agents[call.ID]; ok {
			if r, err := future.Wait(); err != nil {
				content = "子 Agent 失败：" + err.Error()
			} else {
				content = r.Summary
				out.SubAgents++
				out.LLMCalls += r.LLMCalls
				out.ToolCalls += r.ToolCalls
			}
		}
		observations = append(observations, Message{Role: RoleTool, Name: call.Name, ToolCallID: call.ID, Content: content})
	}
	return observations
}

// ResearchFlow is a sub-agent: its own ReAct loop with read-only tools,
// mounted by TurnFlow through GoFlow. Several run in parallel, each under
// sub_agent[<call id>]. It could mount further sub-agents; nesting is bounded
// by Options.MaxFlowDepth.
// ResearchFlow 是子 Agent：拥有只读工具的独立 ReAct 循环，由 TurnFlow 通过 GoFlow 挂载。
// 多个子 Agent 可并行运行，各自位于 sub_agent[<call id>] 之下。它也可以继续挂载子 Agent，
// 嵌套深度受 Options.MaxFlowDepth 限制。
type ResearchFlow struct{ Agent Agent }

func (r ResearchFlow) Run(f *cflow.FlowContext, task ResearchTask) (ResearchReport, error) {
	history := []Message{
		{Role: RoleSystem, Content: "You are a research sub-agent. Search, then summarize."},
		{Role: RoleUser, Content: "搜索 " + task.Topic},
	}
	out, err := r.Agent.react(f, "mock-research-1", history, r.act)
	if err != nil {
		return ResearchReport{}, err
	}
	report := ResearchReport{Topic: task.Topic, LLMCalls: out.LLMCalls, ToolCalls: out.ToolCalls}
	if n := len(out.Messages); n > 0 {
		report.Summary = fmt.Sprintf("[%s 调研结论] %s", task.Topic, out.Messages[n-1].Content)
	}
	return report, nil
}

func (r ResearchFlow) act(s *cflow.FlowContext, calls []ToolCall, out *TurnOutput) []Message {
	futures := make([]*cflow.Future[ToolResult], len(calls))
	for i, call := range calls {
		futures[i] = s.Go("tool_call", r.Agent.Tools, call)
	}
	var observations []Message
	for i, call := range calls {
		content := ""
		if result, err := futures[i].Wait(); err != nil {
			content = "工具执行失败：" + err.Error()
		} else {
			content = result.Content
			out.ToolCalls++
		}
		observations = append(observations, Message{Role: RoleTool, Name: call.Name, ToolCallID: call.ID, Content: content})
	}
	return observations
}

func sensitive(specs []ToolSpec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return spec.Sensitive
		}
	}
	return false
}

func suffix(note string) string {
	if note == "" {
		return ""
	}
	return "（" + note + "）"
}
