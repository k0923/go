package tests

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/k0923/go/dflow"
)

type configuredNode struct {
	Label string `json:"label"`
}

func (*configuredNode) Type() string { return "configured" }
func (n *configuredNode) Run(context.Context, map[string]any) (dflow.Result, error) {
	return dflow.Result{Output: n.Label}, nil
}

type decodedNode struct {
	label string
}

func (*decodedNode) Type() string { return "decoded" }
func (n *decodedNode) DecodeNodeJSON(raw json.RawMessage) error {
	var config struct {
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return err
	}
	n.label = config.Prefix + "-decoded"
	return nil
}
func (n *decodedNode) Run(context.Context, map[string]any) (dflow.Result, error) {
	return dflow.Result{Output: n.label}, nil
}

func TestRegistryFactoryAndConfig(t *testing.T) {
	var registry dflow.Registry // Zero value follows xfield's convention.
	var calls atomic.Int32
	if err := registry.Register(func() dflow.Node {
		calls.Add(1)
		return &configuredNode{}
	}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("registration must discover Type through one factory call")
	}
	first, err := registry.DecodeNode(dflow.NodeSpec{Type: "configured", Config: json.RawMessage(`{"label":"first"}`)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.DecodeNode(dflow.NodeSpec{Type: "configured", Config: json.RawMessage(`{"label":"second"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if first == second || calls.Load() != 3 {
		t.Fatal("each decode must construct a fresh instance")
	}
	one, _ := first.Run(context.Background(), nil)
	two, _ := second.Run(context.Background(), nil)
	if one.Output != "first" || two.Output != "second" {
		t.Fatal("node configurations leaked", one, two)
	}
	if err := registry.Register(func() dflow.Node { return &configuredNode{} }); !errors.Is(err, dflow.ErrInvalidDefinition) {
		t.Fatal("duplicate registration must be rejected", err)
	}
	for _, config := range []string{`{"unknown":1}`, `null`, `{"label":"x"} {}`} {
		if _, err := registry.DecodeNode(dflow.NodeSpec{Type: "configured", Config: json.RawMessage(config)}); err == nil {
			t.Fatal("invalid config accepted", config)
		}
	}
	if err := registry.Register(func() dflow.Node { return &decodedNode{} }); err != nil {
		t.Fatal(err)
	}
	custom, err := registry.DecodeNode(dflow.NodeSpec{Type: "decoded", Config: json.RawMessage(`{"prefix":"custom"}`)})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := custom.Run(context.Background(), nil)
	if out.Output != "custom-decoded" {
		t.Fatal("custom decoder was not used", out)
	}
}

func TestRegistryRejectsInvalidFactoriesAndTypeChanges(t *testing.T) {
	registry := dflow.NewRegistry()
	for _, factory := range []dflow.Factory{
		nil,
		func() dflow.Node { return nil },
		func() dflow.Node { return (*configuredNode)(nil) },
		func() dflow.Node { return &dflow.FuncNode{} },
	} {
		if err := registry.Register(factory); !errors.Is(err, dflow.ErrInvalidDefinition) {
			t.Fatal("invalid factory accepted", err)
		}
	}
	var count atomic.Int32
	if err := registry.Register(func() dflow.Node {
		name := "initial"
		if count.Add(1) > 1 {
			name = "changed"
		}
		return &dflow.FuncNode{Kind: name}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DecodeNode(dflow.NodeSpec{Type: "initial"}); !errors.Is(err, dflow.ErrInvalidDefinition) {
		t.Fatal("unstable factory type accepted", err)
	}
}

func TestRegistryBuiltinsAreAtomicAndInstanceScoped(t *testing.T) {
	registry := dflow.NewRegistry()
	if err := registry.Register(func() dflow.Node {
		return &dflow.FuncNode{Kind: "condition", Handler: func(context.Context, map[string]any) (dflow.Result, error) {
			return dflow.Result{Output: "custom"}, nil
		}}
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterBuiltins(); !errors.Is(err, dflow.ErrInvalidDefinition) {
		t.Fatal("builtin conflict must reject the batch", err)
	}
	if _, err := registry.DecodeNode(dflow.NodeSpec{Type: "start"}); !errors.Is(err, dflow.ErrInvalidDefinition) {
		t.Fatal("partial builtin registration occurred", err)
	}
	if _, err := dflow.DefaultRegistry().DecodeNode(dflow.NodeSpec{Type: "start"}); err != nil {
		t.Fatal("one registry leaked into another", err)
	}
}

func TestRegistryConcurrentDecode(t *testing.T) {
	registry := dflow.DefaultRegistry()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, err := registry.DecodeNode(dflow.NodeSpec{Type: "passthrough"})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
