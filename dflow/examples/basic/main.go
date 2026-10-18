package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/memory"
)

func main() {
	const definitionJSON = `{
  "version": "1",
  "nodes": [
    {"id":"start","type":"start"},
    {"id":"calculate","type":"code",
      "inputsValues":{"price":{"type":"ref","content":["start","price"]}},
      "config":{"script":{"language":"javascript","content":"function main({params}) { return { total: params.price * 2 }; }"}}
    },
    {"id":"end","type":"end",
      "inputsValues":{"total":{"type":"ref","content":["calculate","total"]}}
    }
  ],
  "edges": [
    {"sourceNodeID":"start","targetNodeID":"calculate"},
    {"sourceNodeID":"calculate","targetNodeID":"end"}
  ]
}`
	var definition dflow.Definition
	if err := json.Unmarshal([]byte(definitionJSON), &definition); err != nil {
		log.Fatal(err)
	}
	graph, err := dflow.Compile(definition, dflow.DefaultRegistry())
	if err != nil {
		log.Fatal(err)
	}
	engine, err := dflow.NewEngine(graph, memory.New(), dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	state, err := engine.Run(context.Background(), map[string]any{"price": 21})
	if err != nil {
		log.Fatal(err)
	}
	output, err := json.Marshal(state.Nodes["end"].Output)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(output))
}
