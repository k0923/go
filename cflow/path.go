package cflow

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Segment is one level of a step address. Name is the fixed text written in
// code ("tool_call"); ID is the optional identity that tells repeated steps
// of the same name apart. It comes from data, never from a counter: the
// input's StepKey(), or an explicit Scope(name, id).
// Segment 是步骤地址的一层。Name 是代码里写死的固定文本（"tool_call"）；ID 是可选身份，
// 用来区分同名的重复步骤。它来自数据而不是计数器：输入的 StepKey()，或显式的 Scope(name, id)。
type Segment struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

// Path is the structured address of a step, e.g.
// [{step 0} {tool_call call_7}]. Infrastructure receives it with every
// record so it can persist the structure however it likes (columns, arrays,
// a tree); the engine itself addresses steps by BuildKey(path).
// Path 是步骤的结构化地址，例如 [{step 0} {tool_call call_7}]。Infrastructure 会随每条
// 记录收到它，可以按自己的方式持久化（拆列、数组、树）；引擎本身用 BuildKey(path) 寻址。
type Path []Segment

func (p Path) String() string { return DefaultKey(p) }

func (p Path) Equal(other Path) bool { return slices.Equal(p, other) }

func (p Path) child(name, id string) Path {
	return append(slices.Clip(p), Segment{Name: name, ID: id})
}

// StepKeyer gives an input its own identity. Run, Go, RunFlow, GoFlow, Loop
// and WaitFor/OnSignal requests that implement it append StepKey() as the
// segment ID, so a fixed name such as "tool_call" stays unique per call:
//
//	func (c ToolCall) StepKey() string { return c.ID }
//	f.Go("tool_call", tools, call) // tool_call[call_7]
//
// StepKey must be derived from the input's data only (deterministic, no time
// or randomness). An empty result means "no ID". A panic is a definition bug.
// StepKeyer 为输入提供自身身份。Run、Go、RunFlow、GoFlow、Loop 以及 WaitFor/OnSignal 的请求
// 实现它时，StepKey() 会作为该段的 ID，使 "tool_call" 这类固定名字在每次调用间保持唯一。
// StepKey 只能由输入数据推导（确定、不含时间或随机数）。返回空串表示“没有 ID”。panic 属于定义缺陷。
type StepKeyer interface {
	StepKey() string
}

// KeyBuilder is OPTIONAL. An Infrastructure implementing it decides how a Path
// becomes the string key used for Get/Commit/Resolve/leases/Signal. It MUST be
// pure, stable across releases, and injective (different paths, different
// keys). Without it the engine uses DefaultKey. Inside one Drive the engine
// rejects a non-injective builder with ErrInvalid; across drives a stored
// Record.Path that differs from the expected path is rejected with ErrChanged.
// KeyBuilder 是可选接口。实现它的 Infrastructure 决定 Path 如何变成 Get/Commit/Resolve/租约/
// Signal 使用的字符串 key。它必须是纯函数、跨版本稳定且单射（不同路径对应不同 key）。
// 未实现时引擎使用 DefaultKey。同一次 Drive 内，引擎以 ErrInvalid 拒绝非单射的实现；
// 跨 Drive 时，存储的 Record.Path 与期望路径不一致会以 ErrChanged 拒绝。
type KeyBuilder interface {
	BuildKey(Path) string
}

// MaxIDLength bounds a segment ID in bytes.
// MaxIDLength 限制段 ID 的字节长度。
const MaxIDLength = 256

// DefaultKey encodes segments as name or name[id], joined by "|". Names match
// ^[A-Za-z0-9_]+$; '\', '[', ']' and '|' inside an ID are backslash-escaped,
// so the encoding is injective. Paths without IDs encode exactly as the
// historical "a|b|c" keys.
// DefaultKey 把每段编码为 name 或 name[id]，用 "|" 连接。Name 匹配 ^[A-Za-z0-9_]+$；
// ID 中的 '\'、'['、']'、'|' 用反斜杠转义，因此编码是单射的。不含 ID 的路径编码结果与
// 原先的 "a|b|c" 形式完全相同。
func DefaultKey(p Path) string {
	var b strings.Builder
	for i, s := range p {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(s.Name)
		if s.ID == "" {
			continue
		}
		b.WriteByte('[')
		for _, r := range s.ID {
			if r == '\\' || r == '[' || r == ']' || r == '|' {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte(']')
	}
	return b.String()
}

func validID(id string) error {
	if len(id) > MaxIDLength || !utf8.ValidString(id) {
		return fmt.Errorf("%w: step id %.32q (max %d bytes of UTF-8)", ErrInvalid, id, MaxIDLength)
	}
	return nil
}

// stepID returns the input's StepKey, isolating a panicking implementation.
// stepID 返回输入的 StepKey，并隔离会 panic 的实现。
func stepID(input any) (id string, err error) {
	keyer, ok := input.(StepKeyer)
	if !ok {
		return "", nil
	}
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("%w: StepKey panicked: %v", ErrInvalid, value)
		}
	}()
	return keyer.StepKey(), nil
}
