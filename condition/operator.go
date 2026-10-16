package condition

// Operator identifies the operation of a TypedNode. Backends switch on these
// constants instead of on strings, so a misspelt operator does not compile.
// The JSON form (Node.Op) uses the names returned by String.
type Operator uint8

const (
	OpAnd        Operator = iota + 1 // and [0..n]
	OpOr                             // or [0..n]
	OpNot                            // not [1]
	OpEq                             // eq [2]
	OpNe                             // ne [2]
	OpLt                             // lt [2]
	OpLe                             // le [2]
	OpGt                             // gt [2]
	OpGe                             // ge [2]
	OpContains                       // contains [2]
	OpStartsWith                     // starts_with [2]
	OpEndsWith                       // ends_with [2]
	OpIn                             // in [2]
	OpNotIn                          // not_in [2]
	OpAny                            // any [2]
	OpAll                            // all [2]
	OpNone                           // none [2]
	OpAdd                            // add [2]
	OpSub                            // sub [2]
	OpMul                            // mul [2]
	OpDiv                            // div [2]
	OpLen                            // len [1]
	opEnd
)

var opNames = [opEnd]string{
	OpAnd: "and", OpOr: "or", OpNot: "not",
	OpEq: "eq", OpNe: "ne", OpLt: "lt", OpLe: "le", OpGt: "gt", OpGe: "ge",
	OpContains: "contains", OpStartsWith: "starts_with", OpEndsWith: "ends_with",
	OpIn: "in", OpNotIn: "not_in",
	OpAny: "any", OpAll: "all", OpNone: "none",
	OpAdd: "add", OpSub: "sub", OpMul: "mul", OpDiv: "div",
	OpLen: "len",
}

var opByName = func() map[string]Operator {
	m := make(map[string]Operator, len(opNames))
	for _, op := range Operators() {
		m[opNames[op]] = op
	}
	return m
}()

// String returns the JSON name of o, or "invalid".
func (o Operator) String() string {
	if o == 0 || o >= opEnd {
		return "invalid"
	}
	return opNames[o]
}

// ParseOperator maps a JSON name such as "starts_with" to its Operator.
func ParseOperator(name string) (Operator, bool) {
	op, ok := opByName[name]
	return op, ok
}

// Operators lists every operator. Backend tests can range over it to make
// sure a newly added operator is not silently left untranslated.
func Operators() []Operator {
	out := make([]Operator, 0, opEnd-1)
	for op := OpAnd; op < opEnd; op++ {
		out = append(out, op)
	}
	return out
}

// Explicit mappings to the evaluator's internal enums.
var (
	ordOf   = [opEnd]ordOp{OpLt: opLt, OpLe: opLe, OpGt: opGt, OpGe: opGe}
	strOf   = [opEnd]strOp{OpContains: opContains, OpStartsWith: opPrefix, OpEndsWith: opSuffix}
	quantOf = [opEnd]quantifier{OpAny: qAny, OpAll: qAll, OpNone: qNone}
	arithOf = [opEnd]arithOp{OpAdd: opAdd, OpSub: opSub, OpMul: opMul, OpDiv: opDiv}
)

func (o Operator) isOrder() bool  { return o >= OpLt && o <= OpGe }
func (o Operator) isString() bool { return o >= OpContains && o <= OpEndsWith }
func (o Operator) isQuant() bool  { return o >= OpAny && o <= OpNone }
func (o Operator) isArith() bool  { return o >= OpAdd && o <= OpDiv }
