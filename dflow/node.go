// Package dflow executes declarative directed acyclic workflows.
//
// Package dflow 执行声明式有向无环工作流。
package dflow

import (
	"context"
)

// Node receives resolved input. Implementations must honor ctx and treat input
// values as read-only. Configuration is immutable; nodes may run concurrently.
//
// Node 接收已经解析完成的输入。实现必须遵守 ctx，并将输入值视为只读。
// 配置是不可变的，节点可能会并发运行。
type Node interface {
	Run(context.Context, map[string]any) (Result, error)
}

// NodeValidator optionally validates configuration without executing the node.
//
// NodeValidator 可选地在不执行节点的情况下校验配置。
type NodeValidator interface {
	Validate() error
}

// ResumableNode handles a persisted suspension's response. A recovered suspended
// node calls Resume, never Run. Resume may finish or return another suspension.
// Both methods may be retried after interruption; side effects need idempotency.
//
// ResumableNode 处理已持久化挂起的响应。恢复出的挂起节点会调用 Resume，
// 而不会调用 Run。Resume 可以完成节点，也可以再次返回挂起。两个方法都可能
// 在中断后重试，因此副作用需要具备幂等性。
type ResumableNode interface {
	Node
	Resume(context.Context, map[string]any, ResumeInput) (Result, error)
}

type ResumeInput struct {
	Token      string
	Suspension Suspension
	Response   any
}

type NodeFunc func(context.Context, map[string]any) (Result, error)

func (f NodeFunc) Run(ctx context.Context, input map[string]any) (Result, error) {
	return f(ctx, input)
}

// Result output values must be supported by the application's Infrastructure.
// Ports selects outgoing source ports: nil activates every edge; an empty
// non-nil slice activates none.
//
// Result 输出值必须被应用的 Infrastructure 支持。Ports 选择出边源端口：
// nil 会激活所有边；非 nil 的空切片不会激活任何边。
type Result struct {
	Output map[string]any `json:"output"`
	Ports  []string       `json:"ports"`
	// Suspend is mutually exclusive with Output and Ports.
	//
	// Suspend 与 Output 和 Ports 互斥。
	Suspend Suspension `json:"-"`
}

// NodeID returns the current node's stable ID, useful for business idempotency.
// The business must combine it with its own run/tenant scope.
//
// NodeID 返回当前节点的稳定 ID，可用于业务幂等键。业务必须将它与自己的
// run/tenant 作用域组合使用。
func NodeID(ctx context.Context) string {
	id, _ := ctx.Value(nodeIDKey{}).(string)
	return id
}

type nodeIDKey struct{}
