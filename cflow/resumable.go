package cflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// FlowDefinition is the common registration interface for every typed Flow.
// Embed FlowBase[I,O] in a concrete definition, or construct one with FlowFunc.
// FlowDefinition 是统一注册接口；业务定义嵌入 FlowBase[I,O] 或通过 FlowFunc 构造。
type FlowDefinition interface {
	bindFlow(FlowDefinition) (flowRunner, error)
}

type flowRunner func(*FlowContext, json.RawMessage) (json.RawMessage, error)

// FlowBase supplies the common registration bridge while keeping typed Run.
// Definitions hold dependencies, never mutable state of an execution.
// FlowBase 提供统一注册适配，保留 Run 的泛型签名；定义保存依赖，不保存执行实例的可变状态。
type FlowBase[I, O any] struct{}

func (FlowBase[I, O]) bindFlow(self FlowDefinition) (flowRunner, error) {
	body, ok := self.(Flow[I, O])
	if !ok || invalidFlow(body) {
		return nil, ErrInvalid
	}
	return func(f *FlowContext, raw json.RawMessage) (json.RawMessage, error) {
		var input I
		if err := decode(raw, &input); err != nil {
			return nil, err
		}
		output, err := body.Run(f, input)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(output)
		if err != nil {
			return nil, fatal(err)
		}
		return data, nil
	}, nil
}

func definitionIdentity(flow FlowDefinition) (any, error) {
	if nilValue(flow) {
		return nil, ErrInvalid
	}
	value := reflect.ValueOf(flow)
	// Concrete definitions are class-like: one registration per Go type.
	// FunctionFlow objects are identified by their pointer so closures with the
	// same I/O types can be registered separately.
	if function, ok := flow.(interface{ functionDefinition() reflect.Type }); ok && function.functionDefinition() == value.Type() {
		return flow, nil
	}
	if value.Kind() != reflect.Pointer && value.Kind() != reflect.Struct {
		return nil, ErrInvalid
	}
	typ := value.Type()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ, nil
}

func (*FunctionFlow[I, O]) functionDefinition() reflect.Type {
	return reflect.TypeFor[*FunctionFlow[I, O]]()
}

// FlowFailure is the serializable failure of an independent execution.
// ActionKey retains errors.As(*ActionError); arbitrary Go error identity is lost.
// Fatal marks a framework failure (drift, misuse, panic): the parent fails too
// instead of receiving it as a business error.
// FlowFailure 保存独立执行的失败。ActionKey 保留 ActionError 类型，任意 Go 错误身份不跨序列化保留。
// Fatal 标记框架故障（漂移、误用、panic）：父级随之失败，而不是作为业务错误交给父级代码。
type FlowFailure struct {
	Message   string
	ActionKey string `json:",omitempty"`
	Fatal     bool   `json:",omitempty"`
}

func (f *FlowFailure) Err() error {
	if f == nil {
		return nil
	}
	if f.Fatal {
		return fatal(errors.New(f.Message))
	}
	if f.ActionKey != "" {
		return &ActionError{Key: f.ActionKey, Message: f.Message}
	}
	return errors.New(f.Message)
}

// LoopSpec is the identity and parent routing of an experimental loop.
// ParentKey == "" is the root run; otherwise the owning child. Concurrency is
// the admission window fixed at registration; it is not part of the identity.
// DiscardOutputs marks a ForItems batch whose outputs are never loaded.
// LoopSpec 保存循环身份和父级路由。ParentKey 为空表示根运行，否则表示所属子级。
// Concurrency 是登记时固定的激活窗口，不属于身份；DiscardOutputs 标记 ForItems 批次，其输出不会被加载。
type LoopSpec struct {
	Key, ParentKey string
	Path           Path
	Workflow       string
	FlowName       string
	ItemsHash      string
	Total, Depth   int
	Concurrency    int  `json:",omitempty"`
	DiscardOutputs bool `json:",omitempty"`
}

// Window is the number of items admitted at registration: Concurrency
// (at least 1) capped by Total.
// Window 是登记时激活的元素数：Concurrency（至少为 1），不超过 Total。
func (s LoopSpec) Window() int {
	return max(min(s.Concurrency, s.Total), min(1, s.Total))
}

// LoopState stores O(1) progress; outputs live in individual FlowExecution rows.
// Items [0, AdmittedCount) have been admitted, RunningCount of them are not yet
// terminal. Failure records the first terminal item failure; once set no more
// items are admitted, and the loop ends when the running items finish.
// LoopState 保存常量大小的进度；输出分别保存在每个 FlowExecution 中。
// 下标 [0, AdmittedCount) 已激活，其中 RunningCount 个尚未终结。Failure 记录首个终态失败；
// 之后不再激活新元素，运行中的元素结束后循环终结。
type LoopState struct {
	LoopSpec
	Status         Status
	Version        int64
	AdmittedCount  int
	RunningCount   int
	CompletedCount int
	FailedKey      string
	FailedIndex    int
	Failure        *FlowFailure
}

const (
	LoopKind Kind = "LOOP"
	FlowKind Kind = "FLOW"
	// Queued is an item not yet admitted by its loop's window.
	// Queued 表示尚未被循环窗口激活的元素。
	Queued Status = "QUEUED"
)

// FlowExecution is one durable, independently addressed item execution.
// Running includes durable suspension: pending wait records hold the deadlines.
// FlowExecution 是可独立寻址的持久元素执行。Running 包含持久等待，截止时间保存在等待记录中。
type FlowExecution struct {
	Key, LoopKey, ParentKey string
	Path                    Path
	Workflow, FlowName      string
	Index, Depth            int
	Status                  Status
	Version                 int64
	Input, Output           json.RawMessage
	Failure                 *FlowFailure
}

