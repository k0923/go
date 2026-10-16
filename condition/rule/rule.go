// Package rule is the declarative, JSON-serialisable form of a condition.
//
// A rule is a tree of two closed families of node types. Conditions yield a
// bool, expressions yield a value:
//
//	Cond  *And{Items}  *Or{Items}  *Not{X}
//	      *Compare{Type, X, Op, Y}         X op Y on string/number/boolean
//	      *Quant{X, Op, Y}                 any/all/none element of list X satisfies Y
//	Expr  *Field{Name}
//	      *Number *String *Bool *Numbers *Strings   literals
//	      *Arith{X, Op, Y}                 add/sub/mul/div
//	      *Len{X}                          element count of a list field
//
// The JSON form is flat, one object per node, discriminated by "type":
//
//	{"type":"and","items":[C, ...]}           {"type":"or","items":[...]}
//	{"type":"not","x":C}
//	{"type":"number","x":E,"opt":"gte","y":E} types: string, number, boolean
//	{"type":"array","x":"items","opt":"any","y":C}
//	{"type":"field","name":"amount"}
//	{"type":"literal","value":100}
//	{"type":"arith","x":E,"opt":"sub","y":E}
//	{"type":"len","x":"items"}
//
// Two shorthands keep common rules short:
//
//   - in an x position, a JSON string is a field: "x":"amount";
//   - in a y position, a JSON scalar or array is a literal: "y":100.
//
// Anything else is written as an explicit object, so comparing two fields is
// {"type":"number","x":"amount","opt":"gt","y":{"type":"field","name":"limit"}}
// and an expression on the left is
// {"type":"number","x":{"type":"len","x":"items"},"opt":"lte","y":20}.
//
// The type of a literal comes from where it appears, never from the JSON
// value alone: the y of a string comparison is a string, the y of in/nin is a
// list of the comparison type, arithmetic operands are numbers. An empty list
// therefore has a definite type too.
//
// Parse only checks structure. Field names and kinds are checked against a
// schema by condition.Schema.Check.
package rule

// Node is implemented by every Cond and Expr.
type Node interface{ node() }

// Cond is a node that yields a bool. The implementations are exactly the
// condition types of this package.
type Cond interface {
	Node
	cond()
}

// Expr is a node that yields a value. The implementations are exactly the
// expression types of this package.
type Expr interface {
	Node
	expr()
}

type (
	// And is true when every item is; an empty And is true.
	And struct{ Items []Cond }
	// Or is true when some item is; an empty Or is false.
	Or struct{ Items []Cond }
	// Not negates X.
	Not struct{ X Cond }
	// Compare applies Op to X and Y, both of type Type. For in/nin, Y is a
	// list of Type.
	Compare struct {
		Type Type
		X    Expr
		Op   Op
		Y    Expr
	}
	// Quant applies the quantifier Op (any, all, none) to the elements of the
	// list field X. Fields in Y refer to the element; the element of a scalar
	// list is the field "$".
	Quant struct {
		X  *Field
		Op Op
		Y  Cond
	}
)

type (
	// Field references a schema field, relative to the current list element
	// inside a Quant.
	Field struct{ Name string }
	// Number is a number literal.
	Number struct{ V float64 }
	// String is a string literal.
	String struct{ V string }
	// Bool is a bool literal.
	Bool struct{ V bool }
	// Numbers is a number list literal, the y of a number in/nin.
	Numbers struct{ V []float64 }
	// Strings is a string list literal, the y of a string in/nin.
	Strings struct{ V []string }
	// Arith applies Op (add, sub, mul, div) to two numbers.
	Arith struct {
		X  Expr
		Op Op
		Y  Expr
	}
	// Len is the element count of the list field X.
	Len struct{ X *Field }
)

func (*And) node()     {}
func (*Or) node()      {}
func (*Not) node()     {}
func (*Compare) node() {}
func (*Quant) node()   {}
func (*Field) node()   {}
func (*Number) node()  {}
func (*String) node()  {}
func (*Bool) node()    {}
func (*Numbers) node() {}
func (*Strings) node() {}
func (*Arith) node()   {}
func (*Len) node()     {}

func (*And) cond()     {}
func (*Or) cond()      {}
func (*Not) cond()     {}
func (*Compare) cond() {}
func (*Quant) cond()   {}

func (*Field) expr()   {}
func (*Number) expr()  {}
func (*String) expr()  {}
func (*Bool) expr()    {}
func (*Numbers) expr() {}
func (*Strings) expr() {}
func (*Arith) expr()   {}
func (*Len) expr()     {}

// Walk calls fn for n and every node below it, depth first. Nil children are
// skipped.
func Walk(n Node, fn func(Node)) {
	if n == nil {
		return
	}
	fn(n)
	switch n := n.(type) {
	case *And:
		for _, c := range n.Items {
			Walk(c, fn)
		}
	case *Or:
		for _, c := range n.Items {
			Walk(c, fn)
		}
	case *Not:
		Walk(n.X, fn)
	case *Compare:
		Walk(n.X, fn)
		Walk(n.Y, fn)
	case *Quant:
		if n.X != nil {
			Walk(n.X, fn)
		}
		Walk(n.Y, fn)
	case *Arith:
		Walk(n.X, fn)
		Walk(n.Y, fn)
	case *Len:
		if n.X != nil {
			Walk(n.X, fn)
		}
	}
}
