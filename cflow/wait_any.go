package cflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"
)

// Case is one WaitAny candidate built with OnSignal, OnKind, OnSleep or
// OnSleepUntil. Every case carries its OWN response type. WaitAny returns the
// winning Case itself, so compare it with the cases you passed:
//
//	reply := cflow.OnSignal[Reply]("reply", request)
//	timeout := cflow.OnSleep("timeout", time.Hour)
//	switch f.WaitAny("reply_or_timeout", reply, timeout) {
//	case reply:
//		return reply.Value().Text, nil
//	case timeout:
//		return "timeout", nil
//	}
//
// A Case is single-use: build it inside the Flow for every WaitAny call.
// Case 是 WaitAny 的一个候选项，由 OnSignal、OnKind、OnSleep 或 OnSleepUntil 构造。
// 每个候选项都有自己的响应类型。WaitAny 返回获胜的 Case 本身，因此直接与传入的
// 候选项比较即可（见上例）。
//
// Case 只能使用一次：每次调用 WaitAny 时都要在 Flow 内部重新构造。
type Case interface {
	// Key is the built storage key once WaitAny ran, the local name before.
	// Key 在 WaitAny 执行后是构建出的存储 key，之前是局部名字。
	Key() string
	// Path is the structured address once WaitAny ran.
	// Path 是 WaitAny 执行后的结构化地址。
	Path() Path
	Kind() Kind
	// Failure is this candidate's isolated error; empty if it did not fail.
	// Failure 是该候选项被隔离的错误；未失败时为空。
	Failure() string
	member() *waitCase
}

type waitCase struct {
	key      string // local name before WaitAny, built key after
	id       string // request.StepKey() for signals
	idErr    error
	path     Path
	kind     Kind
	typ      string // leaf type segment, e.g. "signal[Request,Response]"
	duration time.Duration
	until    time.Time
	encode   func() (json.RawMessage, error) // nil for timers
	decode   func(json.RawMessage) error     // nil for timers
	failure  string
	used     atomic.Bool
}

func (c *waitCase) Key() string       { return c.key }
func (c *waitCase) Path() Path        { return slices.Clone(c.path) }
func (c *waitCase) Kind() Kind        { return c.kind }
func (c *waitCase) Failure() string   { return c.failure }
func (c *waitCase) member() *waitCase { return c }

// SignalCase is answered by an external JSON response decoded as Response.
// SignalCase 由外部 JSON 响应完成，响应会解码为 Response。
type SignalCase[Response any] struct {
	waitCase
	value Response
}

// Value is the decoded response: the zero value unless this case won.
// Value 是解码后的响应：只有该候选项获胜时才非零值。
func (c *SignalCase[Response]) Value() Response { return c.value }

// OnSignal describes a WaitFor candidate. Response is explicit, Request is
// inferred: OnSignal[Approval]("approval", order). A request implementing
// StepKeyer addresses the member as name[request.StepKey()].
// OnSignal 描述一个 WaitFor 候选项。Response 需显式指定，Request 自动推断：
// OnSignal[Approval]("approval", order)。请求实现 StepKeyer 时成员地址为 name[request.StepKey()]。
func OnSignal[Response, Request any](key string, request Request) *SignalCase[Response] {
	return OnKind[Response](SignalKind, key, request)
}

// OnKind describes a payload-bearing candidate of a custom kind. The
// Infrastructure must support its registration and resolution; the reference
// memory store rejects unknown kinds with ErrSignalUnsupported instead of
// silently treating them as WaitFor.
// OnKind 描述一个自定义 Kind 的携带负载候选项。Infrastructure 必须支持其注册与解析；
// 参考内存存储会以 ErrSignalUnsupported 拒绝未知 Kind，而不会静默当作 WaitFor。
func OnKind[Response, Request any](kind Kind, key string, request Request) *SignalCase[Response] {
	if kind == "" {
		kind = SignalKind
	}
	c := &SignalCase[Response]{}
	c.key, c.kind, c.typ = key, kind, signature[Request, Response]("signal")
	c.id, c.idErr = stepID(request)
	c.encode = func() (json.RawMessage, error) { return json.Marshal(request) }
	c.decode = func(raw json.RawMessage) error { return decode(raw, &c.value) }
	return c
}

