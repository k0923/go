package condition

import (
	"fmt"
	"math"
	"reflect"
)

// MaxDepth bounds the nesting of a Node tree accepted by Check/Compile,
// protecting against stack exhaustion from hostile input.
const MaxDepth = 64

// SelfField names the element itself inside quantifiers over scalar lists.
const SelfField = "$"

// Kind is the static type of a field or of a typed node's result.
type Kind uint8

const (
	KindBool Kind = iota + 1
	KindNumber
	KindString
	KindNumbers   // list of numbers
	KindStrings   // list of strings
	KindObjects   // list of objects, see FieldInfo.Elem
	KindEmptyList // the literal []: fits any list position
)

func (k Kind) String() string {
	names := [...]string{"invalid", "bool", "number", "string", "number list", "string list", "object list", "empty list"}
	if int(k) >= len(names) {
		return "invalid"
	}
	return names[k]
}

// IsList reports whether k is one of the list kinds.
func (k Kind) IsList() bool { return k >= KindNumbers && k <= KindEmptyList }

// FieldInfo describes a registered field independently of the Go type that
// carries it. Backends (SQL, MongoDB, ...) only ever see FieldInfo.
type FieldInfo struct {
	Name string
	Kind Kind
	// Elem lists the element fields of a list field. Scalar lists expose a
	// single field named SelfField. Nil for scalar fields.
	Elem map[string]*FieldInfo
}

// TypedNode is a Node that passed Check: every field is resolved, every
// operand kind is known and every structural rule holds. It is the single
// intermediate form shared by the evaluator and by query translators, so
// backends never re-validate and never disagree about what a rule means.
//
// Exactly one of the following shapes applies:
//
//	Field != nil   field reference (relative to the current list scope)
//	Lit != nil     literal; read it through the accessor matching Kind
//	Op != 0        operator applied to Args
//
// For any/all/none, Args[0] is the list field and Args[1] is the predicate,
// whose field references are resolved against Args[0].Field.Elem.
type TypedNode struct {
	Op    Operator
	Kind  Kind
	Args  []*TypedNode
	Field *FieldInfo
	Lit   *Literal
	// Path locates the node in the source Node, e.g. "$.args[1]".
	Path string
}

// IsLiteral reports whether t is a literal.
func (t *TypedNode) IsLiteral() bool { return t.Lit != nil }

// Literal is a checked constant. Its Kind is fixed at Check time and only the
// accessor matching that Kind may be called; any other accessor panics, so a
// backend bug fails loudly in tests instead of emitting a wrong query.
type Literal struct {
	kind Kind
	b    bool
	num  float64
	str  string
	nums []float64
	strs []string
}

// Kind reports the literal's kind.
func (l *Literal) Kind() Kind { return l.kind }

func (l *Literal) want(k Kind) {
	if l.kind != k {
		panic("condition: " + l.kind.String() + " literal read as " + k.String())
	}
}

// Bool returns a KindBool literal.
func (l *Literal) Bool() bool { l.want(KindBool); return l.b }

// Number returns a KindNumber literal.
func (l *Literal) Number() float64 { l.want(KindNumber); return l.num }

// String returns a KindString literal.
func (l *Literal) String() string { l.want(KindString); return l.str }

// Numbers returns a KindNumbers literal. The slice must not be modified.
func (l *Literal) Numbers() []float64 { l.want(KindNumbers); return l.nums }

// Strings returns a KindStrings literal. The slice must not be modified.
func (l *Literal) Strings() []string { l.want(KindStrings); return l.strs }

// Len returns the element count of a list literal (0 for KindEmptyList).
func (l *Literal) Len() int {
	if !l.kind.IsList() {
		panic("condition: len of " + l.kind.String() + " literal")
	}
	return len(l.nums) + len(l.strs)
}

func argPath(path string, i int) string { return fmt.Sprintf("%s.args[%d]", path, i) }

