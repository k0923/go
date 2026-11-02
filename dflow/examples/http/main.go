package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/examples/nodes"
	"github.com/k0923/go/dflow/memory"
)

func main() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "你好，" + r.URL.Query().Get("name")})
	}))
	defer server.Close()
	ctx := context.Background()
	records, err := nodes.Compile(ctx, nodes.Definition{
		Nodes: []nodes.Spec{
			{ID: "request", Node: &nodes.HTTPNode{Method: "GET", TimeoutMS: 5000},
				Inputs: map[string]nodes.Param{
					"url": nodes.Const(server.URL), "params": nodes.Input("query"),
				}},
			{ID: "parse", Node: &nodes.JSONParseNode{}, Inputs: map[string]nodes.Param{"json": nodes.Ref("request", "body")}},
			{ID: "end", Node: &nodes.EndNode{}, Inputs: map[string]nodes.Param{"message": nodes.Ref("parse", "message")}},
		},
		Edges: []nodes.Edge{
			{Source: "request", Target: "parse"},
			{Source: "parse", Target: "end"},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	store, publication, err := memory.NewRun(records, "request")
	if err != nil {
		log.Fatal(err)
	}
	engine, err := dflow.NewEngine(publication, store, dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	state, err := driver.Run(ctx, engine, "request", nodes.RuntimeWithStore(map[string]any{"query": map[string]any{"name": "HTTP"}}, store))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(state.Nodes["end"].Output["message"])
}
