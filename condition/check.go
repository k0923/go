package condition

import (
	"fmt"
	"math"
	"reflect"

	"github.com/k0923/go/condition/rule"
)

// MaxDepth bounds the nesting of a rule accepted by Check/Compile.
const MaxDepth = rule.MaxDepth

// SelfField names the element itself inside quantifiers over scalar lists.
const SelfField = "$"

// Kind is the static type of a registered field.
type Kind uint8

const (
	KindBool Kind = iota + 1
	KindNumber
	KindString
	KindNumbers // list of numbers
	KindStrings // list of strings
	KindObjects // list of objects, see FieldInfo.Elem
)

func (k Kind) String() string {
	names := [...]string{"invalid", "bool", "number", "string", "number list", "string list", "object list"}
	if int(k) >= len(names) {
		return "invalid"
	}
	return names[k]
}

// IsList reports whether k is one of the list kinds.
func (k Kind) IsList() bool { return k >= KindNumbers && k <= KindObjects }

// FieldInfo describes a registered field independently of the Go type that
// carries it. Backends (SQL, MongoDB, ...) only ever see FieldInfo.
type FieldInfo struct {
	Name string
	Kind Kind
	// Elem lists the element fields of a list field. Scalar lists expose a
	// single field named SelfField. Nil for scalar fields.
	Elem map[string]*FieldInfo
}

// Checked is a rule that passed Schema.Check: every field is resolved, every
// operand has the type its comparison declares, and every structural rule
// holds. It is the single input shared by Compile and by query translators,
// so backends never re-validate and never disagree about what a rule means.
//
// A Checked can only be obtained from Check. The rule it wraps must not be
// modified afterwards.
type Checked struct {
	root   rule.Cond
	fields map[*rule.Field]*FieldInfo
	paths  map[rule.Node]string
}

// Root returns the checked rule.
func (c *Checked) Root() rule.Cond { return c.root }

// Field returns the registered field a reference resolved to. Inside a
// Quant, references resolve against the list's element fields.
func (c *Checked) Field(f *rule.Field) *FieldInfo { return c.fields[f] }

// Path returns the JSON path of n, e.g. "$.items[1].x", for error messages.
func (c *Checked) Path(n rule.Node) string { return c.paths[n] }

// kindOf maps a comparison type to the kind of its operands.
var kindOf = map[rule.Type]Kind{rule.TypeString: KindString, rule.TypeNumber: KindNumber, rule.TypeBoolean: KindBool}

// listOf maps a comparison type to the kind of the y of in/nin.
var listOf = map[rule.Type]Kind{rule.TypeString: KindStrings, rule.TypeNumber: KindNumbers}

type checker struct{ out *Checked }

func check(fields map[string]*FieldInfo, root rule.Cond) (*Checked, error) {
	c := &checker{out: &Checked{root: root, fields: map[*rule.Field]*FieldInfo{}, paths: map[rule.Node]string{}}}
	if err := c.cond(fields, root, "$", 0); err != nil {
		return nil, err
	}
	return c.out, nil
}

