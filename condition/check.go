package condition

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"time"

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
	KindDate
	// KindObject is a nested object registered with Schema.Object. Its
	// leaves are fields of their own; the object itself can only be the x of
	// exists.
	KindObject
)

func (k Kind) String() string {
	names := [...]string{"invalid", "bool", "number", "string", "number list", "string list", "object list", "date", "object"}
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
	// Optional fields may be missing, see Schema.Optional. Backends must
	// make a comparison on a missing scalar false (ne and nin: true), exists
	// false, and treat a missing list as empty.
	Optional bool

	macros map[string]*macro // element macros of a list field
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

// Root returns the checked rule. Calls are replaced by their macro
// expansions, so backends never see a *rule.Call.
func (c *Checked) Root() rule.Cond { return c.root }

// Field returns the registered field a reference resolved to. Inside a
// Quant, references resolve against the list's element fields.
func (c *Checked) Field(f *rule.Field) *FieldInfo { return c.fields[f] }

// Path returns the JSON path of n, e.g. "$.items[1].x", for error messages.
func (c *Checked) Path(n rule.Node) string { return c.paths[n] }

// kindOf maps a comparison type to the kind of its operands.
var kindOf = map[rule.Type]Kind{rule.TypeString: KindString, rule.TypeNumber: KindNumber, rule.TypeBoolean: KindBool, rule.TypeDate: KindDate}

// listOf maps a comparison type to the kind of the y of in/nin.
var listOf = map[rule.Type]Kind{rule.TypeString: KindStrings, rule.TypeNumber: KindNumbers}

// scope is what references resolve against: the root schema, or the
// element schema inside a Quant.
type scope struct {
	fields map[string]*FieldInfo
	macros map[string]*macro
}

// maxExpansions bounds the macro calls expanded for one rule, so a macro
// that calls itself twice cannot blow up exponentially within MaxDepth.
const maxExpansions = 1024

type checker struct {
	out        *Checked
	expansions int
}

func check(sc scope, root rule.Cond) (*Checked, error) {
	c := &checker{out: &Checked{fields: map[*rule.Field]*FieldInfo{}, paths: map[rule.Node]string{}}}
	out, err := c.cond(sc, root, "$", 0)
	if err != nil {
		return nil, err
	}
	c.out.root = out
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

// cond checks n and returns it with every Call expanded. Nodes without a
// Call below them are returned unchanged; their ancestors are copied.
func (c *checker) cond(sc scope, n rule.Cond, path string, depth int) (rule.Cond, error) {
	if err := c.enter(n, path, depth); err != nil {
		return nil, err
	}
	items := func(cs []rule.Cond) ([]rule.Cond, bool, error) {
		out, changed := cs, false
		for i, it := range cs {
			x, err := c.cond(sc, it, fmt.Sprintf("%s.items[%d]", path, i), depth+1)
			if err != nil {
				return nil, false, err
			}
			if x != it && !changed {
				out, changed = slices.Clone(cs), true
			}
			out[i] = x
		}
		return out, changed, nil
	}
	switch n := n.(type) {
	case *rule.And:
		xs, changed, err := items(n.Items)
		if err != nil || !changed {
			return n, err
		}
		return c.copied(&rule.And{Items: xs}, path), nil
	case *rule.Or:
		xs, changed, err := items(n.Items)
		if err != nil || !changed {
			return n, err
		}
		return c.copied(&rule.Or{Items: xs}, path), nil
	case *rule.Not:
		x, err := c.cond(sc, n.X, path+".x", depth+1)
		if err != nil || x == n.X {
			return n, err
		}
		return c.copied(&rule.Not{X: x}, path), nil
	case *rule.Compare:
		want, ok := kindOf[n.Type]
		if !ok || !n.Type.Allows(n.Op) {
			return nil, rule.Errorf(path+".opt", ErrUnknownOp, "%s not allowed for type %s", n.Op, n.Type)
		}
		if err := c.want(sc.fields, n.X, want, path+".x", depth+1); err != nil {
			return nil, err
		}
		if n.Op.IsList() {
			want = listOf[n.Type]
		}
		return n, c.want(sc.fields, n.Y, want, path+".y", depth+1)
	case *rule.Quant:
		if !n.Op.IsQuant() {
			return nil, rule.Errorf(path+".opt", ErrUnknownOp, "%s not allowed for type array", n.Op)
		}
		list, err := c.list(sc.fields, n.X, path+".x", depth+1)
		if err != nil {
			return nil, err
		}
		// The predicate only sees the element's fields and macros.
		y, err := c.cond(scope{list.Elem, list.macros}, n.Y, path+".y", depth+1)
		if err != nil || y == n.Y {
			return n, err
		}
		return c.copied(&rule.Quant{X: n.X, Op: n.Op, Y: y}, path), nil
	case *rule.Exists:
		if err := c.enter(n.X, path+".x", depth+1); err != nil {
			return nil, err
		}
		_, err := c.field(sc.fields, n.X, path+".x")
		return n, err
	case *rule.Call:
		return c.call(sc, n, path, depth)
	}
	return nil, rule.Errorf(path, ErrInvalidNode, "unsupported condition %T", n)
}

func (c *checker) copied(n rule.Cond, path string) rule.Cond {
	c.out.paths[n] = path
	return n
}

// call checks the arguments against the macro's parameters, expands it and
// checks the expansion in the caller's scope. Nodes of the expansion get
// paths such as "$.items[1]<vip>.x".
func (c *checker) call(sc scope, n *rule.Call, path string, depth int) (rule.Cond, error) {
	m, ok := sc.macros[n.Name]
	if !ok {
		return nil, rule.Errorf(path+".name", ErrUnknownMacro, "%q", n.Name)
	}
	if c.expansions++; c.expansions > maxExpansions {
		return nil, rule.Errorf(path, ErrTooDeep, "more than %d macro expansions", maxExpansions)
	}
	if len(n.Args) != len(m.params) {
		return nil, rule.Errorf(path+".args", ErrTypeMismatch, "macro %q takes %d arguments, got %d", n.Name, len(m.params), len(n.Args))
	}
	args := make([]rule.Expr, len(n.Args))
	callerNodes := map[rule.Node]bool{}
	for i, a := range n.Args {
		ap := fmt.Sprintf("%s.args[%d]", path, i)
		a, err := coerceArg(a, m.params[i], ap)
		if err != nil {
			return nil, err
		}
		if err := c.want(sc.fields, a, m.params[i], ap, depth+1); err != nil {
			return nil, err
		}
		args[i] = a
		rule.Walk(a, func(x rule.Node) { callerNodes[x] = true })
	}
	body := m.expand(slices.Clone(args))
	if isNil(body) {
		return nil, rule.Errorf(path, ErrInvalidNode, "macro %q expanded to nothing", n.Name)
	}
	if m.prefix != "" {
		body = prefixCond(body, m.prefix, callerNodes)
	}
	return c.cond(sc, body, path+"<"+n.Name+">", depth+1)
}

// coerceArg gives a naturally typed JSON literal the type the macro
// declares: an RFC 3339 String becomes a Date, an empty list either list.
func coerceArg(a rule.Expr, k Kind, path string) (rule.Expr, error) {
	switch v := a.(type) {
	case *rule.String:
		if k == KindDate {
			t, err := time.Parse(time.RFC3339Nano, v.V)
			if err != nil {
				return nil, rule.Errorf(path, ErrTypeMismatch, "want an RFC 3339 date, got %q", v.V)
			}
			return &rule.Date{V: t}, nil
		}
	case *rule.Strings:
		if k == KindNumbers && len(v.V) == 0 {
			return &rule.Numbers{}, nil
		}
	case *rule.Numbers:
		if k == KindStrings && len(v.V) == 0 {
			return &rule.Strings{}, nil
		}
	}
	return a, nil
}

// prefixCond copies the expansion of a macro lifted by Schema.Object,
// prefixing the fields and macros it references at its top level. Quant
// predicates are element-scoped and caller arguments already resolve in the
// caller's scope, so neither is touched.
func prefixCond(n rule.Cond, p string, caller map[rule.Node]bool) rule.Cond {
	if isNil(n) {
		return n
	}
	conds := func(cs []rule.Cond) []rule.Cond {
		out := make([]rule.Cond, len(cs))
		for i, x := range cs {
			out[i] = prefixCond(x, p, caller)
		}
		return out
	}
	switch n := n.(type) {
	case *rule.And:
		return &rule.And{Items: conds(n.Items)}
	case *rule.Or:
		return &rule.Or{Items: conds(n.Items)}
	case *rule.Not:
		return &rule.Not{X: prefixCond(n.X, p, caller)}
	case *rule.Compare:
		return &rule.Compare{Type: n.Type, X: prefixExpr(n.X, p, caller), Op: n.Op, Y: prefixExpr(n.Y, p, caller)}
	case *rule.Quant:
		return &rule.Quant{X: prefixField(n.X, p, caller), Op: n.Op, Y: n.Y}
	case *rule.Exists:
		return &rule.Exists{X: prefixField(n.X, p, caller)}
	case *rule.Call:
		args := make([]rule.Expr, len(n.Args))
		for i, a := range n.Args {
			args[i] = prefixExpr(a, p, caller)
		}
		return &rule.Call{Name: p + n.Name, Args: args}
	}
	return n
}

func prefixExpr(e rule.Expr, p string, caller map[rule.Node]bool) rule.Expr {
	if isNil(e) || caller[e] {
		return e
	}
	switch e := e.(type) {
	case *rule.Field:
		return prefixField(e, p, caller)
	case *rule.Arith:
		return &rule.Arith{X: prefixExpr(e.X, p, caller), Op: e.Op, Y: prefixExpr(e.Y, p, caller)}
	case *rule.Len:
		return &rule.Len{X: prefixField(e.X, p, caller)}
	}
	return e
}

func prefixField(f *rule.Field, p string, caller map[rule.Node]bool) *rule.Field {
	if f == nil || caller[f] {
		return f
	}
	return &rule.Field{Name: p + f.Name}
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
	case *rule.Date:
		return KindDate, nil
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
