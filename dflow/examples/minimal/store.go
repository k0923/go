package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/lease"
)

// store is scoped to one execution. The mutex represents a storage transaction;
// replace it with a database transaction for persistent, cross-process execution.
type store struct {
	mu     sync.Mutex
	nodes  provider
	states map[string]dflow.NodeState
	leases map[string]lease.Token
}

var _ dflow.Infrastructure = (*store)(nil)

func (s *store) ClaimNode(ctx context.Context, id string, ttl time.Duration) (*dflow.NodeTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, lease.ErrInvalid
	}
	current, ok := s.states[id]
	if !ok {
		return nil, fmt.Errorf("unknown node %q", id)
	}
	if time.Now().Before(s.leases[id].ExpiresAt) {
		return nil, nil
	}
	switch current.Status {
	case dflow.Pending:
		if !current.Ready {
			return nil, nil
		}
	case dflow.Running, dflow.Failed, dflow.Suspended:
	default:
		return nil, nil
	}
	current.Version++
	current.Generation++
	current.Status, current.Ready = dflow.Running, false
	token := lease.Token{Key: id, Generation: current.Generation, ExpiresAt: time.Now().Add(ttl)}
	if s.leases == nil {
		s.leases = map[string]lease.Token{}
	}
	s.states[id], s.leases[id] = current, token
	return &dflow.NodeTask{NodeID: id, State: cloneState(current), Lease: token}, nil
}

func (s *store) RenewNode(ctx context.Context, token lease.Token, ttl time.Duration) (*lease.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, lease.ErrInvalid
	}
	current := s.leases[token.Key]
	if token.Generation != current.Generation || !time.Now().Before(current.ExpiresAt) {
		return nil, lease.ErrConflict
	}
	current.ExpiresAt = time.Now().Add(ttl)
	s.leases[token.Key] = current
	return &current, nil
}

func (s *store) ReleaseNode(ctx context.Context, token lease.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, ok := s.leases[token.Key]
	if !ok || token.Generation != current.Generation {
		return lease.ErrConflict
	}
	delete(s.leases, token.Key)
	return nil
}

func (s *store) Commit(ctx context.Context, token lease.Token, commit dflow.NodeCommit) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current := s.leases[token.Key]
	if token.Key != commit.NodeID || token.Generation != current.Generation || !time.Now().Before(current.ExpiresAt) {
		return nil, lease.ErrConflict
	}
	state := s.states[commit.NodeID]
	if err := dflow.ValidateCommit(state, commit, token.Generation); err != nil {
		return nil, err
	}
	// Stage every update first, so an arrival conflict cannot partially commit.
	updates := map[string]dflow.NodeState{commit.NodeID: dflow.ApplyCommitResult(state, commit)}
	var ready []string
	for target, active := range commit.Arrivals {
		record, ok := s.nodes[target]
		if !ok {
			return nil, dflow.ErrInvalidDefinition
		}
		before := s.states[target]
		after, err := dflow.ApplyArrival(before, commit.NodeID, record.GetIncoming(), active)
		if err != nil {
			return nil, err
		}
		updates[target] = after
		if !before.Ready && after.Ready {
			ready = append(ready, target)
		}
	}
	for id, state := range updates {
		s.states[id] = cloneState(state)
	}
	delete(s.leases, token.Key)
	slices.Sort(ready)
	return ready, nil
}

func (s *store) output(ctx context.Context, id string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, ok := s.states[id]
	if !ok || state.Status != dflow.Succeeded {
		return nil, fmt.Errorf("output of %q is unavailable", id)
	}
	return maps.Clone(state.Output), nil
}
