package tflow

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

type Options struct {
	// PlanningPolicy is the default scheduling policy for new submissions; each
	// main persists its selected policy. Nil uses FillPolicy.
	// PlanningPolicy 是新提交任务的默认调度策略；每个主任务会持久化所选策略。
	// Nil 表示使用 FillPolicy。
	PlanningPolicy PlanningPolicy

	// MaxReconcilePasses bounds one reconciliation loop. Zero uses 64.
	// MaxReconcilePasses 限制单次收敛循环的最大轮数。零值表示 64。
	MaxReconcilePasses int

	// DispatchLimit bounds automatic command delivery after commits. Zero uses 100.
	// DispatchLimit 限制提交后自动投递 Command 的条数。零值表示 100。
	DispatchLimit int

	// DeliveryLease is the Store lease for one command delivery.
	// Zero uses one minute.
	// DeliveryLease 是单条 Command 投递的租约时间。零值表示一分钟。
	DeliveryLease time.Duration
	// DeliveryTimeout bounds external processing, leaving time to acknowledge
	// before the lease expires. Zero uses 80% of DeliveryLease.
	DeliveryTimeout time.Duration

	// RetryDelay is the base delay after dispatch errors. Zero uses one second.
	// RetryDelay 是投递失败后下一次重试前的等待时间。
	RetryDelay time.Duration
	// RetryPolicy overrides the default capped exponential delay with jitter.
	// Returning zero permits immediate retries, still bounded by the batch budget.
	RetryPolicy RetryPolicy
	Observer    Observer

	// CleanupTimeout bounds the release/ack cleanup after each delivery attempt.
	// Zero uses five seconds.
	// CleanupTimeout 限制每次投递后 release/ack 清理操作的超时时间。零值表示五秒。
	CleanupTimeout time.Duration

	// Clock supplies task snapshot timestamps. Store owns lease and retry time.
	// Clock 为任务快照提供时间，租约和重试时间由 Store 提供。Nil 使用 time.Now。
	Clock Clock

	// Dispatcher receives non-reconcile commands during automatic delivery. Nil
	// disables automatic external dispatch after commits.
	// Dispatcher 在自动投递时接收非收敛类 Command。Nil 表示提交后不自动外部投递。
	Dispatcher Dispatcher

	// Executor enables direct in-process delivery through Handle. Configure either
	// Executor or Dispatcher. For reliable async execution use a persistent runtime.
	Executor Executor

	// MainDecider is persisted as the default for new tasks. A MainTask that
	// implements MainDecider takes precedence. Nil uses DefaultMainDecider.
	MainDecider MainDecider

	// TerminationHandler is the fallback when the historical MainTask has no
	// handler. It is configured for the bound main. Handle calls it outside
	// transactions, at least once; record wakes durably and deduplicate by
	// event.ID across recovery instances. Nil ignores notifications for tasks
	// without their own handler.
	// TerminationHandler 是历史 MainTask 未实现处理器时的默认通知入口。
	// 接收方应按 event.ID 幂等持久保存唤醒，不能依赖原执行对象仍在本进程。
	TerminationHandler TerminationHandler
}

// Engine coordinates MAIN_TASK lifecycle and durable command delivery.
// Engine 协调 MAIN_TASK 生命周期和持久 Command 的投递，不执行业务步骤本身。
type Engine struct {
	store   Store
	options Options
}

