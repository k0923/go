package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

type workflowRuntime[Input any, Output any] struct {
	runID        string
	ownerID      string
	initialLease ExecutionLease
	// events 是本地回放缓存。FindEvent 会消费匹配到的事件，避免同一次
	// 进程内回放重复命中同一个事件。
	events    []Event
	state     *RunState[Input, Output]
	lock      sync.Mutex
	store     WorkflowStore[Input, Output]
	consumed  map[int]bool
	stepNames map[string]struct{}
	// activeLeases 记录当前 runtime 持有的所有 continuation 租约。心跳只续租
	// 这个集合里的租约，store 侧通过 generation fencing 拒绝过期副本。
	activeLeases  map[string]ExecutionLease
	leaseDuration time.Duration
	// targeted 表示本次执行是从某条分支恢复的，只应沿 initialLease 声明的
	// 分支路径继续；全新运行则从根 continuation 开始。
	targeted        bool
	targetBranchIDs []string
}

// newRuntime 创建一个全新的工作流运行，并从根 continuation 开始执行。
func newRuntime[Input any, Output any](
	runID string,
	input Input,
	store WorkflowStore[Input, Output],
	leaseDuration time.Duration,
) (*workflowRuntime[Input, Output], error) {
	ownerID := newRuntimeID()
	leaseDuration = normalizeLeaseDuration(leaseDuration)
	state, lease, err := store.CreateRun(
		runID,
		input,
		ownerID,
		leaseDuration,
	)
	if err != nil {
		return nil, err
	}
	runtime := &workflowRuntime[Input, Output]{
		runID:         runID,
		ownerID:       ownerID,
		initialLease:  lease,
		state:         state,
		store:         store,
		consumed:      make(map[int]bool),
		stepNames:     make(map[string]struct{}),
		activeLeases:  make(map[string]ExecutionLease),
		leaseDuration: leaseDuration,
	}
	runtime.trackLease(lease)
	return runtime, nil
}

// resumeRuntime 基于持久化状态和调用方已抢占到的 continuation 租约，重建
// 一个进程内 runtime。
func resumeRuntime[Input any, Output any](
	runID string,
	store WorkflowStore[Input, Output],
	lease ExecutionLease,
	leaseDuration time.Duration,
) (*workflowRuntime[Input, Output], error) {
	leaseDuration = normalizeLeaseDuration(leaseDuration)
	state, err := store.GetRunState(runID)
	if err != nil {
		return nil, err
	}
	if state == nil || state.Status == RunStatusFinished {
		return nil, ErrRunStateNotFound
	}
	events, err := store.GetEvents(runID)
	if err != nil {
		return nil, err
	}
	runtime := &workflowRuntime[Input, Output]{
		runID:           runID,
		ownerID:         lease.OwnerID,
		initialLease:    lease,
		events:          events,
		state:           state,
		store:           store,
		consumed:        make(map[int]bool),
		stepNames:       make(map[string]struct{}),
		activeLeases:    make(map[string]ExecutionLease),
		leaseDuration:   leaseDuration,
		targeted:        true,
		targetBranchIDs: append([]string(nil), lease.Key.BranchIDs...),
	}
	runtime.trackLease(lease)
	return runtime, nil
}

// ClaimStepName 为当前执行声明一个步骤名，避免同一运行中跨分支重复使用。
func (runtime *workflowRuntime[Input, Output]) ClaimStepName(
	name string,
	branchIDs []string,
) error {
	runtime.lock.Lock()
	defer runtime.lock.Unlock()
	if runtime.stepNames == nil {
		runtime.stepNames = make(map[string]struct{})
	}
	if _, exists := runtime.stepNames[name]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateStepName, name)
	}
	for _, event := range runtime.events {
		if !isStepEvent(event) ||
			event.GetBase().Name != name ||
			slices.Equal(event.GetBase().BranchIds, branchIDs) {
			continue
		}
		return fmt.Errorf("%w: %s", ErrDuplicateStepName, name)
	}
	runtime.stepNames[name] = struct{}{}
	return nil
}

func isStepEvent(event Event) bool {
	switch event.(type) {
	case *StepStartEvent, *StepFinishEvent, *StepFailedEvent:
		return true
	default:
		return false
	}
}

// FindEvent 在回放缓存中查找下一个尚未消费、且类型为 T 的事件。工作流代码
// 通过它复用已持久化的副作用结果，保证重放时不会再次执行同一个操作。
func (runtime *workflowRuntime[Input, Output]) FindEvent[T Event](
	predicate func(event T, index int) bool,
) (T, int) {
	runtime.lock.Lock()
	defer runtime.lock.Unlock()
	var defaultValue T
	for i := 0; i < len(runtime.events); i++ {
		if runtime.consumed[i] {
			continue
		}
		event := runtime.events[i]
		if evt, ok := event.(T); ok && predicate(evt, i) {
			runtime.consumed[i] = true
			return evt, i
		}
	}
	return defaultValue, -1
}

