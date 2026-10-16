package condition

import "fmt"

// Schema declares the fields a rule may reference and binds each of them to
// an accessor on T. The same Schema serves two purposes:
//
//   - Check turns a Node into a *TypedNode, the backend-neutral form consumed
//     by query translators (see the sqlquery and mongoquery packages);
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
	cond   Condition[T]
	num    Expr[T, float64]
	str    Expr[T, string]
	nums   Expr[T, []float64]
	strs   Expr[T, []string]
	length Expr[T, int]
	// quant builds any/all/none with a predicate already checked against the
	// element schema; it captures the element type E at registration time.
	quant func(q quantifier, pred *TypedNode) Condition[T]
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

// Bool registers a boolean field. It is usable directly as a condition.
func (s *Schema[T]) Bool(name string, get func(T) bool) *Schema[T] {
	mustGetter(name, get == nil)
	return s.add(name, KindBool, nil, binding[T]{cond: CondFunc[T](func(v T) (bool, error) { return get(v), nil })})
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

// Strings registers a string list field. Inside any/all/none the element is
// referenced as {"field":"$"}.
func (s *Schema[T]) Strings(name string, get func(T) []string) *Schema[T] {
	mustGetter(name, get == nil)
	items := Field(get)
	elem := NewSchema[string]().String(SelfField, func(v string) string { return v })
	return s.add(name, KindStrings, elem.fields, binding[T]{strs: items, length: Len(items), quant: quantOver(items, elem)})
}

// Numbers registers a number list field. Inside any/all/none the element is
// referenced as {"field":"$"}.
func (s *Schema[T]) Numbers(name string, get func(T) []float64) *Schema[T] {
	mustGetter(name, get == nil)
	items := Field(get)
	elem := NewSchema[float64]().Number(SelfField, func(v float64) float64 { return v })
	return s.add(name, KindNumbers, elem.fields, binding[T]{nums: items, length: Len(items), quant: quantOver(items, elem)})
}

// List registers a list of objects whose fields are described by elem. It is
// the operand of any/all/none and len. elem may be s itself for recursive
// structures.
func (s *Schema[T]) List[E any](name string, get func(T) []E, elem *Schema[E]) *Schema[T] {
	mustGetter(name, get == nil)
	if elem == nil {
		panic("condition: nil element schema")
	}
	items := Field(get)
	return s.add(name, KindObjects, elem.fields, binding[T]{length: Len(items), quant: quantOver(items, elem)})
}

func quantOver[T, E any](items Expr[T, []E], elem *Schema[E]) func(quantifier, *TypedNode) Condition[T] {
	return func(q quantifier, pred *TypedNode) Condition[T] {
		return newQuant(q, items, elem.build(pred).cond)
	}
}

// Check validates n and returns its typed form. All configuration errors are
// reported here as *CompileError carrying the JSON path of the bad node.
// The result is the input for query translators.
func (s *Schema[T]) Check(n Node) (*TypedNode, error) {
	t, err := check(s.fields, n, "$", 0)
	if err != nil {
		return nil, err
	}
	if t.Kind != KindBool {
		return nil, errAt(t.Path, ErrTypeMismatch, "want bool, got %s", t.Kind)
	}
	return t, nil
}

// CheckJSON is ParseNode followed by Check.
func (s *Schema[T]) CheckJSON(data []byte) (*TypedNode, error) {
	n, err := ParseNode(data)
	if err != nil {
		return nil, err
	}
	return s.Check(n)
}

// Compile checks n and builds an executable in-memory condition.
func (s *Schema[T]) Compile(n Node) (Condition[T], error) {
	t, err := s.Check(n)
	if err != nil {
		return nil, err
	}
	return s.build(t).cond, nil
}

// CompileJSON is ParseNode followed by Compile.
func (s *Schema[T]) CompileJSON(data []byte) (Condition[T], error) {
	n, err := ParseNode(data)
	if err != nil {
		return nil, err
	}
	return s.Compile(n)
}

// build turns a node produced by Check against this schema into executable
// operands. Check has already enforced every invariant relied on here.
func (s *Schema[T]) build(t *TypedNode) binding[T] {
	if t.Field != nil {
		return s.bind[t.Field]
	}
	if t.IsLiteral() {
		return literalBinding[T](t)
	}
	args := func() []binding[T] {
		out := make([]binding[T], len(t.Args))
		for i, a := range t.Args {
			out[i] = s.build(a)
		}
		return out
	}

	switch op := t.Op; {
	case op == OpAnd || op == OpOr:
		vs := args()
		conds := make([]Condition[T], len(vs))
		for i, v := range vs {
			conds[i] = v.cond
		}
		if op == OpAnd {
			return binding[T]{cond: And(conds...)}
		}
		return binding[T]{cond: Or(conds...)}

	case op == OpNot:
		return binding[T]{cond: Not(s.build(t.Args[0]).cond)}

	case op == OpEq || op == OpNe:
		vs, neg := args(), op == OpNe
		switch t.Args[0].Kind {
		case KindNumber:
			return binding[T]{cond: newEq(neg, vs[0].num, vs[1].num)}
		case KindString:
			return binding[T]{cond: newEq(neg, vs[0].str, vs[1].str)}
		default:
			return binding[T]{cond: newEq(neg, condAsExpr(vs[0].cond), condAsExpr(vs[1].cond))}
		}

	case op.isOrder():
		vs := args()
		if t.Args[0].Kind == KindNumber {
			return binding[T]{cond: newOrd(ordOf[op], vs[0].num, vs[1].num)}
		}
		return binding[T]{cond: newOrd(ordOf[op], vs[0].str, vs[1].str)}

	case op.isString():
		vs := args()
		return binding[T]{cond: newStr(strOf[op], vs[0].str, vs[1].str)}

	case op == OpIn || op == OpNotIn:
		vs, neg := args(), op == OpNotIn
		list := t.Args[1]
		switch {
		case list.Kind == KindEmptyList:
			return binding[T]{cond: constCond[T](neg)}
		case t.Args[0].Kind == KindNumber:
			return binding[T]{cond: inBinding(neg, vs[0].num, vs[1].nums, list.Lit)}
		default:
			return binding[T]{cond: inBinding(neg, vs[0].str, vs[1].strs, list.Lit)}
		}

	case op.isQuant():
		return binding[T]{cond: s.build(t.Args[0]).quant(quantOf[op], t.Args[1])}

	case op.isArith():
		vs := args()
		return binding[T]{num: newArith(arithOf[op], vs[0].num, vs[1].num)}

	default: // OpLen
		if t.Args[0].Kind == KindEmptyList {
			return binding[T]{num: Const[T](0.0)}
		}
		l := s.build(t.Args[0]).length
		if c, ok := l.(constExpr[T, int]); ok {
			return binding[T]{num: Const[T](float64(c.v))}
		}
		return binding[T]{num: ExprFunc[T, float64](func(v T) (float64, error) {
			n, err := l.Eval(v)
			return float64(n), err
		})}
	}
}

func literalBinding[T any](t *TypedNode) binding[T] {
	l := t.Lit
	switch l.Kind() {
	case KindBool:
		return binding[T]{cond: constCond[T](l.Bool())}
	case KindNumber:
		return binding[T]{num: Const[T](l.Number())}
	case KindString:
		return binding[T]{str: Const[T](l.String())}
	case KindNumbers:
		return binding[T]{nums: Const[T](l.Numbers()), length: Const[T](l.Len())}
	case KindStrings:
		return binding[T]{strs: Const[T](l.Strings()), length: Const[T](l.Len())}
	}
	return binding[T]{length: Const[T](0)} // KindEmptyList
}

// condAsExpr lets a condition take part in eq/ne as a bool operand.
func condAsExpr[T any](c Condition[T]) Expr[T, bool] {
	if k, ok := c.(constCond[T]); ok {
		return Const[T](bool(k))
	}
	return ExprFunc[T, bool](c.Match)
}

// inBinding uses the hashed In for literal lists and InList otherwise.
func inBinding[T any, V comparable](neg bool, x Expr[T, V], list Expr[T, []V], lit *Literal) Condition[T] {
	if lit != nil {
		vals, _ := list.Eval(*new(T))
		return newIn(neg, x, vals)
	}
	if neg {
		return NotInList(x, list)
	}
	return InList(x, list)
}
