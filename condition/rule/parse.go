package rule

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Parse decodes a JSON rule. Unknown types, unknown or missing keys,
// misspelt opt values, literals of the wrong type and trailing data are all
// rejected with an *Error carrying the JSON path of the bad node.
func Parse(data []byte) (Cond, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, Errorf("$", ErrInvalidNode, "%v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, Errorf("$", ErrInvalidNode, "unexpected trailing data")
	}
	return parseCond(raw, "$", 0)
}

// object is a decoded JSON object; type holds its "type".
type object struct {
	typ    string
	fields map[string]json.RawMessage
	path   string
	depth  int
}

// The decoding tables: for each "type", the keys it accepts besides "type"
// (all required unless listed in optionalKeys) and its decoder. They are
// filled in init because the decoders recurse back into the tables.
var (
	condTypes map[string]struct {
		keys   []string
		decode func(o *object) (Cond, error)
	}
	exprTypes map[string]struct {
		keys   []string
		decode func(o *object, lit slot) (Expr, error)
	}
	optionalKeys = map[string]bool{"items": true}
)

func init() {
	condTypes = map[string]struct {
		keys   []string
		decode func(o *object) (Cond, error)
	}{
		"and":     {[]string{"items"}, func(o *object) (Cond, error) { items, err := o.items(); return &And{items}, err }},
		"or":      {[]string{"items"}, func(o *object) (Cond, error) { items, err := o.items(); return &Or{items}, err }},
		"not":     {[]string{"x"}, func(o *object) (Cond, error) { x, err := o.cond("x"); return &Not{x}, err }},
		"string":  {[]string{"x", "opt", "y"}, func(o *object) (Cond, error) { return o.compare(TypeString) }},
		"number":  {[]string{"x", "opt", "y"}, func(o *object) (Cond, error) { return o.compare(TypeNumber) }},
		"boolean": {[]string{"x", "opt", "y"}, func(o *object) (Cond, error) { return o.compare(TypeBoolean) }},
		"array":   {[]string{"x", "opt", "y"}, (*object).quant},
	}
	exprTypes = map[string]struct {
		keys   []string
		decode func(o *object, lit slot) (Expr, error)
	}{
		"field":   {[]string{"name"}, func(o *object, _ slot) (Expr, error) { return o.field() }},
		"literal": {[]string{"value"}, func(o *object, lit slot) (Expr, error) { return lit.decode(o.fields["value"], o.at("value")) }},
		"arith":   {[]string{"x", "opt", "y"}, func(o *object, _ slot) (Expr, error) { return o.arith() }},
		"len":     {[]string{"x"}, func(o *object, _ slot) (Expr, error) { f, err := o.listField("x"); return &Len{f}, err }},
	}
}

func open(raw json.RawMessage, path string, depth int, keys func(typ string) ([]string, bool)) (*object, error) {
	if depth > MaxDepth {
		return nil, Errorf(path, ErrTooDeep, "exceeds %d levels", MaxDepth)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, Errorf(path, ErrInvalidNode, "want an object, got %s", preview(raw))
	}
	var typ string
	if err := json.Unmarshal(fields["type"], &typ); err != nil || typ == "" {
		return nil, Errorf(path, ErrInvalidNode, `missing or non-string "type"`)
	}
	allowed, ok := keys(typ)
	if !ok {
		return nil, Errorf(path+".type", ErrUnknownOp, "%q", typ)
	}
	for k := range fields {
		if k != "type" && !slices.Contains(allowed, k) {
			return nil, Errorf(path+"."+k, ErrInvalidNode, "unknown key for type %q", typ)
		}
	}
	for _, k := range allowed {
		if _, ok := fields[k]; !ok && !optionalKeys[k] {
			return nil, Errorf(path, ErrInvalidNode, "type %q needs %q", typ, k)
		}
	}
	return &object{typ: typ, fields: fields, path: path, depth: depth}, nil
}

func parseCond(raw json.RawMessage, path string, depth int) (Cond, error) {
	o, err := open(raw, path, depth, func(typ string) ([]string, bool) {
		if _, isExpr := exprTypes[typ]; isExpr {
			return nil, false
		}
		t, ok := condTypes[typ]
		return t.keys, ok
	})
	if err != nil {
		return nil, wantCond(err, raw, path)
	}
	return condTypes[o.typ].decode(o)
}

// wantCond turns "unknown type field" into a clearer message for expressions
// used where a condition is expected.
func wantCond(err error, raw json.RawMessage, path string) error {
	var head struct{ Type string }
	if json.Unmarshal(raw, &head) == nil {
		if _, isExpr := exprTypes[head.Type]; isExpr {
			return Errorf(path, ErrTypeMismatch, "want a condition, got %q", head.Type)
		}
	}
	return err
}

// parseExpr decodes an operand. lit decides how a JSON scalar or array is
// read; inX selects the x-position shorthand, where a JSON string is a field.
func parseExpr(raw json.RawMessage, path string, depth int, lit slot, inX bool) (Expr, error) {
	switch bytes.TrimLeft(raw, " \t\r\n")[0] {
	case '{':
	case '"':
		if inX {
			var name string
			_ = json.Unmarshal(raw, &name)
			return newField(name, path)
		}
		return lit.decode(raw, path)
	default:
		return lit.decode(raw, path)
	}
	o, err := open(raw, path, depth, func(typ string) ([]string, bool) {
		t, ok := exprTypes[typ]
		return t.keys, ok
	})
	if err != nil {
		var head struct{ Type string }
		if json.Unmarshal(raw, &head) == nil {
			if _, isCond := condTypes[head.Type]; isCond {
				return nil, Errorf(path, ErrTypeMismatch, "want a value, got condition %q", head.Type)
			}
		}
		return nil, err
	}
	return exprTypes[o.typ].decode(o, lit)
}

