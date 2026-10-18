package dflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/k0923/go/lease"
)

const ChildSuspensionType = "dflow.child"

// ChildRequest pins a publication and an explicit return node. It is carried by
// a ChildInvocation payload and delivered after the parent suspension commits.
type ChildRequest struct {
	Publication string         `json:"publication"`
	OutputNode  string         `json:"outputNode"`
	Input       map[string]any `json:"input"`
}

type ChildSuspension struct {
	ChildRequest
}

func (*ChildSuspension) Type() string              { return ChildSuspensionType }
func (n *ChildSuspension) ChildCall() ChildRequest { return n.ChildRequest }

// ChildInvocation is shared by subflow and loop suspension payloads.
type ChildInvocation interface {
	Suspension
	ChildCall() ChildRequest
}

type ChildResponse struct {
	Output any    `json:"output"`
	Error  string `json:"error,omitempty"`
}

// ChildExecution identifies one call, independently of dispatcher attempts.
// Bind MUST return storage scoped by ParentScope+NodeID+Token, and a provider
// pinned to Request.Publication. Never reuse the parent's Infrastructure.
type ChildExecution struct {
	ParentScope string
	NodeID      string
	Token       string
	Request     ChildRequest
}

// ScopeKey deterministically identifies this child invocation across redelivery.
// Includes the business parent scope and framework-generated suspension token.
func (c ChildExecution) ScopeKey() string {
	raw, _ := json.Marshal([]string{c.ParentScope, c.NodeID, c.Token})
	hash := sha256.Sum256(raw)
	return "dflow-child/" + hex.EncodeToString(hash[:])
}

type ChildBinder func(context.Context, ChildExecution) (*Engine, Infrastructure, error)

// ChildDispatcher handles ONE Request. The caller owns outbox polling/Ack and
// resources returned by Bind. Handle returns false while the child is blocked;
// retain the Request and retry after child progress (or a restart). True means
// the parent response is durably resolved or this parent token became stale.
// Transient execution/storage/lease errors return an error: retain and retry.
// A durable failed child node is returned as a ChildResponse business error.
// Nested children use the same dispatcher recursively, with each child's scope.
type ChildDispatcher struct {
	Bind ChildBinder
}

func (d ChildDispatcher) Handle(ctx context.Context, parent *Engine, parentScope string, notification Notification) (bool, error) {
	if parent == nil || parentScope == "" || d.Bind == nil || notification.Kind != RequestNotification {
		return false, fmt.Errorf("dflow: invalid child dispatch")
	}
	payload := notification.Request
	if isNil(payload) {
		return false, ErrInvalidSuspension
	}
	call, ok := payload.(ChildInvocation)
	if !ok {
		return false, fmt.Errorf("dflow: suspension %q is not a child invocation", payload.Type())
	}
	request := call.ChildCall()
	if request.Publication == "" || request.OutputNode == "" {
		return false, fmt.Errorf("dflow: invalid child request")
	}
	current, err := parent.infra.GetNode(ctx, notification.NodeID)
	if err != nil {
		return false, err
	}
	if current == nil || current.Suspension == nil || current.Suspension.Token != notification.Token {
		return true, nil // Late request for an old suspension.
	}
	response, err := parent.infra.GetResponse(ctx, notification.NodeID, notification.Token)
	if err != nil {
		return false, err
	}
	if response != nil {
		return true, nil
	}
	child, store, err := d.Bind(ctx, ChildExecution{
		ParentScope: parentScope, NodeID: notification.NodeID, Token: notification.Token, Request: request,
	})
	if err != nil {
		return false, err
	}
	if child == nil || isNil(store) {
		return false, fmt.Errorf("dflow: child binder must return engine's own infrastructure")
	}
	// Validate the return contract before starting, so a typo cannot leave the
	// parent waiting forever for a node that is absent from the publication.
	returnNode, err := child.definitions.LoadNode(ctx, request.OutputNode)
	if err != nil {
		return false, err
	}
	if returnNode == nil || returnNode.Spec.ID != request.OutputNode {
		return false, ErrInvalidDefinition
	}
	_, driveErr := child.Run(ctx, request.Input)
	// Only an actual durable business node failure is returned to the parent.
	// Infrastructure failures and uncertain commits must remain retryable.
	if driveErr != nil {
		var nodeErr *NodeError
		var infraErr *InfrastructureError
		var leaseErr *lease.Error
		if errors.As(driveErr, &infraErr) || errors.As(driveErr, &leaseErr) ||
			errors.Is(driveErr, context.Canceled) || errors.Is(driveErr, context.DeadlineExceeded) ||
			!errors.As(driveErr, &nodeErr) {
			return false, driveErr
		}
		failed, err := store.GetNode(ctx, nodeErr.NodeID)
		if err != nil {
			return false, err
		}
		if failed == nil || failed.Status != Failed {
			return false, driveErr
		}
		_, err = parent.Wakeup(ctx, notification.NodeID, notification.Token, ChildResponse{Error: failed.Error})
		return err == nil, err
	}
	output, err := store.GetNode(ctx, request.OutputNode)
	if err != nil {
		return false, err
	}
	if output == nil || (output.Status != Succeeded && output.Status != Skipped && output.Status != Failed) {
		return false, nil
	}
	result := ChildResponse{Output: output.Output}
	if output.Status != Succeeded {
		result.Error = fmt.Sprintf("child return node %q is %s: %s", request.OutputNode, output.Status, output.Error)
	}
	_, err = parent.Wakeup(ctx, notification.NodeID, notification.Token, result)
	return err == nil, err
}

func decodeChildResponse(value any) (ChildResponse, error) {
	response, ok := value.(ChildResponse)
	if !ok {
		if pointer, valid := value.(*ChildResponse); valid && pointer != nil {
			response = *pointer
		} else {
			return response, fmt.Errorf("child response must be ChildResponse")
		}
	}
	if response.Error != "" {
		return response, fmt.Errorf("child execution: %s", response.Error)
	}
	return response, nil
}