// check validates n against fields and returns its typed form.
func check(fields map[string]*FieldInfo, n Node, path string, depth int) (*TypedNode, error) {
	if depth > MaxDepth {
		return nil, errAt(path, ErrTooDeep, "exceeds %d levels", MaxDepth)
	}
	set := 0
	for _, b := range []bool{n.Op != "", n.Field != "", n.Value != nil} {
		if b {
			set++
		}
	}
	if set != 1 {
		return nil, errAt(path, ErrInvalidNode, "exactly one of op, field, value must be set")
	}
	if n.Op == "" && len(n.Args) > 0 {
		return nil, errAt(path, ErrInvalidNode, "args without op")
	}
	switch {
	case n.Field != "":
		f, ok := fields[n.Field]
		if !ok {
			return nil, errAt(path, ErrUnknownField, "%q", n.Field)
		}
		return &TypedNode{Kind: f.Kind, Field: f, Path: path}, nil
	case n.Value != nil:
		return literal(n.Value, path)
	}

	op, known := ParseOperator(n.Op)
	if !known {
		return nil, errAt(path, ErrUnknownOp, "%q", n.Op)
	}
	out := &TypedNode{Op: op, Path: path}
	arity := func(min, max int) error {
		if len(n.Args) >= min && (max < 0 || len(n.Args) <= max) {
			return nil
		}
		want := fmt.Sprint(min)
		if max < 0 {
			want = fmt.Sprintf("at least %d", min)
		}
		return errAt(path, ErrArity, "%s wants %s, got %d", n.Op, want, len(n.Args))
	}
	// args checks the argument count and every argument in the current scope.
	args := func(min, max int) error {
		if err := arity(min, max); err != nil {
			return err
		}
		out.Args = make([]*TypedNode, len(n.Args))
		for i, a := range n.Args {
			t, err := check(fields, a, argPath(path, i), depth+1)
			if err != nil {
				return err
			}
			out.Args[i] = t
		}
		return nil
	}
	wantBool := func(i int) error {
		if k := out.Args[i].Kind; k != KindBool {
			return errAt(out.Args[i].Path, ErrTypeMismatch, "want bool, got %s", k)
		}
		return nil
	}
	mismatch := func() error {
		return errAt(path, ErrTypeMismatch, "%s on %s and %s", n.Op, out.Args[0].Kind, out.Args[1].Kind)
	}

	switch {
	case op == OpAnd || op == OpOr || op == OpNot:
		min, max := 0, -1
		if op == OpNot {
			min, max = 1, 1
		}
		if err := args(min, max); err != nil {
			return nil, err
		}
		for i := range out.Args {
			if err := wantBool(i); err != nil {
				return nil, err
			}
		}
		out.Kind = KindBool

	case op == OpEq || op == OpNe:
		if err := args(2, 2); err != nil {
			return nil, err
		}
		a, b := out.Args[0].Kind, out.Args[1].Kind
		if a != b || (a != KindNumber && a != KindString && a != KindBool) {
			return nil, mismatch()
		}
		out.Kind = KindBool

	case op.isOrder():
		if err := args(2, 2); err != nil {
			return nil, err
		}
		a, b := out.Args[0].Kind, out.Args[1].Kind
		if a != b || (a != KindNumber && a != KindString) {
			return nil, mismatch()
		}
		out.Kind = KindBool

	case op.isString():
		if err := args(2, 2); err != nil {
			return nil, err
		}
		if out.Args[0].Kind != KindString || out.Args[1].Kind != KindString {
			return nil, mismatch()
		}
		out.Kind = KindBool

	case op == OpIn || op == OpNotIn:
		if err := args(2, 2); err != nil {
			return nil, err
		}
		a, b := out.Args[0].Kind, out.Args[1].Kind
		ok := (a == KindNumber || a == KindString) && b == KindEmptyList ||
			a == KindNumber && b == KindNumbers ||
			a == KindString && b == KindStrings
		if !ok {
			return nil, mismatch()
		}
		out.Kind = KindBool

	case op.isQuant():
		if err := arity(2, 2); err != nil {
			return nil, err
		}
		list, err := check(fields, n.Args[0], argPath(path, 0), depth+1)
		if err != nil {
			return nil, err
		}
		if list.Field == nil || list.Field.Elem == nil {
			return nil, errAt(list.Path, ErrTypeMismatch, "%s wants a list field, got %s", op, list.Kind)
		}
		// The predicate only sees the element's fields.
		pred, err := check(list.Field.Elem, n.Args[1], argPath(path, 1), depth+1)
		if err != nil {
			return nil, err
		}
		if pred.Kind != KindBool {
			return nil, errAt(pred.Path, ErrTypeMismatch, "want bool, got %s", pred.Kind)
		}
		out.Args, out.Kind = []*TypedNode{list, pred}, KindBool

	case op.isArith():
		if err := args(2, 2); err != nil {
			return nil, err
		}
		if out.Args[0].Kind != KindNumber || out.Args[1].Kind != KindNumber {
			return nil, mismatch()
		}
		if y := out.Args[1]; op == OpDiv && y.IsLiteral() && y.Lit.Number() == 0 {
			return nil, &CompileError{Path: y.Path, Err: ErrDivideByZero}
		}
		out.Kind = KindNumber

	default: // OpLen; ParseOperator rejected everything else
		if err := args(1, 1); err != nil {
			return nil, err
		}
		if k := out.Args[0].Kind; !k.IsList() {
			return nil, errAt(out.Args[0].Path, ErrTypeMismatch, "len wants a list, got %s", k)
		}
		out.Kind = KindNumber
	}
	return out, nil
}

