package tflow

import (
	"fmt"
	"math"
)

// PlanningView contains scheduling facts only. Business configuration and source
// pagination are owned by MainTask and its Store adapter.
type PlanningView interface {
	ConcurrencyLimit() int64
	RunningCount() int64
	PendingCount() int64
	DispatchExhausted() bool
}

// PlanningDecision grants activation and planning budgets, not state changes.
// The engine re-evaluates after activation and CAS-checks every write.
type PlanningDecision struct {
	ActivateLimit int64
	PlanLimit     int64
}

// PlanningPolicy must be immutable, pure and stable across recovery instances.
// It is stored with the main, rather than chosen by each reporting process.
type PlanningPolicy interface {
	Decide(PlanningView) PlanningDecision
}

type PolicyValidator interface{ Validate() error }

type planningView struct{ main MainRecord }

func (v planningView) ConcurrencyLimit() int64 { return v.main.ConcurrencyLimit }
func (v planningView) RunningCount() int64     { return v.main.Subs.Running }
func (v planningView) PendingCount() int64     { return v.main.Subs.Pending }
func (v planningView) DispatchExhausted() bool { return v.main.DispatchExhausted }

// FillPolicy fills every free slot, activating pending units before planning.
type FillPolicy struct{}

func (FillPolicy) Decide(v PlanningView) PlanningDecision {
	free := v.ConcurrencyLimit() - v.RunningCount()
	activate := min(free, v.PendingCount())
	plan := free - activate
	if v.DispatchExhausted() {
		plan = 0
	}
	return PlanningDecision{ActivateLimit: activate, PlanLimit: plan}
}

// WavePolicy starts another wave only when all current executions are terminal.
// Size=0 uses ConcurrencyLimit; a positive Size is capped by that limit.
type WavePolicy struct{ Size int64 }

func (p WavePolicy) Validate() error {
	if p.Size < 0 {
		return ErrInvalid
	}
	return nil
}

func (p WavePolicy) Decide(v PlanningView) PlanningDecision {
	if v.RunningCount() != 0 {
		return PlanningDecision{}
	}
	size := v.ConcurrencyLimit()
	if p.Size > 0 {
		size = min(size, p.Size)
	}
	if v.PendingCount() > 0 {
		return PlanningDecision{ActivateLimit: min(size, v.PendingCount())}
	}
	if v.DispatchExhausted() {
		return PlanningDecision{}
	}
	return PlanningDecision{PlanLimit: size}
}

// BufferPolicy activates pending units first, then replenishes a low cache.
// MinPending=0 preserves demand-only reads: refill only with free slots and an
// empty cache. A positive MinPending enables prefetch even when slots are full.
// MaxPlanSize bounds one source read; zero means no extra bound.
type BufferPolicy struct {
	MinPending    int64
	TargetPending int64
	MaxPlanSize   int64
}

func (p BufferPolicy) Validate() error {
	if p.MinPending < 0 || p.TargetPending < 0 || p.MaxPlanSize < 0 ||
		p.MinPending > p.TargetPending || p.TargetPending > int64(math.MaxInt) ||
		p.MaxPlanSize > int64(math.MaxInt) {
		return ErrInvalid
	}
	return nil
}

func (p BufferPolicy) Decide(v PlanningView) PlanningDecision {
	free := v.ConcurrencyLimit() - v.RunningCount()
	pending := v.PendingCount()
	if free > 0 && pending > 0 {
		return PlanningDecision{ActivateLimit: min(free, v.PendingCount())}
	}
	if v.DispatchExhausted() || (free <= 0 && p.MinPending == 0) {
		return PlanningDecision{}
	}
	if pending > p.MinPending {
		return PlanningDecision{}
	}
	plan := max(int64(0), free) + max(int64(0), p.TargetPending-pending)
	if p.MaxPlanSize > 0 {
		plan = min(plan, p.MaxPlanSize)
	}
	return PlanningDecision{PlanLimit: plan}
}

func validatePolicy(policy PlanningPolicy) error {
	if IsNil(policy) {
		return ErrInvalid
	}
	if validator, ok := policy.(PolicyValidator); ok {
		return validator.Validate()
	}
	return nil
}

func (e *Engine) decidePlanning(m MainRecord) (d PlanningDecision, err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("%w: planning policy panicked: %v", ErrInvalid, value)
		}
	}()
	d = m.PlanningPolicy.Decide(planningView{main: m})
	free := m.ConcurrencyLimit - m.Subs.Running
	if d.ActivateLimit < 0 || d.PlanLimit < 0 || d.ActivateLimit > free ||
		d.ActivateLimit > m.Subs.Pending || d.PlanLimit > int64(math.MaxInt) {
		return PlanningDecision{}, fmt.Errorf("%w: planning policy returned an invalid budget", ErrInvalid)
	}
	if m.DispatchExhausted {
		d.PlanLimit = 0
	}
	return d, nil
}
