package condition

import (
	"fmt"

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
}

// binding is an evaluated operand: only the member matching its kind is set
// (plus length and quant for list kinds).
type binding[T any] struct {
	bl     Expr[T, bool]
	num    Expr[T, float64]
	str    Expr[T, string]
	nums   Expr[T, []float64]
	strs   Expr[T, []string]
	length Expr[T, int]
	// quant builds any/all/none with a predicate already checked against the
	// element schema; it captures the element type E at registration time.
	quant func(q quantifier, chk *Checked, pred rule.Cond) Condition[T]
}

// NewSchema returns an empty schema.
func NewSchema[T any]() *Schema[T] {
	return &Schema[T]{fields: map[string]*FieldInfo{}, bind: map[*FieldInfo]binding[T]{}}
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

// List registers a list of objects whose fields are described by elem. It is
// the x of array rules and of len. elem may be s itself for recursive
// structures.
func (s *Schema[T]) List[E any](name string, get func(T) []E, elem *Schema[E]) *Schema[T] {
	mustGetter(name, get == nil)
	if elem == nil {
		panic("condition: nil element schema")
	}
	items := Field(get)
	return s.add(name, KindObjects, elem.fields, binding[T]{length: Len(items), quant: quantOver(items, elem)})
}

func quantOver[T, E any](items Expr[T, []E], elem *Schema[E]) func(quantifier, *Checked, rule.Cond) Condition[T] {
	return func(q quantifier, chk *Checked, pred rule.Cond) Condition[T] {
		return newQuant(q, items, elem.cond(chk, pred))
	}
}

// Check validates c against the schema. All configuration errors are
// reported here as *CompileError carrying the JSON path of the bad node.
// The result is the input of Compile and of query translators.
func (s *Schema[T]) Check(c rule.Cond) (*Checked, error) { return check(s.fields, c) }

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
	case *rule.Compare:
		x, y := s.expr(chk, n.X), s.expr(chk, n.Y)
		switch n.Type {
		case rule.TypeBoolean:
			return newEq(n.Op == rule.OpNe, x.bl, y.bl)
		case rule.TypeNumber:
			return compare(n.Op, x.num, y.num, y.nums, n.Y)
		default:
			if op, ok := strOf[n.Op]; ok {
				return newStr(op, x.str, y.str)
			}
			return compare(n.Op, x.str, y.str, y.strs, n.Y)
		}
	}
	panic(fmt.Sprintf("condition: unchecked node %T", n))
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
)
