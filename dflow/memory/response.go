package memory

import (
	"context"
	"slices"

	"github.com/k0923/go/dflow"
)

var _ dflow.Outbox = (*Infrastructure)(nil)

func (i *Infrastructure) GetResponse(ctx context.Context, nodeID, token string) (*dflow.Response, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	response, exists := i.responses[[2]string{nodeID, token}]
	if !exists {
		return nil, dflow.ErrWaitNotFound
	}
	if response == nil {
		return nil, nil
	}
	return clone(response)
}

func (i *Infrastructure) Resolve(ctx context.Context, nodeID, token string, value any) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	key := [2]string{nodeID, token}
	response, exists := i.responses[key]
	if !exists {
		return false, dflow.ErrWaitNotFound
	}
	if response != nil {
		return false, nil
	}
	node := i.state.Nodes[nodeID]
	if node.Suspension == nil || node.Suspension.Token != token {
		return false, nil
	}
	copy, err := clone(value)
	if err != nil {
		return false, err
	}
	i.responses[key] = &dflow.Response{Value: copy}
	node.Ready = true
	node.Version++
	i.state.Nodes[nodeID] = node
	i.notify(dflow.Notification{Kind: dflow.WakeNotification, NodeID: nodeID, Token: token})
	return true, nil
}

func (i *Infrastructure) notify(notification dflow.Notification) {
	if i.outbox == nil {
		i.outbox = make(map[int64]dflow.Notification)
	}
	i.sequence++
	notification.ID = i.sequence
	i.outbox[i.sequence] = notification
}

func (i *Infrastructure) Pending(ctx context.Context) ([]dflow.Notification, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]dflow.Notification, 0, len(i.outbox))
	for _, item := range i.outbox {
		var err error
		item.Request, err = clone(item.Request)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	slices.SortFunc(result, func(a, b dflow.Notification) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return result, nil
}

func (i *Infrastructure) Ack(ctx context.Context, id int64) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(i.outbox, id)
	return nil
}
