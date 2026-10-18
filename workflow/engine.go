package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/k0923/go/lease"
)

type Options struct {
	LeaseTTL       time.Duration
	CleanupTimeout time.Duration
	// MaxFlowDepth bounds how deeply Flows may mount Flows (RunFlow/GoFlow
	// from inside a Flow). Default 8. Exceeding it fails the drive with
	// ErrInvalid, deterministically on every replay.
	// MaxFlowDepth 限制 Flow 挂载 Flow 的深度（在 Flow 内调用 RunFlow/GoFlow）。默认 8。
	// 超过时 drive 以 ErrInvalid 失败，且每次回放都确定复现。
	MaxFlowDepth int
	// Now supplies workflow clock reads; production must agree with the store
	// clock. Lease validity is ALWAYS decided by the store. When nil, a store
	// implementing Clock supplies it, otherwise time.Now.
	// Now 提供工作流时钟读取；生产环境中它必须与存储时钟一致。
	// 租约有效性始终由存储决定。为空时，若存储实现了 Clock 则使用存储时钟，否则使用 time.Now。
	Now func() time.Time
}

// Engine executes ONE definition replay per Drive. It is an execution kernel:
// run-level state (created/finished/output), scheduling and versioning belong
// to the caller. Bind each run to the definition version that started it; a
// hotfix ships as a new version for new runs while old runs keep their code.
// Engine 每次 Drive 执行一次定义回放。它是执行内核：run 级状态（创建/结束/输出）、
// 调度与版本管理归调用方。每个 run 绑定启动它的定义版本；热修复以新版本发布给
// 新 run，老 run 继续运行原代码。
type Engine struct {
	store    Infrastructure
	options  Options
	leases   *lease.Runner
	eventID  string
	events   eventHub
	buildKey func(Path) string
}

func NewEngine(store Infrastructure, options Options) (*Engine, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: infrastructure required", ErrInvalid)
	}
	if options.LeaseTTL == 0 {
		options.LeaseTTL = 30 * time.Second
	}
	if options.CleanupTimeout == 0 {
		options.CleanupTimeout = 5 * time.Second
	}
	if options.MaxFlowDepth == 0 {
		options.MaxFlowDepth = 8
	}
	if options.LeaseTTL < 3*time.Nanosecond || options.CleanupTimeout <= 0 || options.MaxFlowDepth < 0 {
		return nil, ErrInvalid
	}
	if options.Now == nil {
		options.Now = time.Now
		if clock, ok := store.(Clock); ok {
			options.Now = clock.Now
		}
	}
	leases, err := lease.NewRunner(store, lease.Options{
		TTL: options.LeaseTTL, CleanupTimeout: options.CleanupTimeout,
	})
	if err != nil {
		return nil, err
	}
	buildKey := DefaultKey
	if builder, ok := store.(KeyBuilder); ok {
		buildKey = builder.BuildKey
	}
	return &Engine{store: store, options: options, leases: leases, eventID: uuid.NewString(), buildKey: buildKey}, nil
}

// ActionKey is stable within a scoped run. For external idempotency compose
// it with your run identity; do not use the generation as an idempotency key.
// ActionKey 在带作用域的运行中保持稳定。用于外部幂等时，需要与运行身份组合；
// 不要将 generation 用作幂等键。
func ActionKey(ctx context.Context) string {
	key, _ := ctx.Value(actionKey{}).(string)
	return key
}

type actionKey struct{}

// execution is the state of ONE Drive. abort holds framework failures
// (infrastructure, retryable Action failures, misuse, drift); they never reach
// orchestration code as errors, they halt it.
// execution 是一次 Drive 的状态。abort 保存框架故障（基础设施、可重试 Action
// 失败、误用、漂移）；它们不会以 error 形式交给编排代码，而是停止编排。
type execution struct {
	engine *Engine
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	// keys maps every claimed key to its path, so a non-injective BuildKey is
	// told apart from a genuinely duplicated step.
	// keys 记录每个已占用 key 对应的路径，以区分非单射的 BuildKey 与真正重复的步骤。
	keys    map[string]Path
	abort   []error
	pending map[string]Pending
	// records holds terminal records preloaded by Drive; read-only afterwards.
	// records 保存 Drive 预加载的终态记录；之后只读。
	records map[string]*Record
	futures []observable
	closed  bool
	wg      sync.WaitGroup
}