func (o *object) at(key string) string { return o.path + "." + key }

func (o *object) cond(key string) (Cond, error) {
	return parseCond(o.fields[key], o.at(key), o.depth+1)
}

func (o *object) items() ([]Cond, error) {
	raw, ok := o.fields["items"]
	if !ok {
		return nil, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, Errorf(o.at("items"), ErrInvalidNode, "want an array")
	}
	out := make([]Cond, len(list))
	for i, r := range list {
		c, err := parseCond(r, fmt.Sprintf("%s.items[%d]", o.path, i), o.depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = c
	}
	return out, nil
}

func (o *object) opt(allowed func(Op) bool) (Op, error) {
	var name string
	_ = json.Unmarshal(o.fields["opt"], &name)
	op, ok := ParseOp(name)
	if !ok || !allowed(op) {
		return 0, Errorf(o.at("opt"), ErrUnknownOp, "%s not allowed for type %q", preview(o.fields["opt"]), o.typ)
	}
	return op, nil
}

func (o *object) compare(t Type) (Cond, error) {
	op, err := o.opt(t.Allows)
	if err != nil {
		return nil, err
	}
	scalar := slot{typ: t}
	x, err := parseExpr(o.fields["x"], o.at("x"), o.depth+1, scalar, true)
	if err != nil {
		return nil, err
	}
	y, err := parseExpr(o.fields["y"], o.at("y"), o.depth+1, slot{typ: t, list: op.IsList()}, false)
	if err != nil {
		return nil, err
	}
	return &Compare{Type: t, X: x, Op: op, Y: y}, nil
}

func (o *object) quant() (Cond, error) {
	op, err := o.opt(Op.IsQuant)
	if err != nil {
		return nil, err
	}
	x, err := o.listField("x")
	if err != nil {
		return nil, err
	}
	y, err := o.cond("y")
	if err != nil {
		return nil, err
	}
	return &Quant{X: x, Op: op, Y: y}, nil
}

func (o *object) arith() (Expr, error) {
	op, err := o.opt(Op.IsArith)
	if err != nil {
		return nil, err
	}
	num := slot{typ: TypeNumber}
	x, err := parseExpr(o.fields["x"], o.at("x"), o.depth+1, num, true)
	if err != nil {
		return nil, err
	}
	y, err := parseExpr(o.fields["y"], o.at("y"), o.depth+1, num, false)
	if err != nil {
		return nil, err
	}
	return &Arith{X: x, Op: op, Y: y}, nil
}

func (o *object) field() (*Field, error) {
	var name string
	if err := json.Unmarshal(o.fields["name"], &name); err != nil {
		return nil, Errorf(o.at("name"), ErrInvalidNode, "want a string")
	}
	return newField(name, o.at("name"))
}

// listField reads key as a field name: "items" or {"type":"field","name":"items"}.
func (o *object) listField(key string) (*Field, error) {
	raw := o.fields[key]
	var name string
	if json.Unmarshal(raw, &name) == nil {
		return newField(name, o.at(key))
	}
	e, err := parseExpr(raw, o.at(key), o.depth+1, slot{}, true)
	if err != nil {
		return nil, err
	}
	f, ok := e.(*Field)
	if !ok {
		return nil, Errorf(o.at(key), ErrTypeMismatch, "want a list field")
	}
	return f, nil
}

func newField(name, path string) (*Field, error) {
	if name == "" {
		return nil, Errorf(path, ErrInvalidNode, "empty field name")
	}
	return &Field{Name: name}, nil
}

// slot is the literal type expected at an operand position.
type slot struct {
	typ  Type // 0: no literal allowed
	list bool
}

func (s slot) decode(raw json.RawMessage, path string) (Expr, error) {
	bad := func() (Expr, error) {
		want := "no literal"
		if s.typ != 0 {
			want = s.typ.String()
			if s.list {
				want += " list"
			}
		}
		return nil, Errorf(path, ErrTypeMismatch, "want %s, got %s", want, preview(raw))
	}
	switch {
	case s.typ == TypeString && !s.list:
		if v, ok := scalar[string](raw); ok {
			return &String{v}, nil
		}
	case s.typ == TypeNumber && !s.list:
		if v, ok := scalar[float64](raw); ok {
			return &Number{v}, nil
		}
	case s.typ == TypeBoolean && !s.list:
		if v, ok := scalar[bool](raw); ok {
			return &Bool{v}, nil
		}
	case s.typ == TypeString:
		if v, ok := list[string](raw); ok {
			return &Strings{v}, nil
		}
	case s.typ == TypeNumber:
		if v, ok := list[float64](raw); ok {
			return &Numbers{v}, nil
		}
	}
	return bad()
}

// scalar decodes raw as V, rejecting null (which encoding/json would accept
// as a no-op).
func scalar[V string | float64 | bool](raw json.RawMessage) (V, bool) {
	var v V
	ok := len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, &v) == nil
	return v, ok
}

func list[V string | float64](raw json.RawMessage) ([]V, bool) {
	var elems []json.RawMessage
	if b := bytes.TrimSpace(raw); len(b) == 0 || b[0] != '[' || json.Unmarshal(b, &elems) != nil {
		return nil, false
	}
	out := make([]V, len(elems))
	for i, e := range elems {
		v, ok := scalar[V](e)
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

func preview(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 40 {
		s = s[:37] + "..."
	}
	if s == "" {
		return "nothing"
	}
	return s
}