// New creates a scheduling engine over a Store.
// New 基于 Store 创建调度引擎。
func New(s Store, opts Options) (*Engine, error) {
	if IsNil(s) || s.MainID() == "" || opts.MaxReconcilePasses < 0 ||
		opts.DispatchLimit < 0 || opts.DeliveryLease < 0 || opts.DeliveryTimeout < 0 || opts.RetryDelay < 0 || opts.CleanupTimeout < 0 {
		return nil, ErrInvalid
	}
	if (!IsNil(opts.Executor) && !IsNil(opts.Dispatcher)) ||
		(opts.Executor != nil && IsNil(opts.Executor)) ||
		(opts.Dispatcher != nil && IsNil(opts.Dispatcher)) ||
		(opts.Clock != nil && IsNil(opts.Clock)) ||
		(opts.TerminationHandler != nil && IsNil(opts.TerminationHandler)) ||
		(opts.RetryPolicy != nil && IsNil(opts.RetryPolicy)) ||
		(opts.Observer != nil && IsNil(opts.Observer)) ||
		(opts.MainDecider != nil && IsNil(opts.MainDecider)) {
		return nil, fmt.Errorf("%w: invalid delivery configuration or typed nil dependency", ErrInvalid)
	}
	if opts.PlanningPolicy == nil {
		opts.PlanningPolicy = FillPolicy{}
	}
	if err := validatePolicy(opts.PlanningPolicy); err != nil {
		return nil, err
	}
	if opts.Clock == nil {
		opts.Clock = ClockFunc(time.Now)
	}
	if opts.MainDecider == nil {
		opts.MainDecider = DefaultMainDecider{}
	}
	if opts.MaxReconcilePasses == 0 {
		opts.MaxReconcilePasses = 64
	}
	if opts.DispatchLimit == 0 {
		opts.DispatchLimit = 100
	}
	if opts.DeliveryLease == 0 {
		opts.DeliveryLease = time.Minute
	}
	if opts.DeliveryTimeout == 0 {
		opts.DeliveryTimeout = opts.DeliveryLease - opts.DeliveryLease/5
	}
	if opts.DeliveryTimeout <= 0 || opts.DeliveryTimeout >= opts.DeliveryLease {
		return nil, fmt.Errorf("%w: delivery timeout must be shorter than lease", ErrInvalid)
	}
	if opts.RetryDelay == 0 {
		opts.RetryDelay = time.Second
	}
	if opts.RetryPolicy == nil {
		opts.RetryPolicy = ExponentialRetry{Base: opts.RetryDelay, Max: max(opts.RetryDelay, time.Minute)}
	}
	if opts.CleanupTimeout == 0 {
		opts.CleanupTimeout = 5 * time.Second
	}
	e := &Engine{store: s, options: opts}
	if !IsNil(opts.Executor) {
		e.options.Dispatcher = &ExecutorDispatcher{engine: e, executor: opts.Executor}
	}
	return e, nil
}

// MainID returns the stable logical task identity bound to the Store.
func (e *Engine) MainID() string { return e.store.MainID() }

type planRequest struct{ limit int }

func (r planRequest) Limit() int { return r.limit }

func (e *Engine) plan(ctx context.Context, m MainRecord, limit int64) (next MainRecord, subs []SubRecord, plan Plan, err error) {
	started := time.Now()
	defer func() {
		if value := recover(); value != nil {
			next, subs, plan, err = MainRecord{}, nil, nil, planPanic(value)
		}
		e.observe(ctx, Observation{Operation: "plan", Duration: time.Since(started), Count: int64(len(subs)), Error: err})
	}()
	if limit <= 0 || limit > int64(math.MaxInt) {
		return MainRecord{}, nil, nil, Permanent(ErrInvalid)
	}
	req := planRequest{limit: int(limit)}
	p, err := m.Task.Plan(ctx, req)
	if err != nil {
		return MainRecord{}, nil, nil, fmt.Errorf("plan main %s: %w", e.MainID(), err)
	}
	next, subs, err = ApplyPlan(m, p, req.Limit(), e.options.Clock.Now())
	if err != nil {
		var detail *PlanError
		if errors.As(err, &detail) {
			detail.MainID = e.MainID()
		}
		err = Permanent(fmt.Errorf("plan main %s: %w", e.MainID(), err))
	}
	return next, subs, p, err
}