var localKey = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// flowScope joins asynchronous descendants before this Flow returns to its
// caller. Mutable fields are protected by execution.mu.
type flowScope struct {
	wg     sync.WaitGroup
	closed bool
}

// FlowContext provides Action, wait and Flow-mounting primitives. Flows may
// mount Flows; Options.MaxFlowDepth bounds the nesting. Actions receive a
// standard context instead of either workflow context.
//
// Primitives never return framework errors. A pending wait or an aborted drive
// unwinds the calling goroutine with a private panic; deferred functions run,
// so defers in orchestration code must not perform side effects. Do not
// recover() blindly in orchestration code: even if you do, every subsequent
// primitive on the same goroutine halts again and nothing new is persisted.
// FlowContext 提供 Action、等待与挂载 Flow 的原语。Flow 可以挂载 Flow，嵌套深度由
// Options.MaxFlowDepth 限制。Action 接收标准 context，不接收任一工作流上下文。
//
// 原语从不返回框架错误。挂起的等待或被中止的 drive 会用私有 panic 展开当前
// goroutine；defer 仍会执行，因此编排代码中的 defer 不得产生副作用。不要在编排
// 代码中盲目 recover()：即使这样做，同一 goroutine 上的后续原语都会再次停止，
// 不会持久化任何新进度。
type FlowContext struct {
	execution *execution
	path      Path
	depth     int // mounted Flow depth; the root Flow is 0
	scope     *flowScope
	strand    *strand
}

func (f *FlowContext) Context() context.Context { return f.execution.ctx }

// Path is the structured address of this Flow's namespace.
// Path 是当前 Flow 命名空间的结构化地址。
func (f *FlowContext) Path() Path { return slices.Clone(f.path) }

// Key is the built key of this Flow's namespace ("" at the root).
// Key 是当前 Flow 命名空间构建出的 key（根为 ""）。
func (f *FlowContext) Key() string {
	if len(f.path) == 0 {
		return ""
	}
	return f.execution.engine.buildKey(f.path)
}

// Scope returns a child namespace name[id] without recording anything. Use it
// for loops or branches whose steps keep fixed names, e.g.
//
//	s := f.Scope("step", strconv.Itoa(i))
//	s.Run("llm", llm, req) // step[0]|llm
//
// id must be deterministic (an index, a business ID), never time or random.
// The scope shares the caller's strand and async joining.
// Scope 返回子命名空间 name[id]，本身不产生记录，用于让步骤保持固定名字的循环或分支。
// id 必须确定（下标、业务 ID），不能是时间或随机数。Scope 与调用方共享 strand 和异步汇合。
func (f *FlowContext) Scope(name string, id string) *FlowContext {
	path, _ := f.claim(name, id)
	return &FlowContext{execution: f.execution, path: path, depth: f.depth, scope: f.scope, strand: f.strand}
}

// signature renders the leaf type of a step for observation, e.g.
// "action[int,string]". It is recorded and emitted, never compared.
// signature 生成步骤的叶子类型，例如 "action[int,string]"。它只被记录和发出，从不参与比较。
func signature[I, O any](name string) string {
	return name + "[" + reflect.TypeFor[I]().String() + "," + reflect.TypeFor[O]().String() + "]"
}

// sameStep is the replay identity check: key (already matched) + kind, plus
// the stored path when present (catches a non-injective KeyBuilder across
// drives). Types and inputs are observed, never compared.
// sameStep 是回放身份校验：key（已匹配）+ kind，以及存在时的存储路径（跨 drive 兜底非单射
// KeyBuilder）。类型与输入只观测、不比较。
func sameStep(r *Record, kind Kind, path Path) bool {
	return r.Kind == kind && (r.Path == nil || r.Path.Equal(path))
}