// SleepCase fires when its deadline passes; it carries no response.
// SleepCase 在截止时间到达时触发；它不携带响应。
type SleepCase struct{ waitCase }

// OnSleep describes a timer whose relative deadline is fixed on registration.
// OnSleep 描述一个在注册时固定相对截止时间的计时器。
func OnSleep(key string, duration time.Duration) *SleepCase {
	c := &SleepCase{}
	c.key, c.kind, c.typ, c.duration = key, SleepKind, "sleep", duration
	return c
}

// OnSleepUntil describes a timer with an absolute deadline.
// OnSleepUntil 描述一个具有绝对截止时间的计时器。
func OnSleepUntil(key string, until time.Time) *SleepCase {
	c := &SleepCase{}
	c.key, c.kind, c.typ, c.until = key, SleepKind, "sleep", until
	return c
}

type WaitFailure struct {
	Key     string
	Message string
}

// WaitRequest is the transport representation consumed by storage adapters.
// Replay identity of a member is Key + Kind (+ Path): Request, Duration, Until,
// Type and encoding failure are inputs fixed by the first registration.
// WaitRequest 是存储适配器消费的传输层表示。成员的回放身份为 Key + Kind（+ Path）：
// Request、Duration、Until、Type 及编码是否失败都是以首次注册为准的输入。
type WaitRequest struct {
	Key  string
	Path Path `json:",omitempty"`
	// Type is the member's leaf signature, kept for observation only.
	// Type 是成员的叶子签名，仅用于观测。
	Type     string `json:",omitempty"`
	Request  json.RawMessage
	Kind     Kind
	Duration time.Duration
	Until    time.Time
	// Error isolates an unusable request while preserving its ordered identity.
	// Error 隔离不可用请求，同时保留其有序身份。
	Error string `json:",omitempty"`
}

// SameWaitMember reports whether a re-submitted member matches the stored
// one. Storage adapters use it for every member, in order.
// SameWaitMember 判断重新提交的成员是否与已存储成员匹配。存储适配器按顺序对每个成员使用它。
func SameWaitMember(stored, request WaitRequest) bool {
	return request.Key == stored.Key && request.Kind == stored.Kind &&
		(stored.Path == nil || stored.Path.Equal(request.Path))
}

// sameJSON treats an absent payload and JSON null as equal, because a nil
// RawMessage round-trips through JSON transports as the literal "null".
// sameJSON 将缺失负载与 JSON null 视为相等，因为 nil RawMessage 经 JSON 传输往返后
// 会变成字面量 "null"。
func sameJSON(a, b json.RawMessage) bool {
	norm := func(raw json.RawMessage) json.RawMessage {
		if string(raw) == "null" {
			return nil
		}
		return raw
	}
	return bytes.Equal(norm(a), norm(b))
}

type WaitAnyState struct {
	Status     Status
	WinnerKey  string
	WinnerKind Kind
	Response   json.RawMessage
	Pending    []Pending
	Failures   []WaitFailure
}

