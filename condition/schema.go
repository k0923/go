package condition

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/k0923/go/condition/rule"
)

// Schema declares the fields a rule may reference and binds each of them to
// an accessor on T. The same Schema serves two purposes:
//
//   - Check validates a rule.Cond into a *Checked, the backend-neutral input
//     of query translators (see the sqlquery and mongoquery packages);
//   - Compile additionally builds an in-memory Condition[T] from it.
//
// Register every field before the first Check/Compile.
type Schema[T any] struct {
	fields map[string]*FieldInfo
	bind   map[*FieldInfo]binding[T]
	macros map[string]*macro
}

// macro is a registered rule.Call target.
type macro struct {
	params []Kind
	expand func(args []rule.Expr) rule.Cond
	// prefix is prepended to the fields and macros the expansion references
	// when the macro was lifted from an object schema by Object.
	prefix string
}

// binding is an evaluated operand: only the member matching its kind is set
// (plus length and quant for list kinds).
type binding[T any] struct {
	bl     Expr[T, bool]
	num    Expr[T, float64]
	str    Expr[T, string]
	nums   Expr[T, []float64]
	strs   Expr[T, []string]
	date   Expr[T, time.Time]
	length Expr[T, int]
	// present reports whether an optional field is present; nil for fields
	// that always are.
	present func(T) bool
	// quant builds any/all/none with a predicate already checked against the
	// element schema; it captures the element type E at registration time.
	quant func(q quantifier, chk *Checked, pred rule.Cond) Condition[T]
}

// NewSchema returns an empty schema.
func NewSchema[T any]() *Schema[T] {
	return &Schema[T]{fields: map[string]*FieldInfo{}, bind: map[*FieldInfo]binding[T]{}, macros: map[string]*macro{}}
}

func (s *Schema[T]) add(name string, k Kind, elem map[string]*FieldInfo, b binding[T]) *Schema[T] {
	if name == "" {
		panic("condition: empty field name")
	}
	if _, dup := s.fields[name]; dup {
		panic(fmt.Sprintf("condition: field %q registered twice", name))
	}
	f := &FieldInfo{Name: name, Kind: k, Elem: elem}
	s.fields[name] = f
	s.bind[f] = b
	return s
}

func mustGetter(name string, isNil bool) {
	if isNil {
		panic("condition: nil accessor for field " + name)
	}
}

// Macro registers a named condition that rules invoke with
// {"type":"call","name":name,"args":[...]}. params declares the kind of each
// argument; Check verifies the arguments, calls expand and checks the
// expansion in place of the call, so a macro is exactly the rule it expands
// to: every backend supports it and evaluation, SQL and MongoDB agree.
//
// expand receives checked arguments and must return a fresh tree built from
// the fields and macros of s; it may embed the arguments anywhere except
// inside an array predicate, whose fields are the element's. Macros may call
// other macros; Check bounds the total expansion.
//
// Macros of an element schema are available inside array rules over that
// list, and Object lifts the macros of an object schema as "name.macro".
// Register macros before the schema is used by List or Object.
func (s *Schema[T]) Macro(name string, params []Kind, expand func(args []rule.Expr) rule.Cond) *Schema[T] {
	if name == "" {
		panic("condition: empty macro name")
	}
	if expand == nil {
		panic("condition: nil expansion for macro " + name)
	}
	for _, k := range params {
		switch k {
		case KindBool, KindNumber, KindString, KindDate, KindNumbers, KindStrings:
		default:
			panic(fmt.Sprintf("condition: macro %s: parameter kind %s cannot be passed", name, k))
		}
	}
	return s.addMacro(name, &macro{params: slices.Clone(params), expand: expand})
}

func (s *Schema[T]) addMacro(name string, m *macro) *Schema[T] {
	if _, dup := s.macros[name]; dup {
		panic(fmt.Sprintf("condition: macro %q registered twice", name))
	}
	s.macros[name] = m
	return s
}

// Field returns the description of a registered field.
func (s *Schema[T]) Field(name string) (*FieldInfo, bool) {
	f, ok := s.fields[name]
	return f, ok
}

// Bool registers a boolean field.
func (s *Schema[T]) Bool(name string, get func(T) bool) *Schema[T] {
	mustGetter(name, get == nil)
	return s.add(name, KindBool, nil, binding[T]{bl: Field(get)})
}

// String registers a string field.
func (s *Schema[T]) String[S ~string](name string, get func(T) S) *Schema[T] {
	mustGetter(name, get == nil)
	if f, ok := any(get).(func(T) string); ok {
		return s.add(name, KindString, nil, binding[T]{str: Field(f)})
	}
	return s.add(name, KindString, nil, binding[T]{str: Field(func(v T) string { return string(get(v)) })})
}