// isNil also catches typed nil pointers such as (*rule.And)(nil).
func isNil(n rule.Node) bool {
	if n == nil {
		return true
	}
	v := reflect.ValueOf(n)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

func (c *checker) enter(n rule.Node, path string, depth int) error {
	if depth > MaxDepth {
		return rule.Errorf(path, ErrTooDeep, "exceeds %d levels", MaxDepth)
	}
	if isNil(n) {
		return rule.Errorf(path, ErrInvalidNode, "missing node")
	}
	if _, seen := c.out.paths[n]; !seen {
		c.out.paths[n] = path
	}
	return nil
}

func (c *checker) cond(fields map[string]*FieldInfo, n rule.Cond, path string, depth int) error {
	if err := c.enter(n, path, depth); err != nil {
		return err
	}
	items := func(cs []rule.Cond) error {
		for i, it := range cs {
			if err := c.cond(fields, it, fmt.Sprintf("%s.items[%d]", path, i), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	switch n := n.(type) {
	case *rule.And:
		return items(n.Items)
	case *rule.Or:
		return items(n.Items)
	case *rule.Not:
		return c.cond(fields, n.X, path+".x", depth+1)
	case *rule.Compare:
		want, ok := kindOf[n.Type]
		if !ok || !n.Type.Allows(n.Op) {
			return rule.Errorf(path+".opt", ErrUnknownOp, "%s not allowed for type %s", n.Op, n.Type)
		}
		if err := c.want(fields, n.X, want, path+".x", depth+1); err != nil {
			return err
		}
		if n.Op.IsList() {
			want = listOf[n.Type]
		}
		return c.want(fields, n.Y, want, path+".y", depth+1)
	case *rule.Quant:
		if !n.Op.IsQuant() {
			return rule.Errorf(path+".opt", ErrUnknownOp, "%s not allowed for type array", n.Op)
		}
		list, err := c.list(fields, n.X, path+".x", depth+1)
		if err != nil {
			return err
		}
		// The predicate only sees the element's fields.
		return c.cond(list.Elem, n.Y, path+".y", depth+1)
	}
	return rule.Errorf(path, ErrInvalidNode, "unsupported condition %T", n)
}

// want checks e and requires it to be of kind k.
func (c *checker) want(fields map[string]*FieldInfo, e rule.Expr, k Kind, path string, depth int) error {
	got, err := c.expr(fields, e, path, depth)
	if err != nil {
		return err
	}
	if got != k {
		return rule.Errorf(path, ErrTypeMismatch, "want %s, got %s", k, got)
	}
	return nil
}

func (c *checker) field(fields map[string]*FieldInfo, f *rule.Field, path string) (*FieldInfo, error) {
	info, ok := fields[f.Name]
	if !ok {
		return nil, rule.Errorf(path, ErrUnknownField, "%q", f.Name)
	}
	if prev, seen := c.out.fields[f]; seen && prev != info {
		return nil, rule.Errorf(path, ErrInvalidNode, "field node %q shared across list scopes", f.Name)
	}
	c.out.fields[f] = info
	return info, nil
}

func (c *checker) list(fields map[string]*FieldInfo, f *rule.Field, path string, depth int) (*FieldInfo, error) {
	if err := c.enter(f, path, depth); err != nil {
		return nil, err
	}
	info, err := c.field(fields, f, path)
	if err != nil {
		return nil, err
	}
	if !info.Kind.IsList() {
		return nil, rule.Errorf(path, ErrTypeMismatch, "want a list field, got %s", info.Kind)
	}
	return info, nil
}

func (c *checker) expr(fields map[string]*FieldInfo, e rule.Expr, path string, depth int) (Kind, error) {
	if err := c.enter(e, path, depth); err != nil {
		return 0, err
	}
	finite := func(fs ...float64) (Kind, error) {
		for _, f := range fs {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return 0, rule.Errorf(path, ErrInvalidNode, "non-finite number literal")
			}
		}
		return 0, nil
	}
	switch e := e.(type) {
	case *rule.Field:
		info, err := c.field(fields, e, path)
		if err != nil {
			return 0, err
		}
		return info.Kind, nil
	case *rule.Number:
		_, err := finite(e.V)
		return KindNumber, err
	case *rule.String:
		return KindString, nil
	case *rule.Bool:
		return KindBool, nil
	case *rule.Numbers:
		_, err := finite(e.V...)
		return KindNumbers, err
	case *rule.Strings:
		return KindStrings, nil
	case *rule.Arith:
		if !e.Op.IsArith() {
			return 0, rule.Errorf(path+".opt", ErrUnknownOp, "%s not allowed for type arith", e.Op)
		}
		if err := c.want(fields, e.X, KindNumber, path+".x", depth+1); err != nil {
			return 0, err
		}
		if err := c.want(fields, e.Y, KindNumber, path+".y", depth+1); err != nil {
			return 0, err
		}
		if y, ok := e.Y.(*rule.Number); ok && e.Op == rule.OpDiv && y.V == 0 {
			return 0, &CompileError{Path: path + ".y", Err: ErrDivideByZero}
		}
		return KindNumber, nil
	case *rule.Len:
		_, err := c.list(fields, e.X, path+".x", depth+1)
		return KindNumber, err
	}
	return 0, rule.Errorf(path, ErrInvalidNode, "unsupported expression %T", e)
}
