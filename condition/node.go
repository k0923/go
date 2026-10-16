package condition

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Node is the declarative, serialisable form of a condition. Exactly one of
// Op, Field or Value is set:
//
//	{"op":"ge","args":[{"field":"age"},{"value":18}]}
//	{"op":"in","args":[{"field":"city"},{"value":["bj","sh"]}]}
//	{"op":"any","args":[{"field":"children"},{"op":"lt","args":[{"field":"age"},{"value":3}]}]}
//
// Supported operators (arity in brackets):
//
//	and, or [0..n]           not [1]
//	eq, ne [2]               number, string or bool operands of the same kind
//	lt, le, gt, ge [2]       number or string operands
//	contains, starts_with, ends_with [2]   string operands
//	in, not_in [2]           scalar, then a literal list or a list field
//	any, all, none [2]       list field, then a predicate on its elements
//	add, sub, mul, div [2]   number operands, yields a number
//	len [1]                  list operand, yields a number
//
// Inside any/all/none over a scalar list field, {"field":"$"} is the element.
// A bare {"value":true} or {"value":false} is a constant condition.
type Node struct {
	Op    string `json:"op,omitempty"`
	Args  []Node `json:"args,omitempty"`
	Field string `json:"field,omitempty"`
	// Value is a literal: number, string, bool, or a list of numbers or of
	// strings. It is untyped only because it is the wire format; Check turns
	// it into a typed *Literal and rejects anything else with a path. In Go
	// code build it with the typed LitXxx constructors below.
	Value any `json:"value,omitzero"`
}

// Op builds an operator node.
func Op(op Operator, args ...Node) Node { return Node{Op: op.String(), Args: args} }

// Ref builds a field reference node.
func Ref(field string) Node { return Node{Field: field} }

// LitBool builds a bool literal node.
func LitBool(v bool) Node { return Node{Value: v} }

// LitNum builds a number literal node.
func LitNum[N Number](v N) Node { return Node{Value: float64(v)} }

// LitStr builds a string literal node.
func LitStr[S ~string](v S) Node { return Node{Value: string(v)} }

// LitNums builds a number list literal node.
func LitNums[N Number](vs ...N) Node {
	out := make([]float64, len(vs))
	for i, v := range vs {
		out[i] = float64(v)
	}
	return Node{Value: out}
}

// LitStrs builds a string list literal node.
func LitStrs[S ~string](vs ...S) Node {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return Node{Value: out}
}

// ParseNode decodes a Node strictly: unknown keys (for instance a misspelt
// "opt") and trailing data are rejected instead of silently ignored.
func ParseNode(data []byte) (Node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var n Node
	if err := dec.Decode(&n); err != nil {
		return Node{}, fmt.Errorf("condition: decode node: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Node{}, fmt.Errorf("condition: decode node: unexpected trailing data")
	}
	return n, nil
}