// Number registers a numeric field. Values are compared as float64, so
// integers beyond 2^53 lose precision.
func (s *Schema[T]) Number[N Number](name string, get func(T) N) *Schema[T] {
	mustGetter(name, get == nil)
	// Skip the conversion wrapper when the accessor already yields float64.
	if f, ok := any(get).(func(T) float64); ok {
		return s.add(name, KindNumber, nil, binding[T]{num: Field(f)})
	}
	return s.add(name, KindNumber, nil, binding[T]{num: Field(func(v T) float64 { return float64(get(v)) })})
}

// Date registers a date field. Dates compare as instants.
func (s *Schema[T]) Date(name string, get func(T) time.Time) *Schema[T] {
	mustGetter(name, get == nil)
	return s.add(name, KindDate, nil, binding[T]{date: Field(get)})
}

// Optional marks the registered field name as possibly missing; has reports
// whether it is present in a value. A comparison with a missing operand is
// false, ne and nin being the negations of eq and in, and exists is false.
// The accessor of a missing scalar is never called. A missing list still
// reads through its accessor, which must then return an empty list.
//
// Marking an object optional makes all of its leaves optional too; a leaf
// is then present when the object and the leaf both are.
func (s *Schema[T]) Optional(name string, has func(T) bool) *Schema[T] {
	mustGetter(name, has == nil)
	f, ok := s.fields[name]
	if !ok {
		panic(fmt.Sprintf("condition: Optional on unregistered field %q", name))
	}
	s.markOptional(f, has)
	if f.Kind == KindObject {
		for n, leaf := range s.fields {
			if strings.HasPrefix(n, name+".") {
				s.markOptional(leaf, has)
			}
		}
	}
	return s
}

func (s *Schema[T]) markOptional(f *FieldInfo, has func(T) bool) {
	b := s.bind[f]
	if prev := b.present; prev != nil {
		b.present = func(v T) bool { return has(v) && prev(v) }
	} else {
		b.present = has
	}
	s.bind[f] = b
	f.Optional = true
}

// Strings registers a string list field. Inside an array rule the element is
// referenced as "$".
func (s *Schema[T]) Strings(name string, get func(T) []string) *Schema[T] {
	mustGetter(name, get == nil)
	items := Field(get)
	elem := NewSchema[string]().String(SelfField, func(v string) string { return v })
	return s.add(name, KindStrings, elem.fields, binding[T]{strs: items, length: Len(items), quant: quantOver(items, elem)})
}

// Numbers registers a number list field. Inside an array rule the element is
// referenced as "$".
func (s *Schema[T]) Numbers(name string, get func(T) []float64) *Schema[T] {
	mustGetter(name, get == nil)
	items := Field(get)
	elem := NewSchema[float64]().Number(SelfField, func(v float64) float64 { return v })
	return s.add(name, KindNumbers, elem.fields, binding[T]{nums: items, length: Len(items), quant: quantOver(items, elem)})
}

// List registers a list whose elements are described by elem. It is the x
// of array rules and of len. elem may be s itself for recursive structures.
//
// Elements need not be objects: a list of lists registers its element list
// as the field "$" of elem, for example
//
//	s.List("matrix", getMatrix, NewSchema[[]float64]().Numbers("$", func(r []float64) []float64 { return r }))
//
// and rules nest array rules over "$".
func (s *Schema[T]) List[E any](name string, get func(T) []E, elem *Schema[E]) *Schema[T] {
	mustGetter(name, get == nil)
	if elem == nil {
		panic("condition: nil element schema")
	}
	items := Field(get)
	s.add(name, KindObjects, elem.fields, binding[T]{length: Len(items), quant: quantOver(items, elem)})
	s.fields[name].macros = elem.macros
	return s
}

// Object registers the fields of a nested object under name: every field f
// of elem becomes the field "name.f" of s, of the same kind. Nested objects
// and lists of elem are included, so Object composes with itself and with
// List. Rules reference the leaves ("buyer.level"); the object itself is a
// field of KindObject that can only be tested with exists, once marked
// Optional.
//
// elem is copied at registration time: register all of its fields first.
// Because of the copy elem cannot be s itself; use List for recursive
// structures.
func (s *Schema[T]) Object[E any](name string, get func(T) E, elem *Schema[E]) *Schema[T] {
	mustGetter(name, get == nil)
	if elem == nil {
		panic("condition: nil object schema")
	}
	if any(elem) == any(s) {
		panic("condition: object " + name + " cannot embed its own schema")
	}
	s.add(name, KindObject, nil, binding[T]{})
	for sub, f := range elem.fields {
		s.add(name+"."+sub, f.Kind, f.Elem, liftBinding(get, elem.bind[f]))
		lifted := s.fields[name+"."+sub]
		lifted.Optional, lifted.macros = f.Optional, f.macros
	}
	for mn, m := range elem.macros {
		s.addMacro(name+"."+mn, &macro{params: m.params, expand: m.expand, prefix: name + "." + m.prefix})
	}
	return s
}

