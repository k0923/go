// Package childflow composes dflow executions: a parent node suspends with a
// child invocation, and a dispatcher runs the child execution and resolves the
// parent. It is built entirely on dflow's public API and the reference driver,
// demonstrating that subflows and loops are business compositions, not engine
// features. Copy or adapt it freely.
package childflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
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
	dflow.Suspension
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

// ChildBinder returns the child engine, its own run-scoped infrastructure and a
// provider pinned to the requested publication. All three are usually created at
// the call site from one storage handle, so returning them needs no engine
// internals.
type ChildBinder func(context.Context, ChildExecution) (*dflow.Engine, dflow.Infrastructure, dflow.DefinitionProvider, error)

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

// Handle drives one child invocation to resolution. parentInfra is the parent's
// run-scoped infrastructure; parentEngine is used only to resolve the parent
// suspension via Wakeup.
func (d ChildDispatcher) Handle(ctx context.Context, parentEngine *dflow.Engine, parentInfra dflow.Infrastructure, parentScope string, notification dflow.Notification) (bool, error) {
	if parentEngine == nil || parentInfra == nil || parentScope == "" || d.Bind == nil || notification.Kind != dflow.RequestNotification {
		return false, fmt.Errorf("childflow: invalid child dispatch")
	}
	payload := notification.Request
	if payload == nil {
		return false, dflow.ErrInvalidSuspension
	}
	call, ok := payload.(ChildInvocation)
	if !ok {
		return false, fmt.Errorf("childflow: suspension %q is not a child invocation", payload.Type())
	}
	request := call.ChildCall()
	if request.Publication == "" || request.OutputNode == "" {
		return false, fmt.Errorf("childflow: invalid child request")
	}
	current, err := parentInfra.GetNode(ctx, notification.NodeID)
	if err != nil {
		return false, err
	}
	if current == nil || current.Suspension == nil || current.Suspension.Token != notification.Token {
		return true, nil // Late request for an old suspension.
	}
	response, err := parentInfra.GetResponse(ctx, notification.NodeID, notification.Token)
	if err != nil {
		return false, err
	}
	if response != nil {
		return true, nil
	}
	child, store, provider, err := d.Bind(ctx, ChildExecution{
		ParentScope: parentScope, NodeID: notification.NodeID, Token: notification.Token, Request: request,
	})
	if err != nil {
		return false, err
	}
	if child == nil || store == nil || provider == nil {
		return false, fmt.Errorf("childflow: binder must return engine, infrastructure and provider")
	}
	// Validate the return contract before starting, so a typo cannot leave the
	// parent waiting forever for a node that is absent from the publication.
	returnNode, err := provider.LoadNode(ctx, request.OutputNode)
	if err != nil {
		return false, err
	}
	if returnNode == nil || returnNode.Spec.ID != request.OutputNode {
		return false, dflow.ErrInvalidDefinition
	}
	_, driveErr := driver.Run(ctx, child, request.Input)
	// Only an actual durable business node failure is returned to the parent.
	// Infrastructure failures and uncertain commits must remain retryable.
	if driveErr != nil {
		var nodeErr *dflow.NodeError
		var infraErr *dflow.InfrastructureError
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
		if failed == nil || failed.Status != dflow.Failed {
			return false, driveErr
		}
		_, err = parentEngine.Wakeup(ctx, notification.NodeID, notification.Token, ChildResponse{Error: failed.Error})
		return err == nil, err
	}
	output, err := store.GetNode(ctx, request.OutputNode)
	if err != nil {
		return false, err
	}
	if output == nil || (output.Status != dflow.Succeeded && output.Status != dflow.Skipped && output.Status != dflow.Failed) {
		return false, nil
	}
	result := ChildResponse{Output: output.Output}
	if output.Status != dflow.Succeeded {
		result.Error = fmt.Sprintf("child return node %q is %s: %s", request.OutputNode, output.Status, output.Error)
	}
	_, err = parentEngine.Wakeup(ctx, notification.NodeID, notification.Token, result)
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