// WaitAnyInfrastructure is OPTIONAL. Existing Infrastructure implementations
// remain source-compatible; WaitAny requires an adapter implementing this API.
//
// WaitAny atomically registers ALL signals + request notifications, or reads an
// existing group's snapshot after checking every ordered member with
// SameWaitMember. Group ownership must be persisted. The existing Resolve operation MUST, in
// the same transaction, choose the first successful member, cancel every other
// pending member, remove obsolete notifications and enqueue CANCEL notifications
// for external providers. Subsequent callbacks cannot
// overwrite the winner or resurrect a cancelled member.
//
// FailWait atomically fails one member without cancelling healthy members.
// If no candidates remain it fails the group and enqueues a wake. Panic/IO
// failures of the adapter are group-level infrastructure errors, not candidate
// failures. Returned snapshots must be deep copies.
// WaitAnyInfrastructure 是可选接口。现有 Infrastructure 实现保持源码兼容；
// WaitAny 需要适配器实现此 API。
//
// WaitAny 会原子地注册所有信号和请求通知，或在用 SameWaitMember 按顺序检查每个成员后读取
// 现有等待组快照。组归属必须持久化。现有 Resolve 操作必须在同一个事务中选择
// 第一个成功成员、取消所有其他 pending 成员、移除过期通知，并为外部提供方
// 入队 CANCEL 通知。后续回调不能覆盖赢家，也不能复活已取消的成员。
//
// FailWait 会原子地将一个成员标记为失败，而不取消健康成员。如果没有候选项剩余，
// 它会将整个组标记为失败并入队一个 wake。适配器的 panic/IO 失败属于组级别
// 基础设施错误，而不是候选项失败。返回的快照必须是深拷贝。
type WaitAnyInfrastructure interface {
	WaitAny(context.Context, string, []WaitRequest) (*WaitAnyState, error)
	FailWait(context.Context, string, int64, string) (bool, error)
}

var ErrWaitAnyUnsupported = errors.New("infrastructure does not support WaitAny")
var ErrSignalUnsupported = errors.New("signal kind unsupported")