// liftBinding turns a binding over E into one over T by reading E from T.
func liftBinding[T, E any](get func(T) E, b binding[E]) binding[T] {
	out := binding[T]{
		bl: lift(get, b.bl), num: lift(get, b.num), str: lift(get, b.str),
		nums: lift(get, b.nums), strs: lift(get, b.strs), date: lift(get, b.date), length: lift(get, b.length),
	}
	if p := b.present; p != nil {
		out.present = func(v T) bool { return p(get(v)) }
	}
	if q := b.quant; q != nil {
		out.quant = func(op quantifier, chk *Checked, pred rule.Cond) Condition[T] {
			switch c := q(op, chk, pred).(type) {
			case constCond[E]:
				return constCond[T](c)
			default:
				return CondFunc[T](func(v T) (bool, error) { return c.Match(get(v)) })
			}
		}
	}
	return out
}

func lift[T, E, V any](get func(T) E, e Expr[E, V]) Expr[T, V] {
	switch e := e.(type) {
	case nil:
		return nil
	case fieldExpr[E, V]:
		// Keep infallible accessors infallible.
		return fieldExpr[T, V](func(v T) V { return e(get(v)) })
	}
	return ExprFunc[T, V](func(v T) (V, error) { return e.Eval(get(v)) })
}

func quantOver[T, E any](items Expr[T, []E], elem *Schema[E]) func(quantifier, *Checked, rule.Cond) Condition[T] {
	return func(q quantifier, chk *Checked, pred rule.Cond) Condition[T] {
		return newQuant(q, items, elem.cond(chk, pred))
	}
}

// Check validates c against the schema. All configuration errors are
// reported here as *CompileError carrying the JSON path of the bad node.
// The result is the input of Compile and of query translators.
func (s *Schema[T]) Check(c rule.Cond) (*Checked, error) {
	return check(scope{s.fields, s.macros}, c)
}

// CheckJSON is rule.Parse followed by Check.
func (s *Schema[T]) CheckJSON(data []byte) (*Checked, error) {
	c, err := rule.Parse(data)
	if err != nil {
		return nil, err
	}
	return s.Check(c)
}

// Compile checks c and builds an executable in-memory condition.
func (s *Schema[T]) Compile(c rule.Cond) (Condition[T], error) {
	chk, err := s.Check(c)
	if err != nil {
		return nil, err
	}
	return s.Build(chk), nil
}

// CompileJSON is rule.Parse followed by Compile.
func (s *Schema[T]) CompileJSON(data []byte) (Condition[T], error) {
	c, err := rule.Parse(data)
	if err != nil {
		return nil, err
	}
	return s.Compile(c)
}

// Build turns a rule checked against this schema into an executable
// condition. It panics if chk was produced by a different schema.
func (s *Schema[T]) Build(chk *Checked) Condition[T] {
	return s.cond(chk, chk.Root())
}

// cond and expr turn checked nodes into executable operands. Check has
// already enforced every invariant relied on here.
func (s *Schema[T]) cond(chk *Checked, n rule.Cond) Condition[T] {
	conds := func(items []rule.Cond) []Condition[T] {
		out := make([]Condition[T], len(items))
		for i, it := range items {
			out[i] = s.cond(chk, it)
		}
		return out
	}
	switch n := n.(type) {
	case *rule.And:
		return And(conds(n.Items)...)
	case *rule.Or:
		return Or(conds(n.Items)...)
	case *rule.Not:
		return Not(s.cond(chk, n.X))
	case *rule.Quant:
		return s.field(chk, n.X).quant(quantOf[n.Op], chk, n.Y)
	case *rule.Exists:
		if p := s.field(chk, n.X).present; p != nil {
			return CondFunc[T](func(v T) (bool, error) { return p(v), nil })
		}
		return True[T]()
	case *rule.Compare:
		guards := s.guards(chk, nil, n.X, n.Y)
		if len(guards) == 0 {
			return s.compare(chk, n, n.Op)
		}
		// Test presence first so accessors never see a missing value, and
		// negate the guarded positive form so ne/nin hold on missing values.
		op, neg := n.Op, false
		if pos, ok := negated[op]; ok {
			op, neg = pos, true
		}
		c := And(append(guards, s.compare(chk, n, op))...)
		if neg {
			return Not(c)
		}
		return c
	}
	panic(fmt.Sprintf("condition: unchecked node %T", n))
}

// negated maps ne and nin to the operator they negate.
var negated = map[rule.Op]rule.Op{rule.OpNe: rule.OpEq, rule.OpNin: rule.OpIn}