func (e *Engine) putStarts(tx Tx, subs []SubRecord) error {
	for _, sub := range subs {
		if sub.Status == SubRunning {
			if err := tx.PutCommand(NewCommand(StartSub, e.MainID(), sub, nil)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Submit creates a MAIN_TASK and commits its initial page and start commands.
// Submit 创建 MAIN_TASK，并原子提交初始页和启动指令。
func (e *Engine) Submit(ctx context.Context, task MainTask, spec SubmitOptions) (MainProgress, error) {
	if spec.PlanningPolicy == nil {
		spec.PlanningPolicy = e.options.PlanningPolicy
	}
	if spec.MainDecider == nil {
		spec.MainDecider = e.options.MainDecider
	}
	m, err := NewMain(task, spec, e.options.Clock.Now())
	if err != nil {
		return MainProgress{}, err
	}
	var subs []SubRecord
	var plan Plan
	decision, err := e.decidePlanning(m)
	if err != nil {
		return MainProgress{}, err
	}
	if decision.PlanLimit > 0 {
		m, subs, plan, err = e.plan(ctx, m, decision.PlanLimit)
	}
	if err != nil {
		return MainProgress{}, err
	}
	err = e.store.WithTx(ctx, func(tx Tx) error {
		var err error
		if plan == nil {
			err = tx.CreateMain(m)
		} else {
			err = tx.CommitPlan(0, m, plan)
		}
		if err != nil {
			return err
		}
		if err := tx.CreateSubs(subs); err != nil {
			return err
		}
		if err := e.putStarts(tx, subs); err != nil {
			return err
		}
		return tx.PutCommand(ReconcileCommand(e.MainID(), m.MainState))
	})
	if err != nil {
		return MainProgress{}, err
	}
	return e.afterCommit(ctx, m.MainProgress)
}

func (e *Engine) afterCommit(ctx context.Context, m MainProgress) (MainProgress, error) {
	err := e.Reconcile(ctx)
	if err == nil && e.options.Dispatcher != nil {
		_, err = e.DispatchPending(ctx, e.options.DispatchLimit, e.options.Dispatcher)
	}
	current, readErr := e.store.GetMain(ctx)
	if readErr == nil {
		m = current.MainProgress
	}
	err = errors.Join(err, readErr)
	if err != nil {
		return m, &CommittedError{MainID: e.MainID(), Cause: err}
	}
	return m, nil
}

// Reconcile is safe to call from reporters, command consumers and recovery.
// It re-evaluates the persisted policy before activation and source planning.
// Reconcile 可由上报者、Command 消费者或恢复扫描重复调用。
// 它按持久策略决定是否激活 Pending 或调用业务 Plan。
func (e *Engine) Reconcile(ctx context.Context) (err error) {
	started := time.Now()
	defer func() { e.observe(ctx, Observation{Operation: "reconcile", Duration: time.Since(started), Error: err}) }()
	return e.reconcile(ctx)
}

func (e *Engine) reconcile(ctx context.Context) error {
	for i := 0; i < e.options.MaxReconcilePasses; i++ {
		e.observe(ctx, Observation{Operation: "reconcile_pass"})
		if err := ctx.Err(); err != nil {
			return err
		}
		m, err := e.store.GetMain(ctx)
		if err != nil {
			return err
		}
		if m.Status.Terminal() {
			return nil
		}
		lifecycle, err := e.decideMain(ctx, m)
		if err != nil {
			return err
		}
		if lifecycle.Action != Continue {
			err := e.commitDecision(ctx, m, lifecycle)
			if errors.Is(err, ErrConflict) {
				e.observe(ctx, Observation{Operation: "conflict", Error: err})
				continue
			}
			return err
		}
		if m.ReadyToComplete() {
			// Business may await durable validation after all units finish.
			return nil
		}
		decision, err := e.decidePlanning(m)
		if err != nil {
			return err
		}
		if decision.ActivateLimit > 0 {
			err := e.activate(ctx, m.Version, decision.ActivateLimit)
			if err != nil && !errors.Is(err, ErrConflict) {
				return err
			}
			if errors.Is(err, ErrConflict) {
				e.observe(ctx, Observation{Operation: "conflict", Error: err})
			}
			continue
		}
		if decision.PlanLimit == 0 {
			return nil
		}
		next, subs, plan, err := e.plan(ctx, m, decision.PlanLimit)
		if err != nil {
			if errors.Is(err, ErrPermanent) && ctx.Err() == nil {
				_, stopErr := e.stop(ctx, MainFailed, err.Error(), m.Version)
				if errors.Is(stopErr, ErrConflict) {
					continue
				}
				return stopErr
			}
			return err
		}
		next.Version = m.Version + 1
		err = e.store.WithTx(ctx, func(tx Tx) error {
			current, err := tx.Main()
			if err != nil {
				return err
			}
			if current.Version != m.Version || current.Status != MainRunning {
				return ErrConflict
			}
			if err := tx.CommitPlan(m.Version, next, plan); err != nil {
				return err
			}
			if err := tx.CreateSubs(subs); err != nil {
				if errors.Is(err, ErrInvalidPlan) {
					return Permanent(err)
				}
				return err
			}
			if err := e.putStarts(tx, subs); err != nil {
				return err
			}
			return tx.PutCommand(ReconcileCommand(e.MainID(), next.MainState))
		})
		if errors.Is(err, ErrConflict) {
			e.observe(ctx, Observation{Operation: "conflict", Error: err})
			continue
		}
		if err != nil {
			if errors.Is(err, ErrPermanent) && ctx.Err() == nil {
				_, stopErr := e.stop(ctx, MainFailed, err.Error(), m.Version)
				if errors.Is(stopErr, ErrConflict) {
					continue
				}
				return stopErr
			}
			return err
		}
	}
	// Bounded retries retain durable reconcile work; they never fail the main.
	return ErrReconcileLimit
}

// UpdateTask commits versioned business facts and a durable wake atomically.
// task must preserve business identity and source progress; stale versions fail
// rather than silently overwriting a concurrent plan or report.
func (e *Engine) UpdateTask(ctx context.Context, expectedVersion int64, task MainTask) (MainProgress, error) {
	if expectedVersion <= 0 || IsNil(task) {
		return MainProgress{}, ErrInvalid
	}
	var progress MainProgress
	err := e.store.WithTx(ctx, func(tx Tx) error {
		if err := tx.SaveTask(expectedVersion, task, e.options.Clock.Now()); err != nil {
			return err
		}
		m, err := tx.Main()
		if err != nil {
			return err
		}
		progress = m.MainProgress
		return tx.PutCommand(ReconcileCommand(e.MainID(), m.MainState))
	})
	if err != nil {
		return MainProgress{}, err
	}
	return e.afterCommit(ctx, progress)
}

func (e *Engine) activate(ctx context.Context, expectedVersion, limit int64) error {
	now := e.options.Clock.Now()
	return e.store.WithTx(ctx, func(tx Tx) error {
		m, err := tx.Main()
		if err != nil {
			return err
		}
		if m.Version != expectedVersion || m.Status != MainRunning {
			return ErrConflict
		}
		available := min(limit, m.ConcurrencyLimit-m.Subs.Running)
		if available <= 0 {
			return nil
		}
		if available > int64(math.MaxInt) {
			return ErrInvalid
		}
		subs, err := tx.FindSubs(SubQuery{Statuses: []SubStatus{SubPending}, Limit: int(available)})
		if err != nil {
			return err
		}
		if len(subs) == 0 {
			return nil
		}
		for i := range subs {
			sub := &subs[i]
			version := sub.Version
			sub.Version++
			sub.Status, sub.UpdatedAt = SubRunning, now
			if err := tx.SaveSub(version, sub.SubState); err != nil {
				return err
			}
		}
		m.Version++
		m.UpdatedAt = now
		if err := tx.SaveMain(expectedVersion, m.MainState); err != nil {
			return err
		}
		if err := e.putStarts(tx, subs); err != nil {
			return err
		}
		return nil
	})
}

func (e *Engine) complete(ctx context.Context, expectedVersion int64, status MainStatus, reason string) error {
	now := e.options.Clock.Now()
	return e.store.WithTx(ctx, func(tx Tx) error {
		current, err := tx.Main()
		if err != nil {
			return err
		}
		if current.Version != expectedVersion || !current.ReadyToComplete() {
			return ErrConflict
		}
		current.Status, current.Version, current.UpdatedAt = status, current.Version+1, now
		current.Error = reason
		if err := tx.SaveMain(expectedVersion, current.MainState); err != nil {
			return err
		}
		return tx.PutCommand(TerminationCommand(e.MainID(), current))
	})
}

func (e *Engine) report(ctx context.Context, report Report) (ReportReceipt, error) {
	for i := 0; i < e.options.MaxReconcilePasses; i++ {
		receipt, err := e.reportOnce(ctx, report)
		if !errors.Is(err, ErrConflict) {
			return receipt, err
		}
		e.observe(ctx, Observation{Operation: "conflict", Error: err})
		if err := ctx.Err(); err != nil {
			return ReportReceipt{}, err
		}
	}
	return ReportReceipt{}, ErrConflict
}

func (e *Engine) reportOnce(ctx context.Context, report Report) (ReportReceipt, error) {
	if report.MainID != e.MainID() || report.SubKey == "" || report.Attempt <= 0 || !report.Status.Terminal() {
		return ReportReceipt{}, ErrInvalid
	}
	var receipt ReportReceipt
	now := e.options.Clock.Now()
	err := e.store.WithTx(ctx, func(tx Tx) error {
		receipt = ReportReceipt{}
		m, err := tx.Main()
		if err != nil {
			return err
		}
		sub, err := tx.Sub(report.SubKey)
		if err != nil {
			return err
		}
		if sub.Attempt != report.Attempt {
			return ErrStaleAttempt
		}
		if sub.Status.Terminal() {
			if sub.Status != report.Status {
				return ErrAlreadyTerminal
			}
			receipt = ReportReceipt{Main: m.MainProgress, Sub: sub.SubState}
			return nil
		}
		if m.Status.Terminal() {
			return ErrAlreadyTerminal
		}
		if sub.Status != SubRunning || m.Subs.Running <= 0 {
			return ErrInvalid
		}
		subVersion, mainVersion := sub.Version, m.Version
		sub.Status, sub.Error = report.Status, report.Error
		sub.Version++
		sub.UpdatedAt = now
		m.Version++
		m.UpdatedAt = now
		if err := tx.SaveSub(subVersion, sub.SubState); err != nil {
			return err
		}
		if err := tx.SaveMain(mainVersion, m.MainState); err != nil {
			return err
		}
		if err := tx.PutCommand(ReconcileCommand(e.MainID(), m.MainState)); err != nil {
			return err
		}
		current, err := tx.Main()
		if err != nil {
			return err
		}
		receipt = ReportReceipt{Main: current.MainProgress, Sub: sub.SubState, Changed: true}
		return nil
	})
	if err != nil {
		return ReportReceipt{}, err
	}
	return receipt, nil
}

// Report commits one terminal SUB_TASK attempt and then reconciles the main.
// Report 提交一个 SUB_TASK 执行代次的终态，并继续推动主任务补位/收敛。
func (e *Engine) Report(ctx context.Context, report Report) (ReportReceipt, error) {
	receipt, err := e.report(ctx, report)
	if err != nil {
		return receipt, err
	}
	receipt.Main, err = e.afterCommit(ctx, receipt.Main)
	return receipt, err
}

func (e *Engine) stop(ctx context.Context, status MainStatus, reason string, expected int64) (StopReceipt, error) {
	var receipt StopReceipt
	now := e.options.Clock.Now()
	err := e.store.WithTx(ctx, func(tx Tx) error {
		receipt = StopReceipt{}
		m, err := tx.Main()
		if err != nil {
			return err
		}
		if expected != 0 && m.Version != expected {
			return ErrConflict
		}
		if m.Status.Terminal() {
			return ErrAlreadyTerminal
		}
		subs, err := tx.FindSubs(SubQuery{Statuses: []SubStatus{SubPending, SubRunning}})
		if err != nil {
			return err
		}
		for i := range subs {
			sub := &subs[i]
			wasRunning, version := sub.Status == SubRunning, sub.Version
			sub.Status, sub.UpdatedAt = SubCanceled, now
			sub.Version++
			if err := tx.SaveSub(version, sub.SubState); err != nil {
				return err
			}
			if wasRunning {
				if err := tx.PutCommand(NewCommand(CancelSub, e.MainID(), *sub, nil)); err != nil {
					return err
				}
			}
		}
		version := m.Version
		m.Status, m.Error, m.UpdatedAt = status, reason, now
		m.Version++
		if err := tx.SaveMain(version, m.MainState); err != nil {
			return err
		}
		current, err := tx.Main()
		if err != nil {
			return err
		}
		if err := tx.PutCommand(TerminationCommand(e.MainID(), current)); err != nil {
			return err
		}
		receipt = StopReceipt{Main: current.MainProgress, SubTasks: subStates(subs)}
		return nil
	})
	if err != nil {
		return StopReceipt{}, err
	}
	return receipt, nil
}

func (e *Engine) stopAndDispatch(ctx context.Context, status MainStatus, reason string) (StopReceipt, error) {
	receipt, err := e.stop(ctx, status, reason, 0)
	if err != nil {
		return receipt, err
	}
	if e.options.Dispatcher != nil {
		_, err = e.DispatchPending(ctx, e.options.DispatchLimit, e.options.Dispatcher)
		if err != nil {
			err = &CommittedError{MainID: e.MainID(), Cause: err}
		}
	}
	return receipt, err
}

// Cancel stops a running MAIN_TASK and emits cancel commands for active subtasks.
// Cancel 取消运行中的 MAIN_TASK，并为活跃子任务生成取消指令。
func (e *Engine) Cancel(ctx context.Context) (StopReceipt, error) {
	return e.stopAndDispatch(ctx, MainCanceled, "")
}

func (e *Engine) Fail(ctx context.Context, reason string) (StopReceipt, error) {
	return e.stopAndDispatch(ctx, MainFailed, reason)
}

// Rerun resets failed/canceled subtasks and returns the main to Running.
// Rerun 重置失败或取消的子任务，并让主任务回到 Running。
func (e *Engine) Rerun(ctx context.Context, scope RerunScope) (RerunReceipt, error) {
	var statuses []SubStatus
	switch scope {
	case RerunFailed:
		statuses = []SubStatus{SubFailed}
	case RerunCanceled:
		statuses = []SubStatus{SubCanceled}
	case RerunFailedCanceled:
		statuses = []SubStatus{SubFailed, SubCanceled}
	default:
		return RerunReceipt{}, ErrInvalid
	}
	return e.rerun(ctx, SubQuery{Statuses: statuses})
}

func (e *Engine) RerunSubTasks(ctx context.Context, keys []string) (RerunReceipt, error) {
	if len(keys) == 0 {
		return RerunReceipt{}, ErrNoSubTaskToRerun
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key == "" || seen[key] {
			return RerunReceipt{}, ErrInvalid
		}
		seen[key] = true
	}
	return e.rerun(ctx, SubQuery{Keys: keys})
}

func (e *Engine) rerun(ctx context.Context, q SubQuery) (RerunReceipt, error) {
	var receipt RerunReceipt
	now := e.options.Clock.Now()
	err := e.store.WithTx(ctx, func(tx Tx) error {
		receipt = RerunReceipt{}
		m, err := tx.Main()
		if err != nil {
			return err
		}
		if m.Status != MainFailed && m.Status != MainCanceled {
			return ErrInvalid
		}
		subs, err := tx.FindSubs(q)
		if err != nil {
			return err
		}
		if len(q.Keys) > 0 && len(subs) != len(q.Keys) {
			return ErrNotFound
		}
		if len(subs) == 0 {
			return ErrNoSubTaskToRerun
		}
		for i := range subs {
			sub := &subs[i]
			if sub.Status != SubFailed && sub.Status != SubCanceled {
				return ErrInvalid
			}
			if sub.Attempt == math.MaxInt64 {
				return ErrInvalid
			}
			version := sub.Version
			sub.Attempt++
			sub.Version++
			sub.Status, sub.Error, sub.ExternalRef = SubPending, "", ""
			sub.UpdatedAt = now
			if err := tx.SaveSub(version, sub.SubState); err != nil {
				return err
			}
		}
		version := m.Version
		m.Status, m.Error, m.UpdatedAt = MainRunning, "", now
		m.Version++
		if err := tx.SaveMain(version, m.MainState); err != nil {
			return err
		}
		if err := tx.PutCommand(ReconcileCommand(e.MainID(), m.MainState)); err != nil {
			return err
		}
		current, err := tx.Main()
		if err != nil {
			return err
		}
		receipt = RerunReceipt{Main: current.MainProgress, SubTasks: subStates(subs)}
		return nil
	})
	if err != nil {
		return receipt, err
	}
	receipt.Main, err = e.afterCommit(ctx, receipt.Main)
	return receipt, err
}

// Resume enqueues a business event for a running SUB_TASK attempt.
// Resume 为正在运行的 SUB_TASK 执行代次登记一个业务恢复事件。
func (e *Engine) Resume(ctx context.Context, ref Execution, event Event) error {
	if ref.MainID != e.MainID() || ref.SubKey == "" || ref.Attempt <= 0 || IsNil(event) || event.Key() == "" {
		return ErrInvalid
	}
	err := e.store.WithTx(ctx, func(tx Tx) error {
		m, err := tx.Main()
		if err != nil {
			return err
		}
		sub, err := tx.Sub(ref.SubKey)
		if err != nil {
			return err
		}
		if sub.Attempt != ref.Attempt {
			return ErrStaleAttempt
		}
		if m.Status.Terminal() || sub.Status.Terminal() {
			return ErrAlreadyTerminal
		}
		if sub.Status != SubRunning {
			return ErrInvalid
		}
		return tx.PutCommand(NewCommand(ResumeSub, e.MainID(), sub, event))
	})
	if err != nil {
		return err
	}
	if e.options.Dispatcher != nil {
		_, err = e.DispatchPending(ctx, e.options.DispatchLimit, e.options.Dispatcher)
		if err != nil {
			return &CommittedError{MainID: ref.MainID, Cause: err}
		}
	}
	return nil
}

func subStates(records []SubRecord) []SubState {
	states := make([]SubState, len(records))
	for i, record := range records {
		states[i] = record.SubState
	}
	return states
}