// literal converts a Go or JSON-decoded value into a typed literal.
func literal(raw any, path string) (*TypedNode, error) {
	node := func(l *Literal) (*TypedNode, error) {
		return &TypedNode{Kind: l.kind, Lit: l, Path: path}, nil
	}
	rv := reflect.ValueOf(raw)
	if s, ok, err := scalar(rv); err != nil {
		return nil, errAt(path, ErrInvalidNode, "%v", err)
	} else if ok {
		switch s := s.(type) {
		case bool:
			return node(&Literal{kind: KindBool, b: s})
		case float64:
			return node(&Literal{kind: KindNumber, num: s})
		default:
			return node(&Literal{kind: KindString, str: s.(string)})
		}
	}
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, errAt(path, ErrInvalidNode, "unsupported literal type %T", raw)
	}
	if rv.Len() == 0 {
		return node(&Literal{kind: KindEmptyList})
	}
	var nums []float64
	var strs []string
	for i := range rv.Len() {
		s, ok, err := scalar(rv.Index(i))
		if err != nil || !ok {
			return nil, errAt(path, ErrInvalidNode, "unsupported list element %d (%v)", i, rv.Index(i))
		}
		switch s := s.(type) {
		case float64:
			nums = append(nums, s)
		case string:
			strs = append(strs, s)
		default:
			return nil, errAt(path, ErrInvalidNode, "unsupported list element %d (%T)", i, s)
		}
	}
	switch {
	case nums != nil && strs != nil:
		return nil, errAt(path, ErrTypeMismatch, "list mixes numbers and strings")
	case nums != nil:
		return node(&Literal{kind: KindNumbers, nums: nums})
	default:
		return node(&Literal{kind: KindStrings, strs: strs})
	}
}

// scalar normalises bool, string and every numeric kind (to float64).
// Interfaces are unwrapped, so []any from encoding/json works.
func scalar(rv reflect.Value) (any, bool, error) {
	for rv.Kind() == reflect.Interface && !rv.IsNil() {
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool(), true, nil
	case reflect.String:
		return rv.String(), true, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(rv.Uint()), true, nil
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false, fmt.Errorf("non-finite number literal")
		}
		return f, true, nil
	}
	return nil, false, nil
}
