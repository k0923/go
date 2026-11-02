package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"

	"github.com/k0923/go/dflow"
)

type edge struct{ source, target string }

func (e edge) GetSourceNodeID() string { return e.source }
func (e edge) GetTargetNodeID() string { return e.target }

type topology struct{ incoming, outgoing []string }
type buildContext map[string]topology

type definition struct {
	nodes []dflow.NodeSpec[buildContext, *execution]
	edges []dflow.Edge
}

func (d definition) GetNodes() []dflow.NodeSpec[buildContext, *execution] { return d.nodes }
func (d definition) GetEdges() []dflow.Edge                               { return d.edges }

type nodeSpec struct{ id, source string }

func (s nodeSpec) GetID() string { return s.id }
func (s nodeSpec) Validate(ctx context.Context, _ buildContext) error {
	return ctx.Err()
}
func (s nodeSpec) Build(_ context.Context, graph buildContext) (dflow.PublishedNode[*execution], error) {
	index := graph[s.id]
	node := dflow.NodeFunc[*execution](func(ctx context.Context, runtime *execution) dflow.NodeResult {
		if s.source == "" {
			return dflow.Result{Output: map[string]any{"orderID": runtime.orderID}}
		}
		output, err := runtime.store.output(ctx, s.source)
		if err != nil {
			return dflow.ResultWithError(err)
		}
		return dflow.Result{
			Input:  output,
			Output: map[string]any{"message": fmt.Sprintf("订单 %v 已处理", output["orderID"])},
		}
	})
	var deps []string
	if s.source != "" {
		deps = []string{s.source}
	}
	return published{s.id, node, index.incoming, index.outgoing, deps}, nil
}

type published struct {
	id                 string
	node               dflow.RuntimeNode[*execution]
	incoming, outgoing []string
	dependencies       []string
}

func (p published) GetID() string                          { return p.id }
func (p published) GetNode() dflow.RuntimeNode[*execution] { return p.node }
func (p published) GetIncoming() []string                  { return p.incoming }
func (p published) GetOutgoing() []string                  { return p.outgoing }
func (p published) GetDependencies() []string              { return p.dependencies }

type provider map[string]dflow.PublishedNode[*execution]

func (p provider) LoadNode(ctx context.Context, id string) (dflow.PublishedNode[*execution], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record, ok := p[id]
	if !ok {
		return nil, fmt.Errorf("unknown published node %q", id)
	}
	return record, nil
}

type execution struct {
	orderID string
	store   *store
}

func main() {
	ctx := context.Background()
	def := definition{
		nodes: []dflow.NodeSpec[buildContext, *execution]{
			nodeSpec{id: "order"},
			nodeSpec{id: "notify", source: "order"},
		},
		edges: []dflow.Edge{edge{"order", "notify"}},
	}
	// Business derives every publication's topology from the same edge table.
	index := buildContext{}
	for _, edge := range def.edges {
		source, target := edge.GetSourceNodeID(), edge.GetTargetNodeID()
		from, to := index[source], index[target]
		from.outgoing = append(from.outgoing, target)
		to.incoming = append(to.incoming, source)
		index[source], index[target] = from, to
	}
	records, err := dflow.Compile(ctx, def, index)
	if err != nil {
		log.Fatal(err)
	}
	nodes := provider{}
	states := map[string]dflow.NodeState{}
	for _, record := range records {
		nodes[record.GetID()] = record
		states[record.GetID()] = dflow.NodeState{Status: dflow.Pending}
	}
	root := records[0].GetID()
	states[root] = dflow.NodeState{Status: dflow.Pending, Ready: true, Enabled: true, Version: 1}
	storage := &store{nodes: nodes, states: states}
	engine, err := dflow.NewEngine[*execution](nodes, storage, dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	runtime := &execution{orderID: "order-42", store: storage}
	queue := []string{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		step, err := engine.Drive(ctx, id, runtime)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: %s → ready=%v\n", step.NodeID, step.Status, step.Ready)
		queue = append(queue, step.Ready...)
	}
	output, err := storage.output(ctx, "notify")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(output["message"])
}

// This example stores only strings and bools. With mutable nested values,
// business storage must provide a deep snapshot or serialize the values.
func cloneState(state dflow.NodeState) dflow.NodeState {
	state.Input = maps.Clone(state.Input)
	state.Output = maps.Clone(state.Output)
	state.Incoming = maps.Clone(state.Incoming)
	state.Outgoing = slices.Clone(state.Outgoing)
	return state
}
