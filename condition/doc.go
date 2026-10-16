// Package condition builds and evaluates typed predicates over Go values.
//
// It offers two entry points that produce the same immutable Condition tree:
//
//  1. A typed builder API for Go code. Operand types are checked by the
//     compiler, so an ordering operator on a bool or a string operator on a
//     number simply does not type-check:
//
//     adult := condition.Ge(condition.Field(func(u User) int { return u.Age }), condition.Const[User](18))
//     rule := condition.And(adult, condition.In(city, "bj", "sh"))
//
//  2. A declarative Node tree (JSON friendly) checked against a Schema. All
//     validation — unknown operators, unknown fields, kind mismatches, arity,
//     constant division by zero, nesting depth — happens once in Check and is
//     reported as a *CompileError carrying the JSON path of the offending node.
//     Check yields a *TypedNode, the backend-neutral intermediate form:
//
//     Node ─Check─> TypedNode ─┬─ Compile            -> Condition[T] (Match)
//     ├─ sqlquery.Where     -> SQL WHERE + args
//     └─ mongoquery.Filter  -> MongoDB filter
//
//     Every consumer reads the same TypedNode, so evaluation and generated
//     queries cannot disagree about what a rule means. New backends (ES,
//     ClickHouse, ...) are written the same way against TypedNode.
//     Match never has to re-validate configuration.
//
//     Node.Value is untyped only because it is the wire format. TypedNode
//     carries an Operator enum and a *Literal whose accessor must match its
//     Kind, so backends never type-assert on any. In Go code, build nodes
//     with Op(OpXxx, ...) and the typed LitXxx constructors.
//
// Semantics worth knowing:
//
//   - And() is true, Or() is false; Any over an empty list is false, All and
//     None are vacuously true. A nil list behaves like an empty one.
//   - And/Or short-circuit left to right and return the first error they meet.
//   - Sub-trees whose result is statically known are folded at build time
//     (for example And(x, False()) becomes False(), In(x) with no values becomes
//     False()). Folded operands are never evaluated, so their errors cannot
//     surface.
//   - Division by zero is an error for both integers and floats.
//   - Values decoded from JSON numbers are float64; integers beyond 2^53 lose
//     precision.
//
// Every Condition and Expr in this package is immutable after construction and
// safe for concurrent use. A Schema must be fully registered before it is used
// to compile, and must not be mutated concurrently with Compile.
package condition
