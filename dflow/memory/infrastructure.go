// Package memory implements run-scoped node scheduling with atomic transitions.
package memory

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/lease"
)

var _ dflow.Infrastructure = (*Infrastructure)(nil)

type Infrastructure struct {
	mu         sync.Mutex
	state      *dflow.State
	generation int64
	token      *lease.Token
	responses  map[[2]string]*dflow.Response
	outbox     map[int64]dflow.Notification
	sequence   int64
}

func New() *Infrastructure { return &Infrastructure{} }

func (i *Infrastructure) valid(token lease.Token) bool {
	return i.token != nil && token.Key == i.token.Key && token.Generation == i.token.Generation && time.Now().Before(i.token.ExpiresAt)
}
func (i *Infrastructure) Claim(ctx context.Context, key string, ttl time.Duration) (*lease.Token, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key != "dflow/run" || ttl <= 0 {
		return nil, lease.ErrInvalid
	}
	if i.token != nil && time.Now().Before(i.token.ExpiresAt) {
		return nil, lease.ErrConflict
	}
	i.generation++
	token := lease.Token{Key: key, Generation: i.generation, ExpiresAt: time.Now().Add(ttl)}
	i.token = &token
	copy := token
	return &copy, nil
}
func (i *Infrastructure) Renew(ctx context.Context, token lease.Token, ttl time.Duration) (*lease.Token, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, lease.ErrInvalid
	}
	if !i.valid(token) {
		return nil, lease.ErrConflict
	}
	i.token.ExpiresAt = time.Now().Add(ttl)
	copy := *i.token
	return &copy, nil
}
func (i *Infrastructure) Release(ctx context.Context, token lease.Token) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.token == nil || token.Key != i.token.Key || token.Generation != i.token.Generation {
		return lease.ErrConflict
	}
	i.token = nil
	return nil
}
func (i *Infrastructure) Initialize(ctx context.Context, token lease.Token, m dflow.Manifest, input map[string]any) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !i.valid(token) {
		return lease.ErrConflict
	}
	if i.state != nil {
		if i.state.DefinitionHash != m.Hash || !reflect.DeepEqual(i.state.Input, input) {
			return dflow.ErrRunMismatch
		}
		return nil
	}
	state := &dflow.State{Version: 1, DefinitionHash: m.Hash, Input: input, Nodes: map[string]dflow.NodeState{}}
	for _, id := range m.Roots {
		state.Nodes[id] = dflow.NodeState{Version: 1, Status: dflow.Pending, Ready: true, Enabled: true}
	}
	copy, err := clone(state)
	if err != nil {
		return err
	}
	i.state = copy
	return nil
}
func (i *Infrastructure) GetInput(ctx context.Context, path ...string) (any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.state == nil {
		return nil, dflow.ErrMissingValue
	}
	value, err := dflow.InputValue(i.state.Input, path...)
	if err != nil {
		return nil, err
	}
	return clone(value)
}
func (i *Infrastructure) Load(ctx context.Context) (*dflow.State, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return clone(i.state)
}
func (i *Infrastructure) GetNode(ctx context.Context, id string) (*dflow.NodeState, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.state == nil {
		return nil, nil
	}
	node, ok := i.state.Nodes[id]
	if !ok {
		return nil, nil
	}
	copy, err := clone(node)
	return &copy, err
}
func (i *Infrastructure) ClaimReady(ctx context.Context, token lease.Token) (*dflow.NodeTask, error) {
	return i.claimNode(ctx, token, "")
}
func (i *Infrastructure) ClaimNode(ctx context.Context, token lease.Token, id string) (*dflow.NodeTask, error) {
	if id == "" {
		return nil, dflow.ErrInvalidDefinition
	}
	return i.claimNode(ctx, token, id)
}
func (i *Infrastructure) claimNode(ctx context.Context, token lease.Token, only string) (*dflow.NodeTask, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !i.valid(token) {
		return nil, lease.ErrConflict
	}
	if i.state == nil {
		return nil, nil
	}
	ids := make([]string, 0, len(i.state.Nodes))
	for id := range i.state.Nodes {
		if only != "" && id != only {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		node := i.state.Nodes[id]
		retry := (node.Status == dflow.Running || node.Status == dflow.Failed) && node.Generation != token.Generation
		if !node.Ready && !retry {
			continue
		}
		if node.Status == dflow.Running && node.Generation == token.Generation {
			continue
		}
		node.Status, node.Ready, node.Generation, node.Error = dflow.Running, false, token.Generation, ""
		node.Version++
		i.state.Nodes[id] = node
		i.state.Version++
		copy, err := clone(node)
		return &dflow.NodeTask{NodeID: id, State: copy}, err
	}
	return nil, nil
}
func (i *Infrastructure) Commit(ctx context.Context, token lease.Token, c dflow.NodeCommit) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !i.valid(token) {
		return lease.ErrConflict
	}
	current := i.state.Nodes[c.NodeID]
	if err := dflow.ValidateCommit(current, c, token.Generation); err != nil {
		return err
	}
	next, err := clone(c.State)
	if err != nil {
		return err
	}
	targets := map[string]dflow.NodeState{}
	for _, arrival := range c.Arrivals {
		target, err := dflow.ApplyArrival(i.state.Nodes[arrival.TargetNodeID], c.NodeID, arrival)
		if err != nil {
			return err
		}
		targets[arrival.TargetNodeID] = target
	}
	var request dflow.Suspension
	newWait := false
	if next.Status == dflow.Suspended {
		if i.responses == nil {
			i.responses = map[[2]string]*dflow.Response{}
		}
		_, exists := i.responses[[2]string{c.NodeID, next.Suspension.Token}]
		newWait = !exists
		request, err = clone(next.Suspension.Payload)
		if err != nil {
			return err
		}
	}
	next.Version = current.Version + 1
	next.Generation = current.Generation
	next.Incoming = current.Incoming
	next.Ready = false
	next.Enabled = current.Enabled
	i.state.Nodes[c.NodeID] = next
	for id, target := range targets {
		i.state.Nodes[id] = target
	}
	i.state.Version++
	if newWait {
		i.responses[[2]string{c.NodeID, next.Suspension.Token}] = nil
		i.notify(dflow.Notification{Kind: dflow.RequestNotification, NodeID: c.NodeID, Token: next.Suspension.Token, Request: request})
	}
	return nil
}
