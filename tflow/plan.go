package tflow

import (
	"fmt"
	"math"
	"time"
)

func NewMain(task MainTask, spec SubmitOptions, now time.Time) (MainRecord, error) {
	if spec.PlanningPolicy == nil {
		spec.PlanningPolicy = FillPolicy{}
	}
	if spec.MainDecider == nil {
		spec.MainDecider = DefaultMainDecider{}
	}
	switch {
	case IsNil(task):
		return MainRecord{}, fmt.Errorf("%w: MainTask is nil", ErrInvalid)
	case IsNil(spec.MainDecider):
		return MainRecord{}, fmt.Errorf("%w: MainDecider is nil", ErrInvalid)
	case spec.ConcurrencyLimit <= 0:
		return MainRecord{}, fmt.Errorf("%w: ConcurrencyLimit must be positive, got %d", ErrInvalid, spec.ConcurrencyLimit)
	}
	if err := validatePolicy(spec.PlanningPolicy); err != nil {
		return MainRecord{}, err
	}
	m := MainRecord{
		Task: task,
		MainConfig: MainConfig{
			ConcurrencyLimit: spec.ConcurrencyLimit,
			PlanningPolicy:   spec.PlanningPolicy,
			MainDecider:      spec.MainDecider,
		},
		MainProgress: MainProgress{MainState: MainState{
			Status:  MainRunning,
			Version: 1, CreatedAt: now, UpdatedAt: now,
		}},
	}
	return m, nil
}

// ApplyPlan captures protocol accessors outside a transaction. Store must CAS
// Main.Version and commit business progress from the Plan with new subtasks.
// ApplyPlan 在事务外读取 Plan 的纯协议方法，生成候选主任务和子任务。
// Store 仍必须用 Main.Version CAS，将业务 Plan 进度与新子任务原子提交。
func ApplyPlan(m MainRecord, plan Plan, limit int, now time.Time) (MainRecord, []SubRecord, error) {
	if IsNil(plan) {
		return MainRecord{}, nil, invalidPlan("plan", -1, ErrInvalidPlan, "must not be nil")
	}
	if m.Status != MainRunning || m.DispatchExhausted || limit <= 0 {
		return MainRecord{}, nil, invalidPlan("request", -1, ErrInvalidPlan, "requires a running unexhausted main and a positive limit")
	}
	tasks := append([]SubTask(nil), plan.Tasks()...)
	hasMore := plan.HasMore()
	if len(tasks) > limit {
		return MainRecord{}, nil, invalidPlan("tasks", -1, ErrInvalidPlan, fmt.Sprintf("returned %d tasks, limit is %d", len(tasks), limit))
	}
	count := int64(len(tasks))
	if count > math.MaxInt64-m.DispatchCursor {
		return MainRecord{}, nil, invalidPlan("tasks", -1, ErrInvalidPlan, "materialized task count overflows int64")
	}
	next := m.DispatchCursor + count
	m.DispatchExhausted = !hasMore
	subs := make([]SubRecord, 0, len(tasks))
	keys := make(map[string]bool, len(tasks))
	slots := m.ConcurrencyLimit - m.Subs.Running
	for i, task := range tasks {
		if IsNil(task) {
			return MainRecord{}, nil, invalidPlan("task", i, ErrInvalidPlan, "must not be nil")
		}
		key := task.Key()
		if key == "" {
			return MainRecord{}, nil, invalidPlan("Key", i, ErrInvalidPlan, "must not be empty")
		}
		if keys[key] {
			return MainRecord{}, nil, invalidPlan("Key", i, ErrInvalidPlan, fmt.Sprintf("duplicate key %q within page", key))
		}
		keys[key] = true
		sub := SubRecord{
			Task: task,
			SubState: SubState{
				Key: key, Cursor: m.DispatchCursor + int64(i),
				Status: SubPending, Attempt: 1, Version: 1,
				CreatedAt: now, UpdatedAt: now,
			},
		}
		if int64(i) < slots {
			sub.Status = SubRunning
			m.Subs.Running++
		} else {
			m.Subs.Pending++
		}
		subs = append(subs, sub)
	}
	m.DispatchCursor = next
	m.UpdatedAt = now
	return m, subs, ValidateMain(m)
}

func ValidateMain(m MainRecord) error {
	if IsNil(m.Task) || ValidateMainState(m.MainConfig, m.MainState) != nil ||
		m.Subs.Running < 0 || m.Subs.Running > m.ConcurrencyLimit ||
		m.Subs.Pending < 0 || m.Subs.Succeeded < 0 || m.Subs.Failed < 0 || m.Subs.Canceled < 0 {
		return ErrInvalid
	}
	remaining := m.DispatchCursor
	for _, n := range []int64{m.Subs.Pending, m.Subs.Running, m.Subs.Succeeded, m.Subs.Failed, m.Subs.Canceled} {
		if n > remaining {
			return ErrInvalid
		}
		remaining -= n
	}
	if remaining != 0 {
		return ErrInvalid
	}
	if m.Status.Terminal() && (m.Subs.Running != 0 || m.Subs.Pending != 0) {
		return ErrInvalid
	}
	if m.Status == MainSucceeded && !m.DispatchExhausted {
		return ErrInvalid
	}
	return nil
}

// ValidateMainState checks durable state independently of derived summaries.
// Adapters validate the complete MainRecord after all subtask writes commit.
func ValidateMainState(config MainConfig, state MainState) error {
	if IsNil(config.PlanningPolicy) || IsNil(config.MainDecider) || config.ConcurrencyLimit <= 0 ||
		state.Version <= 0 || (state.Status != MainRunning && !state.Status.Terminal()) ||
		state.DispatchCursor < 0 {
		return ErrInvalid
	}
	return nil
}

func ValidateSub(s SubRecord) error {
	if IsNil(s.Task) || s.Key == "" || s.Cursor < 0 ||
		s.Attempt <= 0 || s.Version <= 0 || (s.Status != SubPending && s.Status != SubRunning && !s.Status.Terminal()) {
		return ErrInvalid
	}
	return nil
}
