package rule

// Type is the operand type of a comparison: the "type" of
// {"type":"number","x":...,"opt":"gt","y":...}. It decides which opt values
// are allowed and how the literals in x and y are decoded.
type Type uint8

const (
	TypeString Type = iota + 1
	TypeNumber
	TypeBoolean
)

var typeNames = map[Type]string{TypeString: "string", TypeNumber: "number", TypeBoolean: "boolean"}

func (t Type) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return "invalid"
}

// Op is the "opt" of a comparison, quantifier or arithmetic node.
type Op uint8

const (
	OpEq         Op = iota + 1 // eq
	OpNe                       // ne
	OpGt                       // gt
	OpGte                      // gte
	OpLt                       // lt
	OpLte                      // lte
	OpIn                       // in: y is a list
	OpNin                      // nin: y is a list
	OpContains                 // contains
	OpStartsWith               // starts_with
	OpEndsWith                 // ends_with
	OpAny                      // any (array)
	OpAll                      // all (array)
	OpNone                     // none (array)
	OpAdd                      // add (arith)
	OpSub                      // sub (arith)
	OpMul                      // mul (arith)
	OpDiv                      // div (arith)
	opEnd
)

var opNames = [opEnd]string{
	OpEq: "eq", OpNe: "ne", OpGt: "gt", OpGte: "gte", OpLt: "lt", OpLte: "lte",
	OpIn: "in", OpNin: "nin",
	OpContains: "contains", OpStartsWith: "starts_with", OpEndsWith: "ends_with",
	OpAny: "any", OpAll: "all", OpNone: "none",
	OpAdd: "add", OpSub: "sub", OpMul: "mul", OpDiv: "div",
}

var opByName = func() map[string]Op {
	m := make(map[string]Op, len(opNames))
	for _, op := range Ops() {
		m[opNames[op]] = op
	}
	return m
}()

// String returns the JSON name of o, or "invalid".
func (o Op) String() string {
	if o == 0 || o >= opEnd {
		return "invalid"
	}
	return opNames[o]
}

// ParseOp maps a JSON name such as "starts_with" to its Op.
func ParseOp(name string) (Op, bool) {
	op, ok := opByName[name]
	return op, ok
}

// Ops lists every operator. Backend tests range over it to make sure a newly
// added operator is not silently left untranslated.
func Ops() []Op {
	out := make([]Op, 0, opEnd-1)
	for op := OpEq; op < opEnd; op++ {
		out = append(out, op)
	}
	return out
}

// Allows reports whether a comparison of type t accepts op.
//
//	string   eq ne gt gte lt lte in nin contains starts_with ends_with
//	number   eq ne gt gte lt lte in nin
//	boolean  eq ne
func (t Type) Allows(op Op) bool {
	switch t {
	case TypeString:
		return op >= OpEq && op <= OpEndsWith
	case TypeNumber:
		return op >= OpEq && op <= OpNin
	case TypeBoolean:
		return op == OpEq || op == OpNe
	}
	return false
}

// IsList reports whether op takes a list as its y operand (in, nin).
func (o Op) IsList() bool { return o == OpIn || o == OpNin }

// IsQuant reports whether op is a quantifier (any, all, none).
func (o Op) IsQuant() bool { return o >= OpAny && o <= OpNone }

// IsArith reports whether op is arithmetic (add, sub, mul, div).
func (o Op) IsArith() bool { return o >= OpAdd && o <= OpDiv }
