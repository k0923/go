package main

import (
	"encoding/json"
	"fmt"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/sqlite"
	xjson "github.com/k0923/go/json"
)

// Codec owns the database format and reconstructs executable nodes with freshly
// injected dependencies. Plain json.Unmarshal cannot restore interface fields.
type Codec struct{ Service *reviewService }

var _ sqlite.Codec = Codec{}

type ManifestJSON struct {
	Hash  string `json:"hash"`
	Start string `json:"start"`
}

type PublishedNodeJSON struct {
	Spec     NodeSpecJSON `json:"spec"`
	Incoming []string     `json:"incoming"`
	Outgoing []EdgeJSON   `json:"outgoing"`
}

type SuspensionJSON struct {
	Token   string                    `json:"token"`
	Payload xjson.G[dflow.Suspension] `json:"payload"`
}

type NodeStateJSON struct {
	Version    int64           `json:"version"`
	Status     dflow.Status    `json:"status"`
	Input      map[string]any  `json:"input"`
	Output     map[string]any  `json:"output"`
	Ports      []string        `json:"ports"`
	Error      string          `json:"error,omitempty"`
	Suspension *SuspensionJSON `json:"suspension,omitempty"`
	Incoming   map[string]bool `json:"incoming"`
	Ready      bool            `json:"ready"`
	Enabled    bool            `json:"enabled"`
	Generation int64           `json:"generation"`
}

// SQLite stores run metadata separately from individual NodeState records.
type StateJSON struct {
	Version        int64                    `json:"version"`
	DefinitionHash string                   `json:"definitionHash"`
	Input          map[string]any           `json:"input"`
	Nodes          map[string]NodeStateJSON `json:"nodes"`
}

type ResponseJSON struct {
	Value xjson.G[Callback] `json:"value"`
}

func nodeStateJSON(s dflow.NodeState) NodeStateJSON {
	wire := NodeStateJSON{
		Version: s.Version, Status: s.Status, Input: s.Input, Output: s.Output,
		Ports: s.Ports, Error: s.Error, Incoming: s.Incoming,
		Ready: s.Ready, Enabled: s.Enabled, Generation: s.Generation,
	}
	if s.Suspension != nil {
		wire.Suspension = &SuspensionJSON{
			Token: s.Suspension.Token, Payload: xjson.NG[dflow.Suspension](s.Suspension.Payload),
		}
	}
	return wire
}

func (s NodeStateJSON) State() dflow.NodeState {
	state := dflow.NodeState{
		Version: s.Version, Status: s.Status, Input: s.Input, Output: s.Output,
		Ports: s.Ports, Error: s.Error, Incoming: s.Incoming,
		Ready: s.Ready, Enabled: s.Enabled, Generation: s.Generation,
	}
	if s.Suspension != nil {
		state.Suspension = &dflow.SuspensionState{
			Token: s.Suspension.Token, Payload: s.Suspension.Payload.Value(),
		}
	}
	return state
}

func (c Codec) Encode(value any) ([]byte, error) {
	switch v := value.(type) {
	case dflow.Manifest:
		return json.Marshal(ManifestJSON{Hash: v.Hash, Start: v.Start})
	case dflow.PublishedNode:
		spec, err := nodeSpecJSON(v.Spec)
		if err != nil {
			return nil, err
		}
		wire := PublishedNodeJSON{Spec: spec, Incoming: v.Incoming}
		for _, edge := range v.Outgoing {
			wire.Outgoing = append(wire.Outgoing, EdgeJSON{
				Source: edge.SourceNodeID, Target: edge.TargetNodeID, Port: edge.SourcePortID,
			})
		}
		return json.Marshal(wire)
	case dflow.NodeState:
		return json.Marshal(nodeStateJSON(v))
	case dflow.State:
		wire := StateJSON{Version: v.Version, DefinitionHash: v.DefinitionHash, Input: v.Input}
		if v.Nodes != nil {
			wire.Nodes = make(map[string]NodeStateJSON, len(v.Nodes))
			for id, node := range v.Nodes {
				wire.Nodes[id] = nodeStateJSON(node)
			}
		}
		return json.Marshal(wire)
	case dflow.Suspension:
		return json.Marshal(xjson.NG[dflow.Suspension](v))
	case dflow.Response:
		var callback Callback
		if v.Value != nil {
			var ok bool
			callback, ok = v.Value.(Callback)
			if !ok {
				return nil, fmt.Errorf("unsupported callback %T", v.Value)
			}
		}
		return json.Marshal(ResponseJSON{Value: xjson.NG[Callback](callback)})
	case []string: // SQLite's predecessor index.
		return json.Marshal(v)
	default:
		return nil, fmt.Errorf("unsupported storage value %T", value)
	}
}

func (c Codec) Decode(raw []byte, target any) error {
	switch v := target.(type) {
	case *dflow.Manifest:
		var wire ManifestJSON
		if err := json.Unmarshal(raw, &wire); err != nil {
			return err
		}
		*v = dflow.Manifest{Hash: wire.Hash, Start: wire.Start}
	case *dflow.PublishedNode:
		var wire PublishedNodeJSON
		if err := json.Unmarshal(raw, &wire); err != nil {
			return err
		}
		spec, err := wire.Spec.Build(c.Service)
		if err != nil {
			return err
		}
		*v = dflow.PublishedNode{Spec: spec, Incoming: wire.Incoming}
		for _, edge := range wire.Outgoing {
			v.Outgoing = append(v.Outgoing, edge.Edge())
		}
	case *dflow.NodeState:
		var wire NodeStateJSON
		if err := json.Unmarshal(raw, &wire); err != nil {
			return err
		}
		*v = wire.State()
	case *dflow.State:
		var wire StateJSON
		if err := json.Unmarshal(raw, &wire); err != nil {
			return err
		}
		*v = dflow.State{Version: wire.Version, DefinitionHash: wire.DefinitionHash, Input: wire.Input}
		if wire.Nodes != nil {
			v.Nodes = make(map[string]dflow.NodeState, len(wire.Nodes))
			for id, node := range wire.Nodes {
				v.Nodes[id] = node.State()
			}
		}
	case *dflow.Suspension:
		var wire xjson.G[dflow.Suspension]
		if err := json.Unmarshal(raw, &wire); err != nil {
			return err
		}
		*v = wire.Value()
	case *dflow.Response:
		var wire ResponseJSON
		if err := json.Unmarshal(raw, &wire); err != nil {
			return err
		}
		*v = dflow.Response{Value: wire.Value.Value()}
	case *[]string:
		return json.Unmarshal(raw, v)
	default:
		return fmt.Errorf("unsupported storage target %T", target)
	}
	return nil
}