// AppendEvent 先持久化事件，再同步写入本地回放缓存，让同一次执行中的后续
// 操作可以立即观察到它。
func (runtime *workflowRuntime[Input, Output]) AppendEvent(
	lease ExecutionLease,
	event Event,
) error {
	if err := runtime.store.AppendEvent(lease, event); err != nil {
		return err
	}
	runtime.lock.Lock()
	defer runtime.lock.Unlock()
	runtime.saveLocalEvent(event)
	return nil
}

// SuspendContinuation 记录当前 continuation 暂停的原因，并将对应租约移出
// 心跳续租集合。
func (runtime *workflowRuntime[Input, Output]) SuspendContinuation(
	lease ExecutionLease,
	event Event,
	suspension Suspension,
) error {
	if err := runtime.store.SuspendContinuation(lease, event, suspension); err != nil {
		return err
	}
	runtime.untrackLease(lease)
	if event != nil {
		runtime.lock.Lock()
		defer runtime.lock.Unlock()
		runtime.saveLocalEvent(event)
	}
	return nil
}

// ForkParallel 原子地将父 continuation 替换为子分支租约。完成 fork 后，
// 只有返回的子分支租约需要继续续租。
func (runtime *workflowRuntime[Input, Output]) ForkParallel(
	lease ExecutionLease,
	branchGroupID string,
	started *ParallelStartedEvent,
	branchIDs []string,
) (ForkParallelResult, error) {
	result, err := runtime.store.ForkParallel(
		lease,
		branchGroupID,
		started,
		branchIDs,
		runtime.leaseDuration,
	)
	if err != nil {
		return ForkParallelResult{}, err
	}
	if len(result.BranchLeases) > 0 {
		runtime.untrackLease(lease)
		for _, branchLease := range result.BranchLeases {
			runtime.trackLease(branchLease)
		}
	}
	runtime.lock.Lock()
	defer runtime.lock.Unlock()
	runtime.saveLocalEvent(started)
	return result, nil
}

// CompleteContinuation 完成一个分支；如果它是分支组里的最后一个分支，则
// 同时抢占父 continuation，让执行可以回到上层继续。
func (runtime *workflowRuntime[Input, Output]) CompleteContinuation(
	lease ExecutionLease,
	branchGroupID string,
	branchCompleted *BranchCompletedEvent,
	parallelCompleted *ParallelCompletedEvent,
) (CompleteContinuationResult, error) {
	result, err := runtime.store.CompleteContinuation(
		lease,
		branchGroupID,
		branchCompleted,
		parallelCompleted,
		runtime.leaseDuration,
	)
	if err != nil {
		return CompleteContinuationResult{}, err
	}
	runtime.untrackLease(lease)
	if result.ParentLease != nil {
		runtime.trackLease(*result.ParentLease)
	}
	runtime.lock.Lock()
	defer runtime.lock.Unlock()
	runtime.saveLocalEvent(branchCompleted)
	if result.ParentLease != nil {
		runtime.saveLocalEvent(parallelCompleted)
	}
	return result, nil
}

// ReleaseContinuation 释放当前持有的 continuation，并停止对它做心跳续租。
func (runtime *workflowRuntime[Input, Output]) ReleaseContinuation(
	lease ExecutionLease,
) error {
	if err := runtime.store.ReleaseContinuation(lease); err != nil {
		return err
	}
	runtime.untrackLease(lease)
	return nil
}

// CompleteRun 将根 continuation 标记为完成，并结束对根租约的续租。
func (runtime *workflowRuntime[Input, Output]) CompleteRun(
	lease ExecutionLease,
	output Output,
) error {
	if err := runtime.store.CompleteRun(lease, output); err != nil {
		return err
	}
	runtime.untrackLease(lease)
	return nil
}

// TargetsChild 判断恢复执行时，当前父路径是否应该继续进入指定子分支。
func (runtime *workflowRuntime[Input, Output]) TargetsChild(
	parentBranchIDs []string,
	branchID string,
) bool {
	if !runtime.targeted ||
		len(runtime.targetBranchIDs) <= len(parentBranchIDs) ||
		!slices.Equal(
			runtime.targetBranchIDs[:len(parentBranchIDs)],
			parentBranchIDs,
		) {
		return false
	}
	return runtime.targetBranchIDs[len(parentBranchIDs)] == branchID
}

// InitialLeaseTargets 判断某个子分支是否正是本次恢复执行抢占到的分支。
func (runtime *workflowRuntime[Input, Output]) InitialLeaseTargets(
	parentBranchIDs []string,
	branchID string,
) bool {
	childBranchIDs := append(append([]string(nil), parentBranchIDs...), branchID)
	return slices.Equal(runtime.initialLease.Key.BranchIDs, childBranchIDs)
}

