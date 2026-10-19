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
	mu        sync.Mutex
	state     *dflow.State
	leases    map[string]time.Time // node ID -> current lease expiry
	responses map[[2]string]*dflow.Response
	outbox    map[int64]dflow.Notification
	sequence  int64
}

func New() *Infrastructure { return &Infrastructure{leases: map[string]time.Time{}} }

// owned reports whether token is the node's live lease. Caller holds mu.
func (i *Infrastructure) owned(token lease.Token) bool {
	if i.state == nil {
		return false
	}
	node, ok := i.state.Nodes[token.Key]
	expires, held := i.leases[token.Key]
	return ok && held && node.Generation == token.Generation && time.Now().Before(expires)
}

// leased reports whether a live lease currently owns the node. Caller holds mu.
func (i *Infrastructure) leased(id string) bool {
	expires, held := i.leases[id]
	return held && time.Now().Before(expires)
}

func drivable(node dflow.NodeState) bool {
	return node.Ready || node.Status == dflow.Running || node.Status == dflow.Failed
}

func (i *Infrastructure) Initialize(ctx context.Context, m dflow.Manifest, input map[string]any) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
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

// ReadyNodes lists drivable node IDs not owned by a live lease: freshly ready
// nodes, nodes left Running by a lost attempt, and Failed nodes a new run may
// retry. The driver stops the current sweep on the first failure (fail-fast);
// retrying is a deliberate new Run/Drain by the business.
func (i *Infrastructure) ReadyNodes(ctx context.Context) ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.state == nil {
		return nil, nil
	}
	var ready []string
	for id, node := range i.state.Nodes {
		if drivable(node) && !i.leased(id) {
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
	if i.state == nil {
		return nil, nil
	}
	node, ok := i.state.Nodes[id]
	if !ok || !drivable(node) || i.leased(id) {
		return nil, nil
	}
	expires := time.Now().Add(ttl)
	node.Status, node.Ready, node.Error = dflow.Running, false, ""
	node.Generation++
	node.Version++
	i.state.Nodes[id] = node
	i.state.Version++
	i.leases[id] = expires
	copy, err := clone(node)
	return &dflow.NodeTask{NodeID: id, State: copy, Lease: lease.Token{Key: id, Generation: node.Generation, ExpiresAt: expires}}, err
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
	if !i.owned(token) {
		return nil, lease.ErrConflict
	}
	token.ExpiresAt = time.Now().Add(ttl)
	i.leases[token.Key] = token.ExpiresAt
	return &token, nil
}
func (i *Infrastructure) ReleaseNode(ctx context.Context, token lease.Token) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.state == nil || i.state.Nodes[token.Key].Generation != token.Generation {
		return lease.ErrConflict
	}
	if _, held := i.leases[token.Key]; !held {
		return lease.ErrConflict
	}
	delete(i.leases, token.Key)
	return nil
}
func (i *Infrastructure) Commit(ctx context.Context, token lease.Token, c dflow.NodeCommit) ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.state == nil || token.Key != c.NodeID {
		return nil, lease.ErrConflict
	}
	current := i.state.Nodes[c.NodeID]
	if current.Generation != token.Generation {
		return nil, lease.ErrConflict // fenced: a newer attempt claimed the node
	}
	if err := dflow.ValidateCommit(current, c, token.Generation); err != nil {
		return nil, err
	}
	if !i.owned(token) {
		return nil, lease.ErrConflict // lease expired or released
	}
	next, err := clone(c.State)
	if err != nil {
		return nil, err
	}
	targets := map[string]dflow.NodeState{}
	var ready []string
	for _, arrival := range c.Arrivals {
		previous := i.state.Nodes[arrival.TargetNodeID]
		target, err := dflow.ApplyArrival(previous, c.NodeID, arrival)
		if err != nil {
			return nil, err
		}
		targets[arrival.TargetNodeID] = target
		if target.Ready && !previous.Ready {
			ready = append(ready, arrival.TargetNodeID)
		}
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
			return nil, err
		}
	}
	next.Version = current.Version + 1
	next.Generation = current.Generation
	next.Incoming = current.Incoming
	next.Ready = false
	next.Enabled = current.Enabled
	i.state.Nodes[c.NodeID] = next
	delete(i.leases, c.NodeID) // the outcome ends this attempt's ownership
	for id, target := range targets {
		i.state.Nodes[id] = target
	}
	i.state.Version++
	if newWait {
		i.responses[[2]string{c.NodeID, next.Suspension.Token}] = nil
		i.notify(dflow.Notification{Kind: dflow.RequestNotification, NodeID: c.NodeID, Token: next.Suspension.Token, Request: request})
	}
	slices.Sort(ready)
	return ready, nil
}