// replayed reports, without failing, a replayed step whose current input or
// leaf type differs from the persisted one; the persisted outcome is used.
// replayed 报告（但不失败）当前输入或叶子类型与持久化值不同的回放步骤；仍使用持久化结果。
func (f *FlowContext) replayed(r *Record, typ string, input json.RawMessage) {
	e := f.execution.engine
	if !sameJSON(r.Input, input) {
		e.emit(f.Context(), Event{Type: InputChanged, Key: r.Key, Path: r.Path, Kind: r.Kind,
			Status: r.Status, RecordVersion: r.Version, StepType: typ,
		}, nil)
	}
	if r.Type != "" && r.Type != typ {
		e.emit(f.Context(), Event{Type: TypeChanged, Key: r.Key, Path: r.Path, Kind: r.Kind,
			Status: r.Status, RecordVersion: r.Version, StepType: typ, StoredType: r.Type,
		}, nil)
	}
}

// halt marks the strand and unwinds it. Recovering this value cannot resume
// progress because every primitive checks the strand first.
// halt 标记 strand 并展开调用栈。recover 该值无法继续推进，因为每个原语都会先检查 strand。
func (f *FlowContext) halt() {
	f.strand.halted.Store(true)
	panic(haltSignal{})
}

// abort records a framework failure, cancels sibling IO and halts.
// abort 记录框架故障，取消兄弟 IO 并停止。
func (f *FlowContext) abort(err error) {
	e := f.execution
	e.mu.Lock()
	e.abort = append(e.abort, err)
	e.mu.Unlock()
	e.cancel()
	f.halt()
}

// suspend records pending waits and halts this strand only. Sibling strands
// keep running so every independent wait is registered in the same drive.
// suspend 记录挂起的等待并仅停止当前 strand。兄弟 strand 继续运行，
// 以便在同一次 drive 中登记所有独立等待。
func (f *FlowContext) suspend(pending ...Pending) {
	e := f.execution
	e.mu.Lock()
	for _, p := range pending {
		e.pending[p.Key] = p
	}
	e.mu.Unlock()
	f.halt()
}

// claim reserves the address name[id] for a primitive or halts. Halted
// strands, cancelled or closed executions, invalid names or IDs, duplicate
// addresses and non-injective key builders never reach storage.
// claim 为原语预留地址 name[id]，否则停止。已停止的 strand、已取消或关闭的执行、
// 无效名称或 ID、重复地址以及非单射的 key 构建器都不会触达存储。
func (f *FlowContext) claim(name, id string) (Path, string) {
	if f.strand.halted.Load() {
		f.halt()
	}
	if !localKey.MatchString(name) {
		f.abort(fatal(fmt.Errorf("%w: key %q", ErrInvalid, name)))
	}
	if err := validID(id); err != nil {
		f.abort(fatal(err))
	}
	path := f.path.child(name, id)
	key := f.execution.engine.buildKey(path)
	e := f.execution
	e.mu.Lock()
	if e.closed || (f.scope != nil && f.scope.closed) {
		e.mu.Unlock()
		f.abort(fatal(fmt.Errorf("%w: execution closed", ErrInvalid)))
	}
	if e.ctx.Err() != nil {
		e.mu.Unlock()
		f.halt() // the cause is already recorded or comes from the caller's ctx
	}
	if previous, ok := e.keys[key]; ok {
		e.mu.Unlock()
		if !previous.Equal(path) {
			f.abort(fatal(fmt.Errorf("%w: BuildKey not injective: %s and %s both map to %q", ErrInvalid, previous, path, key)))
		}
		f.abort(fatal(fmt.Errorf("%w: duplicate key %s", ErrInvalid, key)))
	}
	if key == "" || key == definitionKey {
		e.mu.Unlock()
		f.abort(fatal(fmt.Errorf("%w: BuildKey returned reserved key %q for %s", ErrInvalid, key, path)))
	}
	e.keys[key] = path
	e.mu.Unlock()
	return path, key
}

// claimInput claims name[input.StepKey()].
// claimInput 占用 name[input.StepKey()]。
func (f *FlowContext) claimInput(name string, input any) (Path, string) {
	id, err := stepID(input)
	if err != nil {
		f.fail(err)
	}
	return f.claim(name, id)
}