func (s *Schema[T]) compare(chk *Checked, n *rule.Compare, op rule.Op) Condition[T] {
	x, y := s.expr(chk, n.X), s.expr(chk, n.Y)
	switch n.Type {
	case rule.TypeBoolean:
		return newEq(op == rule.OpNe, x.bl, y.bl)
	case rule.TypeNumber:
		return compare(op, x.num, y.num, y.nums, n.Y)
	case rule.TypeDate:
		return newDate(dateOf[op], x.date, y.date)
	default:
		if sop, ok := strOf[op]; ok {
			return newStr(sop, x.str, y.str)
		}
		return compare(op, x.str, y.str, y.strs, n.Y)
	}
}

// guards returns a presence test for every optional scalar field the
// operands read. Lists need none: a missing list reads as empty, and Len is
// zero.
func (s *Schema[T]) guards(chk *Checked, out []Condition[T], es ...rule.Expr) []Condition[T] {
	for _, e := range es {
		switch e := e.(type) {
		case *rule.Field:
			if p := s.field(chk, e).present; p != nil && !chk.Field(e).Kind.IsList() {
				out = append(out, CondFunc[T](func(v T) (bool, error) { return p(v), nil }))
			}
		case *rule.Arith:
			out = s.guards(chk, out, e.X, e.Y)
		}
	}
	return out
}

// compare builds eq/ne/order/in/nin for a scalar type V.
func compare[T any, V ordered](op rule.Op, x, y Expr[T, V], list Expr[T, []V], yNode rule.Expr) Condition[T] {
	switch op {
	case rule.OpEq, rule.OpNe:
		return newEq(op == rule.OpNe, x, y)
	case rule.OpIn, rule.OpNin:
		neg := op == rule.OpNin
		if _, isField := yNode.(*rule.Field); !isField {
			vals, _ := list.Eval(*new(T))
			return newIn(neg, x, vals)
		}
		if neg {
			return NotInList(x, list)
		}
		return InList(x, list)
	}
	return newOrd(ordOf[op], x, y)
}

// ordered is the set of scalar operand types that support every
// comparison operator.
type ordered interface{ ~float64 | ~string }

func (s *Schema[T]) field(chk *Checked, f *rule.Field) binding[T] {
	b, ok := s.bind[chk.Field(f)]
	if !ok {
		panic("condition: rule was checked against a different schema")
	}
	return b
}

func (s *Schema[T]) expr(chk *Checked, e rule.Expr) binding[T] {
	switch e := e.(type) {
	case *rule.Field:
		return s.field(chk, e)
	case *rule.Number:
		return binding[T]{num: Const[T](e.V)}
	case *rule.String:
		return binding[T]{str: Const[T](e.V)}
	case *rule.Bool:
		return binding[T]{bl: Const[T](e.V)}
	case *rule.Date:
		return binding[T]{date: Const[T](e.V)}
	case *rule.Numbers:
		return binding[T]{nums: Const[T](e.V)}
	case *rule.Strings:
		return binding[T]{strs: Const[T](e.V)}
	case *rule.Arith:
		return binding[T]{num: newArith(arithOf[e.Op], s.expr(chk, e.X).num, s.expr(chk, e.Y).num)}
	case *rule.Len:
		l := s.field(chk, e.X).length
		return binding[T]{num: ExprFunc[T, float64](func(v T) (float64, error) {
			n, err := l.Eval(v)
			return float64(n), err
		})}
	}
	panic(fmt.Sprintf("condition: unchecked node %T", e))
}

// Explicit mappings from rule operators to the evaluator's internal enums.
var (
	ordOf   = map[rule.Op]ordOp{rule.OpLt: opLt, rule.OpLte: opLe, rule.OpGt: opGt, rule.OpGte: opGe}
	strOf   = map[rule.Op]strOp{rule.OpContains: opContains, rule.OpStartsWith: opPrefix, rule.OpEndsWith: opSuffix}
	quantOf = map[rule.Op]quantifier{rule.OpAny: qAny, rule.OpAll: qAll, rule.OpNone: qNone}
	arithOf = map[rule.Op]arithOp{rule.OpAdd: opAdd, rule.OpSub: opSub, rule.OpMul: opMul, rule.OpDiv: opDiv}
	dateOf  = map[rule.Op]func(int) bool{
		rule.OpEq: func(c int) bool { return c == 0 }, rule.OpNe: func(c int) bool { return c != 0 },
		rule.OpLt: func(c int) bool { return c < 0 }, rule.OpLte: func(c int) bool { return c <= 0 },
		rule.OpGt: func(c int) bool { return c > 0 }, rule.OpGte: func(c int) bool { return c >= 0 },
	}
)
