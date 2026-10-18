package tests

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/k0923/go/dflow"
)

// These helpers adapt old fixture descriptions at the application boundary.
// No registration or encoding is used by the engine.
type testRegistry map[string]dflow.Node
type namedNode string

func (n namedNode) Run(context.Context, map[string]any) (dflow.Result, error) {
	return dflow.Result{}, fmt.Errorf("unbound fixture node %q", n)
}

func configuredFixture(kind string, raw json.RawMessage) dflow.Node {
	var node dflow.Node
	switch kind {
	case "http":
		node = &dflow.HTTPNode{}
	case "loop":
		node = &dflow.LoopNode{}
	case "subflow":
		node = &dflow.SubflowNode{}
	case "code":
		var config dflow.CodeConfig
		if err := json.Unmarshal(raw, &config); err != nil {
			panic(err)
		}
		code, err := dflow.NewCodeNode(config)
		if err != nil {
			panic(err)
		}
		return code
	}
	if err := json.Unmarshal(raw, node); err != nil {
		panic(err)
	}
	return node
}

func fixtureDefinition(def dflow.Definition, nodes testRegistry) dflow.Definition {
	if def.Version == "" {
		def.Version = "fixture-v1"
	}
	def.Nodes = append([]dflow.NodeSpec(nil), def.Nodes...)
	for index := range def.Nodes {
		if name, ok := def.Nodes[index].Node.(namedNode); ok {
			def.Nodes[index].Node = nodes[string(name)]
		}
	}
	return def
}

var fixtureExecutables sync.Map
var fixtureSequence atomic.Uint64

// fixtureCodec stores application values using gob. Published fixture handlers
// are rebound by the test application; they are not serializable Go functions.
type fixtureCodec struct{}

type fixturePublication struct {
	Node dflow.PublishedNode
	Key  string
}

func (fixtureCodec) Encode(value any) ([]byte, error) {
	if node, ok := value.(dflow.PublishedNode); ok {
		key := fmt.Sprint(fixtureSequence.Add(1))
		fixtureExecutables.Store(key, node.Spec.Node)
		node.Spec.Node = nil
		value = fixturePublication{Node: node, Key: key}
	}
	var buffer bytes.Buffer
	if payload, ok := value.(dflow.Suspension); ok {
		err := gob.NewEncoder(&buffer).Encode(&payload)
		return buffer.Bytes(), err
	}
	err := gob.NewEncoder(&buffer).Encode(value)
	return buffer.Bytes(), err
}

func (fixtureCodec) Decode(raw []byte, target any) error {
	if node, ok := target.(*dflow.PublishedNode); ok {
		var publication fixturePublication
		if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&publication); err != nil {
			return err
		}
		value, exists := fixtureExecutables.Load(publication.Key)
		if !exists {
			return fmt.Errorf("application must rebind fixture executable")
		}
		*node = publication.Node
		node.Spec.Node = value.(dflow.Node)
		return nil
	}
	if payload, ok := target.(*dflow.Suspension); ok {
		return gob.NewDecoder(bytes.NewReader(raw)).Decode(payload)
	}
	return gob.NewDecoder(bytes.NewReader(raw)).Decode(target)
}

func init() {
	gob.Register(map[string]any{})
	gob.Register([]any{})
	gob.Register(dflow.ConstantParameter{})
	gob.Register(dflow.ReferenceParameter{})
	gob.Register(dflow.TemplateParameter{})
	gob.Register(&dflow.ValueSuspension{})
	gob.Register(&dflow.ChildSuspension{})
	gob.Register(&dflow.LoopSuspension{})
	gob.Register(dflow.ChildResponse{})
	gob.Register(&approvalSuspension{})
}
