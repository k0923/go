package rule

import "encoding/json"

// Every node marshals to the canonical JSON form accepted by Parse, with
// "type" first and the shorthands used where they apply, so
// Parse(Marshal(c)) yields an equal tree.

// wire is the flat JSON object shared by every node type.
type wire struct {
	Type  string `json:"type"`
	Name  string `json:"name,omitempty"`
	X     any    `json:"x,omitempty"`
	Opt   string `json:"opt,omitempty"`
	Y     any    `json:"y,omitempty"`
	Items []Cond `json:"items,omitempty"`
}

func (n *And) MarshalJSON() ([]byte, error) { return json.Marshal(wire{Type: "and", Items: n.Items}) }
func (n *Or) MarshalJSON() ([]byte, error)  { return json.Marshal(wire{Type: "or", Items: n.Items}) }
func (n *Not) MarshalJSON() ([]byte, error) { return json.Marshal(wire{Type: "not", X: n.X}) }

func (n *Compare) MarshalJSON() ([]byte, error) {
	return json.Marshal(wire{Type: n.Type.String(), X: xForm(n.X), Opt: n.Op.String(), Y: yForm(n.Y)})
}

func (n *Quant) MarshalJSON() ([]byte, error) {
	return json.Marshal(wire{Type: "array", X: xForm(n.X), Opt: n.Op.String(), Y: n.Y})
}

func (n *Arith) MarshalJSON() ([]byte, error) {
	return json.Marshal(wire{Type: "arith", X: xForm(n.X), Opt: n.Op.String(), Y: yForm(n.Y)})
}

func (n *Field) MarshalJSON() ([]byte, error)   { return json.Marshal(wire{Type: "field", Name: n.Name}) }
func (n *Len) MarshalJSON() ([]byte, error)     { return json.Marshal(wire{Type: "len", X: xForm(n.X)}) }
func (n *Number) MarshalJSON() ([]byte, error)  { return literal(n.V) }
func (n *String) MarshalJSON() ([]byte, error)  { return literal(n.V) }
func (n *Bool) MarshalJSON() ([]byte, error)    { return literal(n.V) }
func (n *Numbers) MarshalJSON() ([]byte, error) { return literal(nonNil(n.V)) }
func (n *Strings) MarshalJSON() ([]byte, error) { return literal(nonNil(n.V)) }

// literal writes an explicit literal. The value is boxed so that zero
// values (false, 0, "") survive omitempty.
func literal(v any) ([]byte, error) {
	return json.Marshal(struct {
		Type  string `json:"type"`
		Value any    `json:"value"`
	}{"literal", v})
}

func nonNil[V any](s []V) []V {
	if s == nil {
		return []V{}
	}
	return s
}

// xForm writes a field in an x position as a bare string.
func xForm(e Expr) any {
	if f, ok := e.(*Field); ok && f != nil {
		return f.Name
	}
	return e
}

// yForm writes a literal in a y position as a bare JSON value. Zero values
// are wrapped in json.RawMessage so omitempty keeps them.
func yForm(e Expr) any {
	var v any
	switch e := e.(type) {
	case *Number:
		v = e.V
	case *String:
		v = e.V
	case *Bool:
		v = e.V
	case *Numbers:
		v = nonNil(e.V)
	case *Strings:
		v = nonNil(e.V)
	default:
		return e
	}
	raw, _ := json.Marshal(v)
	return json.RawMessage(raw)
}