// fail aborts with a deterministic error after the strand check, so a halted
// strand halts instead of reporting a secondary failure.
// fail 在 strand 检查之后以确定性错误中止，使已停止的 strand 只停止而不报告次生故障。
func (f *FlowContext) fail(err error) {
	if f.strand.halted.Load() {
		f.halt()
	}
	f.abort(fatal(err))
}

// must converts a framework error into an abort. Drift and decoding errors are
// deterministic and therefore fatal; storage errors stay retryable.
// must 把框架错误转换为中止。漂移和解码错误是确定性的，因此是致命的；存储错误仍可重试。
func (f *FlowContext) must(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, ErrChanged) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrSignalUnsupported) ||
		errors.Is(err, ErrWaitAnyUnsupported) {
		err = fatal(err)
	}
	f.abort(err)
}

func decode[T any](raw json.RawMessage, output *T) error {
	if err := json.Unmarshal(raw, output); err != nil {
		return fatal(fmt.Errorf("decode record: %w", err))
	}
	return nil
}

// runStrand executes orchestration code, converting a halt into a flag and any
// other panic into a fatal Flow failure (a bug that replay cannot fix).
// runStrand 执行编排代码，把 halt 转换为标记，把其他 panic 转换为致命的 Flow
// 失败（重放无法修复的 bug）。
func runStrand[T any](s *strand, fn func() (T, error)) (output T, err error, halted bool) {
	defer func() {
		if value := recover(); value != nil {
			if _, ok := value.(haltSignal); ok {
				s.halted.Store(true)
			} else {
				err = fatal(&PanicError{Value: value, Stack: debug.Stack()})
			}
		}
		if s.halted.Load() {
			var zero T
			output, err, halted = zero, nil, true
		}
	}()
	output, err = fn()
	return output, err, false
}

