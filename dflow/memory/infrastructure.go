// Package memory provides an in-process, run-scoped dflow Infrastructure.
package memory

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/lease"
)

var _ dflow.Infrastructure = (*Infrastructure)(nil)

// Infrastructure's zero value is ready to use. Create one instance per run.
type Infrastructure struct {
	mu         sync.Mutex
	state      []byte
	version    int64
	generation int64
	token      *lease.Token
	responses  map[[2]string]*dflow.Response
	outbox     map[int64]dflow.Notification
	sequence   int64
}

func New() *Infrastructure { return &Infrastructure{} }

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

func (i *Infrastructure) valid(token lease.Token) bool {
	return i.token != nil && token.Key == i.token.Key &&
		token.Generation == i.token.Generation && time.Now().Before(i.token.ExpiresAt)
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

func (i *Infrastructure) Load(ctx context.Context) (*dflow.State, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.state == nil {
		return nil, nil
	}
	var state dflow.State
	if err := json.Unmarshal(i.state, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (i *Infrastructure) Save(ctx context.Context, token lease.Token, state *dflow.State, expectedVersion int64) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !i.valid(token) {
		return lease.ErrConflict
	}
	if state == nil || expectedVersion != i.version || state.Version != expectedVersion {
		return dflow.ErrConflict
	}
	copy := *state
	copy.Version = expectedVersion + 1
	data, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	// Prepare all notification payloads before making any mutations.
	var requests []dflow.Notification
	for id, node := range state.Nodes {
		if node.Suspension == nil {
			continue
		}
		if node.Suspension.Token == "" {
			return dflow.ErrInvalidSuspension
		}
		key := [2]string{id, node.Suspension.Token}
		if _, exists := i.responses[key]; exists {
			continue
		}
		if node.Status != dflow.Suspended {
			return dflow.ErrInvalidSuspension
		}
		request, err := json.Marshal(node.Suspension.Request)
		if err != nil {
			return err
		}
		requests = append(requests, dflow.Notification{
			Kind: dflow.RequestNotification, NodeID: id, Token: node.Suspension.Token, Request: request,
		})
	}
	if i.responses == nil {
		i.responses = make(map[[2]string]*dflow.Response)
	}
	for _, request := range requests {
		i.responses[[2]string{request.NodeID, request.Token}] = nil
		i.notify(request)
	}
	i.state, i.version = data, copy.Version
	return nil
}
