package dflow

import (
	"encoding/json"
	"fmt"
	"slices"
)

// ApplyArrival validates and folds one predecessor notification. Storage adapters
// call this INSIDE their transaction/lock and persist the returned state atomically.
// The ready latch never resets on duplicate delivery.
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
	copy, err := cloneJSON(target)
	if err != nil {
		return target, err
	}
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
		if payload.Payload.Type == "" || !json.Valid(payload.Payload.Data) {
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