// trackLease 将租约加入心跳集合。同一路径只保留最新写入的一份租约。
func (runtime *workflowRuntime[Input, Output]) trackLease(lease ExecutionLease) {
	runtime.lock.Lock()
	defer runtime.lock.Unlock()
	if runtime.activeLeases == nil {
		runtime.activeLeases = make(map[string]ExecutionLease)
	}
	runtime.activeLeases[executionLeaseKey(lease.Key)] = lease
}

// untrackLease 只移除 generation 匹配的租约，避免误删同一路径上更新后的租约。
func (runtime *workflowRuntime[Input, Output]) untrackLease(lease ExecutionLease) {
	runtime.lock.Lock()
	defer runtime.lock.Unlock()
	key := executionLeaseKey(lease.Key)
	tracked, exists := runtime.activeLeases[key]
	if exists && tracked.Generation == lease.Generation {
		delete(runtime.activeLeases, key)
	}
}

// startLeaseHeartbeat 在用户代码执行期间持续续租当前 runtime 持有的租约。
// 返回的函数会停止心跳 goroutine，并等待它退出。
func (runtime *workflowRuntime[Input, Output]) startLeaseHeartbeat() func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	interval := runtime.leaseDuration / 3
	if interval <= 0 {
		interval = time.Nanosecond
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				runtime.renewActiveLeases()
			case <-stop:
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// renewActiveLeases 先快照 activeLeases，再调用 store 续租，避免在 I/O 期间
// 长时间持有 runtime 锁。
func (runtime *workflowRuntime[Input, Output]) renewActiveLeases() {
	runtime.lock.Lock()
	leases := make([]ExecutionLease, 0, len(runtime.activeLeases))
	for _, lease := range runtime.activeLeases {
		leases = append(leases, lease)
	}
	runtime.lock.Unlock()

	for _, lease := range leases {
		renewed, err := runtime.store.RenewLease(lease, runtime.leaseDuration)
		if err != nil {
			continue
		}
		runtime.lock.Lock()
		key := executionLeaseKey(lease.Key)
		tracked, exists := runtime.activeLeases[key]
		if exists && tracked.Generation == lease.Generation {
			runtime.activeLeases[key] = renewed
		}
		runtime.lock.Unlock()
	}
}

// executionLeaseKey 故意不包含 generation：同一个 continuation 路径在当前
// runtime 中最多只跟踪一个租约，替换或移除时再校验 generation。
func executionLeaseKey(key ContinuationKey) string {
	branchIDs, _ := json.Marshal(key.BranchIDs)
	return key.RunID + ":" + string(branchIDs)
}

// saveLocalEvent 按工作流事件身份做 upsert，因为重试或分支 join 时 store
// 也可能覆盖同一身份的事件。
func (runtime *workflowRuntime[Input, Output]) saveLocalEvent(event Event) {
	for index, storedEvent := range runtime.events {
		if sameEventIdentity(storedEvent, event) {
			runtime.events[index] = event
			return
		}
	}
	runtime.events = append(runtime.events, event)
}

// executeWorkflow 统一处理 runtime 层面的清理契约：框架控制流 panic 表示暂停，
// 普通 panic 会释放租约后继续抛出，返回 error 会释放租约，根流程成功返回则完成运行。
func executeWorkflow[Input any, Output any](
	runtime *workflowRuntime[Input, Output],
	handler func(ctx *WorkflowContext[Input, Output]) (Output, error),
) (err error) {
	stopHeartbeat := runtime.startLeaseHeartbeat()
	lease := runtime.initialLease
	var rootLease *ExecutionLease
	if len(lease.Key.BranchIDs) == 0 {
		rootLease = &lease
	}
	ctx := &WorkflowContext[Input, Output]{
		Context: context.Background(),
		input:   runtime.state.Input,
		runtime: runtime,
		lease:   rootLease,
	}

	defer func() {
		stopHeartbeat()
		recovered := classifyPanic(recover())
		if recovered.value != nil && recovered.signal == nil {
			if ctx.lease != nil {
				_ = runtime.ReleaseContinuation(*ctx.lease)
				ctx.lease = nil
			}
			panic(recovered.value)
		}
		if recovered.signal != nil {
			return
		}
		if err != nil {
			if ctx.lease != nil {
				err = errors.Join(err, runtime.ReleaseContinuation(*ctx.lease))
				ctx.lease = nil
			}
			return
		}
		if ctx.lease == nil {
			return
		}
		err = runtime.CompleteRun(*ctx.lease, ctx.output)
		ctx.lease = nil
	}()

	ctx.output, err = handler(ctx)
	return err
}

type RunStatus string

const (
	RunStatusRunning  RunStatus = "running"
	RunStatusPaused   RunStatus = "paused"
	RunStatusFinished RunStatus = "finished"
	RunStatusErrored  RunStatus = "errored"
	RunStatusAborted  RunStatus = "aborted"
)
