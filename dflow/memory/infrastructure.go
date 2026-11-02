// Package memory implements a run-scoped in-memory dflow Infrastructure.
package memory

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/lease"
)

var _ dflow.Infrastructure = (*Infrastructure)(nil)

type Publication[R any] struct {
	records map[string]dflow.PublishedNode[R]
	order   []string
}

func Publish[R any](records []dflow.PublishedNode[R]) (*Publication[R], error) {
	if len(records) == 0 {
		return nil, dflow.ErrInvalidDefinition
	}
	p := &Publication[R]{records: map[string]dflow.PublishedNode[R]{}, order: make([]string, 0, len(records))}
	for _, record := range records {
		if record == nil || record.GetID() == "" {
			return nil, dflow.ErrInvalidDefinition
		}
		id := record.GetID()
		if _, exists := p.records[id]; exists {
			return nil, dflow.ErrInvalidDefinition
		}
		p.records[id] = record
		p.order = append(p.order, id)
	}
	return p, nil
}

func (p *Publication[R]) LoadNode(ctx context.Context, id string) (dflow.PublishedNode[R], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record, ok := p.records[id]
	if !ok {
		return nil, dflow.ErrInvalidDefinition
	}
	return record, nil
}

func (p *Publication[R]) IDs() []string { return slices.Clone(p.order) }

type Infrastructure struct {
	mu       sync.Mutex
	incoming map[string][]string
	states   map[string]dflow.NodeState
	leases   map[string]lease.Token
}

func New[R any](publication *Publication[R], root string) (*Infrastructure, error) {
	if publication == nil || root == "" {
		return nil, dflow.ErrInvalidDefinition
	}
	i := &Infrastructure{
		incoming: map[string][]string{},
		states:   map[string]dflow.NodeState{},
		leases:   map[string]lease.Token{},
	}
	for _, id := range publication.order {
		record := publication.records[id]
		i.incoming[id] = slices.Clone(record.GetIncoming())
		i.states[id] = dflow.NodeState{Status: dflow.Pending}
	}
	if _, ok := i.states[root]; !ok {
		return nil, dflow.ErrInvalidDefinition
	}
	rootState := i.states[root]
	rootState.Version, rootState.Ready, rootState.Enabled = 1, true, true
	i.states[root] = rootState
	return i, nil
}

func NewRun[R any](records []dflow.PublishedNode[R], root string) (*Infrastructure, *Publication[R], error) {
	publication, err := Publish(records)
	if err != nil {
		return nil, nil, err
	}
	infra, err := New(publication, root)
	if err != nil {
		return nil, nil, err
	}
	return infra, publication, nil
}

func (i *Infrastructure) ReadyNodes(ctx context.Context) ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now()
	var ready []string
	for id, state := range i.states {
		if i.leased(id, now) || state.Status == dflow.Suspended {
			continue
		}
		if state.Ready || state.Status == dflow.Running || state.Status == dflow.Failed {
			ready = append(ready, id)
		}
	}
	slices.Sort(ready)
	return ready, nil
}

func (i *Infrastructure) ClaimNode(ctx context.Context, id string, ttl time.Duration) (*dflow.NodeTask, error) {
	if id == "" {
		return nil, dflow.ErrInvalidDefinition
	}
	if ttl <= 0 {
		return nil, lease.ErrInvalid
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, ok := i.states[id]
	if !ok || i.leased(id, time.Now()) || !claimable(state) {
		return nil, nil
	}
	state.Version++
	state.Generation++
	state.Status = dflow.Running
	state.Ready = false
	state.Error = ""
	token := lease.Token{Key: id, Generation: state.Generation, ExpiresAt: time.Now().Add(ttl)}
	i.states[id], i.leases[id] = state, token
	return &dflow.NodeTask{NodeID: id, State: cloneState(state), Lease: token}, nil
}

func (i *Infrastructure) RenewNode(ctx context.Context, token lease.Token, ttl time.Duration) (*lease.Token, error) {
	if ttl <= 0 {
		return nil, lease.ErrInvalid
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, ok := i.leases[token.Key]
	if !ok || current.Generation != token.Generation || !time.Now().Before(current.ExpiresAt) {
		return nil, lease.ErrConflict
	}
	current.ExpiresAt = time.Now().Add(ttl)
	i.leases[token.Key] = current
	return &current, nil
}

func (i *Infrastructure) ReleaseNode(ctx context.Context, token lease.Token) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, ok := i.leases[token.Key]
	if !ok || current.Generation != token.Generation {
		return lease.ErrConflict
	}
	delete(i.leases, token.Key)
	return nil
}

func (i *Infrastructure) Commit(ctx context.Context, token lease.Token, commit dflow.NodeCommit) ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	currentToken, ok := i.leases[token.Key]
	if !ok || token.Key != commit.NodeID || currentToken.Generation != token.Generation || !time.Now().Before(currentToken.ExpiresAt) {
		return nil, lease.ErrConflict
	}
	current, ok := i.states[commit.NodeID]
	if !ok {
		return nil, lease.ErrConflict
	}
	if err := dflow.ValidateCommit(current, commit, token.Generation); err != nil {
		return nil, err
	}
	updates := map[string]dflow.NodeState{commit.NodeID: dflow.ApplyCommitResult(current, commit)}
	var ready []string
	for targetID, active := range commit.Arrivals {
		before, ok := i.states[targetID]
		if !ok {
			return nil, dflow.ErrInvalidDefinition
		}
		after, err := dflow.ApplyArrival(before, commit.NodeID, i.incoming[targetID], active)
		if err != nil {
			return nil, err
		}
		updates[targetID] = after
		if !before.Ready && after.Ready {
			ready = append(ready, targetID)
		}
	}
	for id, state := range updates {
		i.states[id] = cloneState(state)
	}
	delete(i.leases, token.Key)
	slices.Sort(ready)
	return ready, nil
}

func (i *Infrastructure) Node(ctx context.Context, id string) (dflow.NodeState, bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return dflow.NodeState{}, false, err
	}
	state, ok := i.states[id]
	return cloneState(state), ok, nil
}

func (i *Infrastructure) Snapshot(ctx context.Context) (map[string]dflow.NodeState, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make(map[string]dflow.NodeState, len(i.states))
	for id, state := range i.states {
		result[id] = cloneState(state)
	}
	return result, nil
}

func (i *Infrastructure) Output(ctx context.Context, id string) (map[string]any, error) {
	state, ok, err := i.Node(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok || state.Status != dflow.Succeeded {
		return nil, dflow.ErrInvalidReference
	}
	return maps.Clone(state.Output), nil
}

func claimable(state dflow.NodeState) bool {
	switch state.Status {
	case dflow.Pending:
		return state.Ready
	case dflow.Running, dflow.Failed, dflow.Suspended:
		return true
	default:
		return false
	}
}

func (i *Infrastructure) leased(id string, now time.Time) bool {
	token, ok := i.leases[id]
	return ok && now.Before(token.ExpiresAt)
}

func cloneState(state dflow.NodeState) dflow.NodeState {
	state.Input = maps.Clone(state.Input)
	state.Output = maps.Clone(state.Output)
	state.Incoming = maps.Clone(state.Incoming)
	state.Outgoing = slices.Clone(state.Outgoing)
	return state
}
