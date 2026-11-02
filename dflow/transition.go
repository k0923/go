package dflow

import (
	"fmt"
	"maps"
	"slices"
)

// ApplyArrival validates and folds one predecessor notification. incoming is the
// target node's complete expected predecessor list. Storage adapters call this
// INSIDE their transaction/lock and persist the returned state atomically. The
// ready latch never resets on duplicate delivery.
//
// ApplyArrival 校验并合并一个前驱通知。incoming 是目标节点完整的预期前驱列表。
// 存储适配器会在自己的事务或锁内部调用它，并原子持久化返回的状态。重复投递时
// ready latch 永远不会重置。
func ApplyArrival(target NodeState, source string, incoming []string, active bool) (NodeState, error) {
	if err := validateIDs(incoming); err != nil {
		return target, err
	}
	expected := map[string]bool{}
	for _, id := range incoming {
		expected[id] = true
	}
	if !expected[source] {
		return target, ErrInvalidDefinition
	}
	copy := target
	copy.Incoming = maps.Clone(target.Incoming)
	if copy.Incoming == nil {
		copy.Incoming = map[string]bool{}
	}
	for id := range copy.Incoming {
		if !expected[id] {
			return target, ErrInvalidDefinition
		}
	}
	if previous, exists := copy.Incoming[source]; exists {
		if previous != active {
			return target, ErrConflict
		}
		return copy, nil
	}
	copy.Incoming[source] = active
	copy.Version++
	if len(copy.Incoming) == len(expected) {
		if copy.Status != "" && copy.Status != Pending {
			return target, fmt.Errorf("%w: target already executed", ErrConflict)
		}
		copy.Ready = true
		copy.Enabled = false
		for _, active := range copy.Incoming {
			copy.Enabled = copy.Enabled || active
		}
	}
	if copy.Status == "" {
		copy.Status = Pending
	}
	return copy, nil
}

// ValidateCommit protects the storage boundary against stale/terminal writers.
//
// ValidateCommit 保护存储边界，防止过期 writer 或 terminal writer 写入。
func ValidateCommit(current NodeState, commit NodeCommit, generation int64) error {
	if commit.NodeID == "" ||
		current.Status != Running || current.Version != commit.ExpectedVersion || current.Generation != generation {
		return ErrConflict
	}
	if commit.Status != Succeeded && commit.Status != Skipped && commit.Status != Failed && commit.Status != Suspended {
		return ErrConflict
	}
	if commit.Status != Failed && commit.Error != nil {
		return ErrConflict
	}
	if commit.Status != Succeeded && commit.Status != Skipped && len(commit.Arrivals) > 0 {
		return ErrConflict
	}
	if (commit.Status == Succeeded && !current.Enabled) ||
		(commit.Status == Skipped && current.Enabled) {
		return ErrConflict
	}
	for id, active := range commit.Arrivals {
		if id == "" || id == commit.NodeID {
			return ErrConflict
		}
		if active && commit.Status != Succeeded {
			return ErrConflict
		}
	}
	return nil
}

// ApplyCommitResult returns the persisted node state for a validated commit.
// Storage adapters should call it after ValidateCommit and then persist the
// returned state atomically with arrival updates and lease release.
// nil Input and Output keep the current persisted values; non-nil maps replace
// the corresponding fields completely.
//
// ApplyCommitResult 返回通过校验后的提交应持久化的节点状态。存储适配器应在
// ValidateCommit 之后调用它，并将返回状态与下游到达更新、租约释放一起原子持久化。
// Input 和 Output 为 nil 时保留当前持久化值；非 nil map 会全量替换对应字段。
func ApplyCommitResult(current NodeState, commit NodeCommit) NodeState {
	next := current
	next.Version = current.Version + 1
	next.Status = commit.Status
	if commit.Input != nil {
		next.Input = maps.Clone(commit.Input)
	}
	if commit.Output != nil {
		next.Output = maps.Clone(commit.Output)
	}
	next.Outgoing = activeArrivals(commit.Arrivals)
	next.Error = ""
	if commit.Error != nil {
		next.Error = commit.Error.Error()
	}
	next.Ready = false
	return next
}

func activeArrivals(arrivals map[string]bool) []string {
	outgoing := []string{}
	for id, active := range arrivals {
		if active {
			outgoing = append(outgoing, id)
		}
	}
	slices.Sort(outgoing)
	return outgoing
}
