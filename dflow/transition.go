package dflow

import (
	"fmt"
	"maps"
	"slices"
)

// ApplyArrival validates and folds one predecessor notification. Storage adapters
// call this INSIDE their transaction/lock and persist the returned state
// atomically. The ready latch never resets on duplicate delivery.
//
// ApplyArrival 校验并合并一个前驱通知。存储适配器会在自己的事务或锁内部调用它，
// 并原子持久化返回的状态。重复投递时 ready latch 永远不会重置。
func ApplyArrival(target NodeState, source string, arrival Arrival) (NodeState, error) {
	expected := map[string]bool{}
	for _, id := range arrival.Incoming {
		if id == "" || expected[id] {
			return target, ErrInvalidDefinition
		}
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
	if active, exists := copy.Incoming[source]; exists {
		if active != arrival.Active {
			return target, ErrConflict
		}
		return copy, nil
	}
	copy.Incoming[source] = arrival.Active
	copy.Version++
	if len(copy.Incoming) == len(expected) {
		if copy.Status != "" && copy.Status != Pending {
			return target, fmt.Errorf("%w: target already executed", ErrConflict)
		}
		copy.Ready = true
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
	if current.Status != Running || current.Version != commit.ExpectedVersion || current.Generation != generation {
		return ErrConflict
	}
	if commit.State.Status != Succeeded && commit.State.Status != Skipped && commit.State.Status != Failed && commit.State.Status != Suspended {
		return ErrConflict
	}
	if commit.State.Status == Suspended && (commit.State.Suspension == nil || commit.State.Suspension.Token == "") {
		return ErrInvalidSuspension
	}
	if payload := commit.State.Suspension; payload != nil {
		if isNil(payload.Payload) || payload.Payload.Type() == "" {
			return ErrInvalidSuspension
		}
	}
	if commit.State.Status != Succeeded && commit.State.Status != Skipped && len(commit.Arrivals) > 0 {
		return ErrConflict
	}
	if (commit.State.Status == Succeeded || commit.State.Status == Skipped) && commit.State.Suspension != nil {
		return ErrConflict
	}
	ids := []string{}
	for _, arrival := range commit.Arrivals {
		if arrival.TargetNodeID == "" || arrival.TargetNodeID == commit.NodeID || slices.Contains(ids, arrival.TargetNodeID) {
			return ErrConflict
		}
		ids = append(ids, arrival.TargetNodeID)
	}
	return nil
}