// Drive replays the definition from its entry point. It joins all registered
// async work before returning, including work the caller forgot to Wait().
//
// Result.Status: Completed (Output valid), Suspended (Pending lists every
// wait), Failed (err is terminal: an escaped business error, definition drift,
// misuse or an orchestration panic), Running (err is retryable: infrastructure,
// lease conflict, retryable Action failure or cancellation; re-drive later).
// Drive 从入口点回放定义。返回前它会等待所有已注册的异步工作，包括调用方忘记
// Wait 的工作。
//
// Result.Status：Completed（Output 有效）、Suspended（Pending 列出全部等待）、
// Failed（err 为终态：逃逸的业务错误、定义漂移、误用或编排 panic）、
// Running（err 可重试：基础设施、租约冲突、可重试 Action 失败或取消；稍后重新驱动）。
func (e *Engine) Drive[I, O any](ctx context.Context, workflow Workflow[I, O], input I) (result Result[O], err error) {
	result.Status = Running
	eventCtx := ctx
	if eventCtx == nil {
		eventCtx = context.Background()
	}
	eventCtx = context.WithValue(eventCtx, eventContextKey{}, eventContext{
		executionID: uuid.NewString(), name: workflow.Name, version: workflow.Version,
	})
	eventStarted := e.options.Now()
	e.emit(eventCtx, Event{Type: DriveStarted, Key: workflow.Name, Status: Running, StartedAt: eventStarted}, nil)
	defer func() {
		e.emit(eventCtx, Event{
			Type: DriveFinished, Key: workflow.Name, Status: result.Status,
			StartedAt: eventStarted, Pending: result.Pending,
		}, err)
	}()
	if ctx == nil || invalidFlow(workflow.root) || !localKey.MatchString(workflow.Name) || workflow.Version == "" {
		return result, ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	identity, err := json.Marshal(struct {
		Name, Version string
		Input         I
	}{workflow.Name, workflow.Version, input})
	if err != nil {
		return result, err
	}
	definition, err := e.store.Put(ctx, &Record{
		Key: definitionKey, Path: Path{{Name: definitionKey}}, Kind: DefinitionKind, Status: Completed, Input: identity,
	})
	if err != nil {
		return result, &OperationError{Key: definitionKey, Op: "Put", Cause: err}
	}
	if !sameJSON(definition.Input, identity) {
		result.Status = Failed
		return result, fatal(ErrChanged)
	}
	var records map[string]*Record
	if lister, ok := e.store.(RecordLister); ok {
		all, listErr := lister.Records(ctx)
		if listErr != nil {
			return result, &OperationError{Key: "$records", Op: "Records", Cause: listErr}
		}
		records = terminalRecords(all)
	}
	runCtx, cancel := context.WithCancel(eventCtx)
	defer cancel()
	ex := &execution{engine: e, ctx: runCtx, cancel: cancel, keys: make(map[string]Path),
		pending: make(map[string]Pending), records: records}
	root := &strand{}
	rootCtx := &FlowContext{execution: ex, strand: root}
	output, handlerErr, _ := runStrand(root, func() (O, error) { return rootCtx.runChildFlow(nil, workflow.root, input) })
	ex.wg.Wait()
	ex.mu.Lock()
	ex.closed = true
	abortErr := errors.Join(ex.abort...)
	var unobserved []error
	for _, future := range ex.futures {
		if futureErr := future.unobserved(); futureErr != nil {
			unobserved = append(unobserved, futureErr)
		}
	}
	unobservedErr := errors.Join(unobserved...)
	for _, pending := range ex.pending {
		result.Pending = append(result.Pending, pending)
	}
	ex.mu.Unlock()
	sort.Slice(result.Pending, func(i, j int) bool { return result.Pending[i].Key < result.Pending[j].Key })
	switch {
	case abortErr != nil:
		// Framework failures win over suspension: a drive that lost progress
		// must be retried (or fail) rather than report a stable wait.
		// 框架故障优先于挂起：丢失进度的 drive 必须重试（或失败），而不是报告稳定等待。
		err = abortErr
		var fatalErr *fatalError
		if errors.As(err, &fatalErr) {
			result.Status = Failed
		}
	case ctx.Err() != nil:
		err = ctx.Err()
	case len(result.Pending) > 0:
		// A halted root ignores whatever the handler returned.
		// 已停止的根 strand 会忽略处理函数的任何返回值。
		result.Status = Suspended
	case root.halted.Load():
		// Only reachable if orchestration recovered a halt and swallowed it.
		// 仅当编排 recover 并吞掉 halt 时才可能到达。
		result.Status = Failed
		err = fatal(fmt.Errorf("%w: orchestration recovered a workflow halt", ErrInvalid))
	case handlerErr != nil:
		// The handler decides failure: only an error it RETURNS is terminal.
		// 由处理函数决定失败：只有它返回的 error 才是终态。
		result.Status = Failed
		err = handlerErr
	case unobservedErr != nil:
		// A Go/GoFlow business error nobody waited for is not silently dropped.
		// 没有人等待的 Go/GoFlow 业务错误不会被静默丢弃。
		result.Status = Failed
		err = unobservedErr
	default:
		result.Output = output
		result.Status = Completed
	}
	return result, err
}

// terminalRecords keeps outcomes that can never change, so replaying them from
// a snapshot is equivalent to reading the store: finished Actions, resolved
// Signal/Sleep records and checkpoints.
// terminalRecords 只保留永不改变的结果，因此从快照回放等价于读取存储：
// 已结束的 Action、已解析的 Signal/Sleep 记录和检查点。
func terminalRecords(all []*Record) map[string]*Record {
	records := make(map[string]*Record, len(all))
	for _, r := range all {
		if r == nil {
			continue
		}
		switch {
		case r.Kind == ActionKind && (r.Status == Completed || r.Status == Failed),
			(r.Kind == SignalKind || r.Kind == SleepKind || r.Kind == CheckpointKind) && r.Status == Completed:
			records[r.Key] = r
		}
	}
	return records
}

// definitionKey addresses the run's definition record. It cannot collide with
// a step: step names match ^[A-Za-z0-9_]+$ and claim rejects it from BuildKey.
// definitionKey 是 run 定义记录的地址。它不会与步骤冲突：步骤名匹配 ^[A-Za-z0-9_]+$，
// 且 claim 会拒绝 BuildKey 返回它。
const definitionKey = "$definition"

// Run executes an Action once per run. The returned error is always nil or a
// committed *ActionError (business failure, possibly after exhausting Retry);
// handle it or return it. Retryable failures and infrastructure faults abort
// the drive instead of returning. The step address is name[input.StepKey()]
// when the input implements StepKeyer, otherwise name.
// Run 在每个 run 中执行一次 Action。返回的 error 只会是 nil 或已提交的
// *ActionError（业务失败，可能是用尽 Retry 之后）；处理它或返回它。
// 可重试失败和基础设施故障会中止 drive，而不是返回。输入实现 StepKeyer 时步骤地址为
// name[input.StepKey()]，否则为 name。
func (f *FlowContext) Run[I, O any](name string, action Action[I, O], input I, options ...ActionOptions) (O, error) {
	path, key := f.claimInput(name, input)
	output, actionErr, err := f.runAction(path, key, action, input, options...)
	f.must(err)
	if actionErr != nil {
		return output, actionErr
	}
	return output, nil
}

// runAction returns (output, committed business failure, framework failure).
// runAction 返回（输出、已提交的业务失败、框架故障）。
func (f *FlowContext) runAction[I, O any](path Path, key string, action Action[I, O], input I, options ...ActionOptions) (output O, business *ActionError, err error) {
	e := f.execution.engine
	typ := signature[I, O]("action")
	eventStarted := e.options.Now()
	actionStatus := Running
	var recordVersion int64
	replayed := false
	defer func() {
		eventErr := err
		if business != nil {
			eventErr = business
		}
		e.emit(f.Context(), Event{
			Type: ActionFinished, Key: key, Path: path, Kind: ActionKind, Status: actionStatus, StepType: typ,
			StartedAt: eventStarted, Replayed: replayed, RecordVersion: recordVersion,
		}, eventErr)
	}()
	if invalidAction(action) || len(options) > 1 {
		return output, nil, ErrInvalid
	}
	var option ActionOptions
	if len(options) == 1 {
		option = options[0]
	}
	if option.Timeout < 0 || option.Retry.MaxAttempts < 0 {
		return output, nil, ErrInvalid
	}
	data, err := json.Marshal(input)
	if err != nil {
		return output, nil, fatal(err)
	}
	started := e.options.Now()
	record := f.execution.records[key]
	if record == nil {
		if record, err = e.store.Get(f.Context(), key); err != nil {
			return output, nil, &OperationError{key, "Get", err}
		}
	}
	// replay reports done=true when the record already holds the outcome.
	// replay 在记录已经包含结果时返回 done=true。
	replay := func(r *Record) (bool, error) {
		if r == nil {
			return false, nil
		}
		if !sameStep(r, ActionKind, path) {
			return true, ErrChanged
		}
		switch r.Status {
		case Completed:
			f.replayed(r, typ, data)
			decodeErr := decode(r.Result, &output)
			replayed, actionStatus, recordVersion = true, r.Status, r.Version
			e.emit(f.Context(), Event{Type: ActionReplayed, Key: key, Path: path, Kind: ActionKind, StepType: typ,
				Status: r.Status, StartedAt: eventStarted, Replayed: true, RecordVersion: r.Version,
			}, decodeErr)
			return true, decodeErr
		case Failed:
			f.replayed(r, typ, data)
			business = &ActionError{Key: key, Message: r.Error}
			replayed, actionStatus, recordVersion = true, r.Status, r.Version
			e.emit(f.Context(), Event{Type: ActionReplayed, Key: key, Path: path, Kind: ActionKind, StepType: typ,
				Status: r.Status, StartedAt: eventStarted, Replayed: true, RecordVersion: r.Version,
			}, business)
			return true, nil
		case Running:
			return false, nil
		default:
			return true, ErrInvalid
		}
	}
	if done, replayErr := replay(record); done {
		return output, business, replayErr
	}
	err = e.leases.Run(f.Context(), key, func(executionCtx context.Context, token lease.Token) error {
		record, err = e.store.Get(executionCtx, key)
		if err != nil {
			return &OperationError{key, "Get", err}
		}
		if done, replayErr := replay(record); done {
			return replayErr
		}
		if record == nil {
			record, err = e.store.Commit(executionCtx, token, &Record{
				Key: key, Path: path, Kind: ActionKind, Status: Running, Type: typ, Input: data, StartedAt: started,
			}, 0)
			if err != nil {
				return &OperationError{key, "Commit", err}
			}
		} else {
			// Re-executing an interrupted attempt: the outcome must describe the
			// input and type actually used now.
			// 重新执行被中断的尝试：结果必须对应本次实际使用的输入与类型。
			f.replayed(record, typ, data)
			record.Path, record.Type, record.Input = path, typ, data
		}
		recordVersion = record.Version
		baseCtx := context.WithValue(executionCtx, actionKey{}, key)
		var actionErr error
		for attempt := 1; ; attempt++ {
			e.emit(baseCtx, Event{Type: ActionStarted, Key: key, Path: path, Kind: ActionKind, StepType: typ,
				Status: Running, StartedAt: eventStarted, RecordVersion: record.Version,
			}, nil)
			output, actionErr = attemptAction(baseCtx, action, input, option.Timeout)
			if executionCtx.Err() != nil {
				// Drive cancelled or lease lost: never commit, never retry here.
				// Drive 被取消或租约丢失：此处既不提交也不重试。
				return errors.Join(actionErr, executionCtx.Err())
			}
			if actionErr == nil || durableFailure(actionErr) {
				break
			}
			if attempt >= option.Retry.MaxAttempts {
				if option.Retry.MaxAttempts == 0 {
					return actionErr // drive-level retry
				}
				break // exhausted: commit as a business failure
			}
			timer := time.NewTimer(option.Retry.delay(attempt))
			select {
			case <-executionCtx.Done():
				timer.Stop()
				return errors.Join(actionErr, executionCtx.Err())
			case <-timer.C:
			}
		}
		if actionErr != nil {
			record.Status = Failed
			record.Error = actionErr.Error()
		} else {
			var marshalErr error
			record.Result, marshalErr = json.Marshal(output)
			if marshalErr != nil {
				return fatal(marshalErr)
			}
			var normalized O
			if decodeErr := decode(record.Result, &normalized); decodeErr != nil {
				return decodeErr
			}
			output = normalized
			record.Status = Completed
		}
		record.EndedAt = e.options.Now()
		// Renewal remains active during the commit. The storage boundary still
		// atomically validates current generation, expiry and record version.
		// 提交期间续租仍保持活跃。存储边界仍会原子验证当前 generation、过期时间和记录版本。
		committed, commitErr := e.store.Commit(executionCtx, token, record, record.Version)
		if commitErr != nil {
			// Keep the action outcome for diagnosis; the commit may have applied.
			// 保留 action 结果用于诊断；提交可能已经生效。
			return errors.Join(&OperationError{key, "Commit", commitErr}, actionErr)
		}
		actionStatus, recordVersion = record.Status, committed.Version
		if actionErr != nil {
			business = &ActionError{Key: key, Message: actionErr.Error(), Cause: actionErr}
		}
		return nil
	})
	if err != nil {
		var zero O
		return zero, nil, err
	}
	return output, business, nil
}

// attemptAction runs one attempt; a panic or attempt timeout is a retryable error.
// attemptAction 执行一次尝试；panic 或单次超时都是可重试错误。
func attemptAction[I, O any](ctx context.Context, action Action[I, O], input I, timeout time.Duration) (O, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	output, err := invoke(func() (O, error) { return action.Run(ctx, input) })
	if contextErr := ctx.Err(); contextErr != nil {
		// An interrupted attempt is never a durable outcome, even if marked Permanent.
		// 被中断的尝试绝不是持久结果，即使标记为 Permanent。
		return output, &interruptedError{errors.Join(contextErr, err)}
	}
	return output, err
}

type interruptedError struct{ cause error }

func (e *interruptedError) Error() string { return e.cause.Error() }
func (e *interruptedError) Unwrap() error { return e.cause }

// durableFailure reports a Permanent failure that is safe to commit. A panic or
// an interrupted attempt stays retryable even if it wraps a PermanentError.
// durableFailure 判断可安全提交的 Permanent 失败。panic 或被中断的尝试即使
// 包装了 PermanentError，也仍可重试。
func durableFailure(err error) bool {
	var permanent *PermanentError
	var panicErr *PanicError
	var interrupted *interruptedError
	return errors.As(err, &permanent) && !errors.As(err, &panicErr) && !errors.As(err, &interrupted)
}
