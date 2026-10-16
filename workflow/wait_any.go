package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// WaitCase describes a signal. A zero Kind means an external WaitFor signal,
// preserving existing callers. Duration is used only for relative Sleep;
// a nonzero Until selects an absolute sleep deadline.
// WaitCase 描述一个信号。Kind 为零值表示外部 WaitFor 信号，以保持现有调用方兼容。
// Duration 仅用于相对 Sleep；非零 Until 表示绝对睡眠截止时间。
type WaitCase[R any] struct {
	Key      string
	Request  R
	Kind     Kind
	Duration time.Duration
	Until    time.Time
}

// Signal is the heterogeneous, transportable description accepted by WaitAny.
// Custom kinds require an Infrastructure implementation supporting their
// registration and resolution semantics; unknown kinds are never silently
// treated as WaitFor by the reference memory store.
// Signal 是 WaitAny 接受的异构、可传输描述。自定义 Kind 需要 Infrastructure
// 实现支持其注册和解析语义；参考内存存储绝不会把未知 Kind 静默当作 WaitFor 处理。
type Signal = WaitCase[any]

// WaitForSignal describes a signal resolved by an external JSON response.
// WaitForSignal 描述由外部 JSON 响应解析的信号。
func WaitForSignal[R any](key string, request R) Signal {
	return Signal{Key: key, Kind: SignalKind, Request: request}
}

// SleepSignal describes a timer whose relative deadline is fixed on registration.
// SleepSignal 描述一个在注册时固定相对截止时间的计时器。
func SleepSignal(key string, duration time.Duration) Signal {
	return Signal{Key: key, Kind: SleepKind, Duration: duration}
}

// SleepUntilSignal describes a timer with an absolute deadline.
// SleepUntilSignal 描述一个具有绝对截止时间的计时器。
func SleepUntilSignal(key string, until time.Time) Signal {
	return Signal{Key: key, Kind: SleepKind, Until: until}
}

type WaitFailure struct {
	Key     string
	Message string
}

type WaitAnyResult[R any] struct {
	Key      string // resolved winner key, e.g. approval|primary
	// 已解析的赢家 key，例如 approval|primary。
	Kind     Kind   // SleepKind winners have a zero Response, not a JSON payload
	// SleepKind 赢家的 Response 为零值，而不是 JSON 负载。
	Response R
	Failures []WaitFailure // isolated candidate errors; not a drive failure
	// 被隔离的候选项错误；不属于驱动失败。
}

// WaitRequest is the transport representation consumed by storage adapters.
// WaitRequest 是存储适配器消费的传输层表示。
type WaitRequest struct {
	Key      string
	Request  json.RawMessage
	Kind     Kind
	Duration time.Duration
	Until    time.Time
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
// existing group's snapshot after checking its ordered keys and request bytes.
// Group ownership must be persisted. The existing Resolve operation MUST, in
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
// WaitAny 会原子地注册所有信号和请求通知，或在检查有序 key 与请求字节后读取
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

// WaitAny registers all candidates simultaneously. It returns a suspended error
// until one signal completes; Engine.Drive reports ALL pending keys. The first
// accepted callback cancels losers immediately in storage. On the next Drive
// the workflow resumes and returns the durable winner, without awaiting losers.
//
// Invalid key topology is rejected before registration. A candidate whose
// request cannot be encoded is isolated in Failures while healthy cases remain.
// Payload-bearing signals share response type R; callbacks must send matching
// JSON. A Sleep winner has Kind=SleepKind and a zero response.
// WaitAny 会同时注册所有候选项。在某个信号完成前，它返回 suspended 错误；
// Engine.Drive 会报告所有 pending key。第一个被接受的回调会立即在存储中
// 取消失败方。下一次 Drive 时，工作流会恢复并返回持久化的赢家，而不会等待失败方。
//
// 无效 key 拓扑会在注册前被拒绝。请求无法编码的候选项会被隔离到 Failures 中，
// 同时保留健康的候选项。携带负载的信号共享响应类型 R；回调必须发送匹配的 JSON。
// Sleep 赢家的 Kind=SleepKind，响应为零值。
func (f *FlowContext) WaitAny[R, Q any](key string, cases ...WaitCase[Q]) (result WaitAnyResult[R], err error) {
	defer f.remember(&err)
	group, err := f.claim(key)
	if err != nil {
		return result, err
	}
	api, ok := f.execution.engine.store.(WaitAnyInfrastructure)
	if !ok {
		return result, ErrWaitAnyUnsupported
	}
	if len(cases) == 0 {
		return result, fmt.Errorf("%w: empty waiting group", ErrInvalid)
	}
	child := &FlowContext{execution: f.execution, prefix: group}
	// Validate every key before any external registration.
	// 在任何外部注册发生前验证每个 key。
	keys := make([]string, len(cases))
	for i, candidate := range cases {
		keys[i], err = child.claim(candidate.Key)
		if err != nil {
			return result, err
		}
	}
	var requests []WaitRequest
	var failures []WaitFailure
	for i, candidate := range cases {
		kind := candidate.Kind
		if kind == "" {
			kind = SignalKind
		}
		var raw json.RawMessage
		var marshalErr error
		if kind != SleepKind {
			raw, marshalErr = invoke(func() (json.RawMessage, error) { return json.Marshal(candidate.Request) })
		}
		if marshalErr != nil {
			failures = append(failures, WaitFailure{Key: keys[i], Message: marshalErr.Error()})
			continue
		}
		requests = append(requests, WaitRequest{
			Key: keys[i], Request: raw, Kind: kind,
			Duration: candidate.Duration, Until: candidate.Until,
		})
	}
	if len(requests) == 0 {
		result.Failures = failures
		return result, Permanent(fmt.Errorf("wait group %s has no usable candidates: %v", group, failures))
	}
	state, stateErr := api.WaitAny(f.Context(), group, requests)
	if stateErr != nil {
		return result, &OperationError{group, "WaitAny", stateErr}
	}
	if state == nil {
		return result, fmt.Errorf("%w: nil WaitAny snapshot", ErrInvalid)
	}
	result.Failures = append(failures, state.Failures...)
	switch state.Status {
	case Completed:
		result.Key = state.WinnerKey
		result.Kind = state.WinnerKind
		if result.Kind == "" {
			result.Kind = SignalKind
		}
		if result.Kind == SleepKind {
			return result, nil
		}
		err = json.Unmarshal(state.Response, &result.Response)
		return result, err
	case Failed:
		return result, Permanent(fmt.Errorf("wait group %s failed: %v", group, state.Failures))
	case Suspended:
		if len(state.Pending) == 0 {
			return result, fmt.Errorf("%w: suspended group has no pending members", ErrInvalid)
		}
		f.execution.mu.Lock()
		for _, pending := range state.Pending {
			f.execution.pending[pending.Key] = pending
		}
		f.execution.mu.Unlock()
		for _, pending := range state.Pending {
			f.execution.engine.emit(f.Context(), Event{Type: WaitSuspended, Key: pending.Key, Kind: pending.Kind,
				Status: Suspended, Pending: []Pending{pending},
			}, nil)
		}
		return result, &SuspendedError{Pending: state.Pending[0]}
	default:
		return result, ErrInvalid
	}
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
