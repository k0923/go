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
	"github.com/k0923/go/dflow/memory"
)

func main() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "你好，" + r.URL.Query().Get("name")})
	}))
	defer server.Close()
	graph, err := dflow.Compile(dflow.Definition{
		Version: "http-v1",
		Nodes: []dflow.NodeSpec{
			{ID: "request", Node: &dflow.HTTPNode{Method: "GET", TimeoutMS: 5000},
				Inputs: dflow.Parameters{
					"url": dflow.Constant(server.URL), "params": dflow.Input("query"),
				}},
			{ID: "parse", Node: &dflow.JSONParseNode{}, Inputs: dflow.Parameters{"json": dflow.Ref("request", "body")}},
			{ID: "end", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"message": dflow.Ref("parse", "message")}},
		},
		Edges: []dflow.Edge{
			{SourceNodeID: "request", TargetNodeID: "parse"},
			{SourceNodeID: "parse", TargetNodeID: "end"},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	engine, err := dflow.NewEngine(graph, memory.New(), dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	state, err := driver.Run(context.Background(), engine, map[string]any{"query": map[string]any{"name": "HTTP"}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(state.Nodes["end"].Output["message"])
}