// WaitAny registers all candidates simultaneously. Until one completes it
// halts the calling orchestration; Engine.Drive reports ALL pending keys. The
// first accepted callback cancels losers immediately in storage. On the next
// Drive the workflow resumes and WaitAny returns the durable winner: one of
// the cases passed in, with its Value decoded (timers carry no value).
//
// Like WaitFor and Sleep it returns no error: candidates are external signals,
// not business steps. A candidate failure (FailWait, unencodable request) only
// drops that candidate (see Case.Failure); when NO candidate remains the drive
// fails terminally with ErrWaitFailed. To let the Flow degrade, have the
// provider answer with a typed failure value, or add a timer candidate.
// Invalid key topology and reused cases are rejected before registration.
// WaitAny 会同时注册所有候选项。在某个候选项完成前，它会停止调用方编排；
// Engine.Drive 会报告所有 pending key。第一个被接受的回调会立即在存储中取消失败方。
// 下一次 Drive 时工作流恢复，WaitAny 返回持久化的赢家：即传入的某个 Case，
// 其 Value 已解码（计时器不携带值）。
//
// 与 WaitFor、Sleep 一样不返回 error：候选项是外部信号，而不是业务步骤。候选项失败
// （FailWait、请求无法编码）只会剔除该候选项（见 Case.Failure）；没有候选项剩余时，
// drive 以 ErrWaitFailed 终态失败。如需让 Flow 降级，请由提供方回答一个类型化的失败值，
// 或加入计时器候选项。无效 key 拓扑和被重复使用的 Case 会在注册前被拒绝。
// The group name is fixed; give it an identity with a Scope when the same
// WaitAny runs in a loop: f.Scope("call", call.ID).WaitAny("approve", ...).
// 组名固定；同一个 WaitAny 在循环中运行时，用 Scope 赋予身份：
// f.Scope("call", call.ID).WaitAny("approve", ...)。
func (f *FlowContext) WaitAny(key string, cases ...Case) Case {
	groupPath, group := f.claim(key, "")
	api, ok := f.execution.engine.store.(WaitAnyInfrastructure)
	if !ok {
		f.must(ErrWaitAnyUnsupported)
	}
	if len(cases) == 0 {
		f.must(fmt.Errorf("%w: empty waiting group", ErrInvalid))
	}
	members := make([]*waitCase, len(cases))
	for i, candidate := range cases {
		if candidate == nil {
			f.must(fmt.Errorf("%w: nil case", ErrInvalid))
		}
		member := candidate.member()
		if (member.kind == SleepKind) != (member.encode == nil) {
			f.must(fmt.Errorf("%w: case %q has kind %s", ErrInvalid, member.key, member.kind))
		}
		if !member.used.CompareAndSwap(false, true) {
			f.must(fmt.Errorf("%w: case %q reused; build cases inside the Flow", ErrInvalid, member.key))
		}
		members[i] = member
	}
	// Validate every key before any external registration.
	// 在任何外部注册发生前验证每个 key。
	child := &FlowContext{execution: f.execution, path: groupPath, depth: f.depth, scope: f.scope, strand: f.strand}
	index := make(map[string]int, len(members))
	for i, member := range members {
		if member.idErr != nil {
			child.fail(member.idErr)
		}
		member.path, member.key = child.claim(member.key, member.id)
		index[member.key] = i
	}
	requests := make([]WaitRequest, len(members))
	for i, member := range members {
		request := WaitRequest{Key: member.key, Path: member.path, Type: member.typ, Kind: member.kind,
			Duration: member.duration, Until: member.until}
		if member.encode != nil {
			raw, marshalErr := invoke(member.encode)
			request.Request = raw
			if marshalErr != nil {
				request.Error = marshalErr.Error()
			}
		}
		requests[i] = request
	}
	state, stateErr := api.WaitAny(f.Context(), group, requests)
	if stateErr != nil {
		f.must(&OperationError{group, "WaitAny", stateErr})
	}
	if state == nil {
		f.must(fmt.Errorf("%w: nil WaitAny snapshot", ErrInvalid))
	}
	for _, failure := range state.Failures {
		i, known := index[failure.Key]
		if !known {
			f.must(fmt.Errorf("%w: unknown failed member %s", ErrInvalid, failure.Key))
		}
		members[i].failure = failure.Message
	}
	switch state.Status {
	case Completed:
		kind := state.WinnerKind
		if kind == "" {
			kind = SignalKind
		}
		i, known := index[state.WinnerKey]
		if !known || kind != members[i].kind {
			f.must(fmt.Errorf("%w: winner %s (%s)", ErrChanged, state.WinnerKey, kind))
		}
		if members[i].decode != nil {
			f.must(members[i].decode(state.Response))
		}
		return cases[i]
	case Failed:
		f.abort(fatal(fmt.Errorf("%w: %s: %v", ErrWaitFailed, group, state.Failures)))
	case Suspended:
		if len(state.Pending) == 0 {
			f.must(fmt.Errorf("%w: suspended group has no pending members", ErrInvalid))
		}
		pendings := slices.Clone(state.Pending)
		for j, pending := range pendings {
			if i, known := index[pending.Key]; known && pending.Path == nil {
				pendings[j].Path = members[i].path
			}
			f.execution.engine.emit(f.Context(), Event{Type: WaitSuspended, Key: pending.Key, Path: pendings[j].Path, Kind: pending.Kind,
				Status: Suspended, Pending: []Pending{pendings[j]},
			}, nil)
		}
		f.suspend(pendings...)
	default:
		f.must(ErrInvalid)
	}
	panic("unreachable")
}

// FailWait reports an independent candidate failure. It does not return a
// workflow error when other candidates are still waiting. Duplicate reports or
// reports for already resolved/cancelled members return false.
// FailWait 报告一个独立候选项失败。当其他候选项仍在等待时，它不会返回工作流错误。
// 重复报告，或对已解析/已取消成员的报告，会返回 false。
func (e *Engine) FailWait(ctx context.Context, key string, version int64, cause error) (bool, error) {
	if cause == nil {
		return false, ErrInvalid
	}
	api, ok := e.store.(WaitAnyInfrastructure)
	if !ok {
		return false, ErrWaitAnyUnsupported
	}
	return api.FailWait(ctx, key, version, cause.Error())
}
