package dflow

// Suspension carries one pending operation's dispatch and resume context. Type
// supports application routing; storage owns encoding and reconstruction.
//
// Suspension 承载一次待处理操作的派发和恢复上下文。Type 用于应用路由；存储
// 负责编码和重建。
type Suspension interface {
	Type() string
}

// ValueSuspension is a generic payload for application-defined operations.
//
// ValueSuspension 是应用自定义操作的通用载荷。
type ValueSuspension struct {
	Kind string
	Data any
}

func (s *ValueSuspension) Type() string { return s.Kind }