// FlowInfrastructure stores independent RunFlow, GoFlow and ForEach children.
// Experimental: API and storage format are unstable.
// All snapshots must be deep copies. StartFlow atomically registers immutable
// input/definition/parent and a child wake, or returns the existing execution.
// For standalone children CommitFlow saves the terminal result and wakes the
// parent atomically. StartLoop atomically validates/registers the entire batch
// and enqueues the first Window() children (empty batches complete immediately).
// CommitFlow fences by the current child lease and version; it atomically saves
// the terminal child, updates the counters exactly once, and enqueues the next
// queued child or, once the loop is terminal, the parent. A failure stops
// admission; the parent wakes after the running children finish. Repeated
// terminal commits return the first outcome without another count/wake.
// LoopResults returns every output in original index order once the loop has
// completed; failed and DiscardOutputs loops return none. No call may load other
// items' internal step records.
// FlowInfrastructure 是独立子 Flow 的实验性可选存储契约。返回值须深拷贝。
// StartFlow 原子登记输入、定义、父级及子级唤醒；重复调用返回已有执行。
// 独立子级 CommitFlow 原子保存终态并唤醒父级。
// StartLoop 原子登记整批，并唤醒前 Window() 个元素；CommitFlow 校验当前子项租约和版本，
// 原子保存终态、只计数一次，并唤醒下一个排队元素，或在循环终结时唤醒父级。
// 失败停止激活，运行中的元素结束后唤醒父级。
// LoopResults 在循环成功后按下标返回全部输出；失败或 DiscardOutputs 的循环不返回输出。
// 不读取各项内部步骤历史。
type FlowInfrastructure interface {
	GetLoop(context.Context, string) (*LoopState, error)
	StartLoop(context.Context, LoopSpec, []FlowExecution) (*LoopState, error)
	GetFlow(context.Context, string) (*FlowExecution, error)
	StartFlow(context.Context, FlowExecution) (*FlowExecution, error)
	CommitFlow(context.Context, Lease, *FlowExecution, int64) (*FlowExecution, error)
	LoopResults(context.Context, string) ([]json.RawMessage, error)
}

var ErrFlowUnsupported = errors.New("infrastructure does not support resumable flows")

// resumeFlow resumes only the independent child at key; see Resume.
// resumeFlow 只恢复 key 上的独立子级，见 Resume。
func (e *Engine) resumeFlow(ctx context.Context, key string) (result Result[json.RawMessage], err error) {
	result.Status = Running
	api, ok := e.store.(FlowInfrastructure)
	if !ok {
		return result, ErrFlowUnsupported
	}
	err = e.leases.Run(ctx, key, func(leaseCtx context.Context, token Lease) error {
		item, getErr := api.GetFlow(leaseCtx, key)
		if getErr != nil {
			return &OperationError{key, "GetFlow", getErr}
		}
		if item == nil {
			return ErrNotFound
		}
		if item.Status == Completed || item.Status == Failed {
			result.Status, result.Output = item.Status, item.Output
			return item.Failure.Err()
		}
		if item.Status != Running {
			return ErrConflict
		}
		def := e.flows[item.FlowName]
		if def == nil {
			// Possibly a rolling deploy: a worker that knows the name may resume it.
			// 可能处于滚动发布：认识该名字的 worker 可以恢复它。
			return fmt.Errorf("%w: register flow %s", ErrInvalid, item.FlowName)
		}
		eventCtx := e.flowEventContext(leaseCtx, item)
		started := e.options.Now()
		e.emit(eventCtx, Event{Type: DriveStarted, Key: key, Path: item.Path, Kind: FlowKind, Status: Running, StartedAt: started}, nil)
		var driveErr error
		defer func() {
			e.emit(eventCtx, Event{Type: DriveFinished, Key: key, Path: item.Path, Kind: FlowKind,
				Status: result.Status, StartedAt: started, Pending: result.Pending}, driveErr)
		}()
		if item.Key != e.buildKey(item.Path) || item.Depth > e.options.MaxFlowDepth {
			// Deterministic on every worker: fail the child (and its parent)
			// instead of retrying forever.
			// 在任何 worker 上都确定复现：令子级（及父级）失败，而不是无限重试。
			result.Status = Failed
			driveErr = fatal(fmt.Errorf("%w: flow %s no longer matches BuildKey or MaxFlowDepth", ErrInvalid, key))
		} else {
			body := FlowFunc[json.RawMessage, json.RawMessage](def)
			result, driveErr = e.execute(eventCtx, body, item.Input, item.Path, item.Depth, key, &token)
		}
		if result.Status != Completed && result.Status != Failed {
			return driveErr
		}
		item.Status, item.Output = result.Status, result.Output
		if driveErr != nil {
			item.Failure = &FlowFailure{Message: driveErr.Error()}
			var fatalErr *fatalError
			var actionErr *ActionError
			if errors.As(driveErr, &fatalErr) {
				item.Failure.Fatal = true
			} else if errors.As(driveErr, &actionErr) {
				item.Failure = &FlowFailure{Message: actionErr.Message, ActionKey: actionErr.Key}
			}
		}
		if _, commitErr := api.CommitFlow(leaseCtx, token, item, item.Version); commitErr != nil {
			result.Status = Running
			driveErr = &OperationError{key, "CommitFlow", commitErr}
			return driveErr
		}
		return driveErr
	})
	return result, err
}
