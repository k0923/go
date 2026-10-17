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
//  2. Declarative rules (package rule) checked against a Schema. A rule is a
//     tree of closed node types — And, Or, Not, Compare, Quant, Exists,
//     Call for conditions; Field, literals, Arith, Len for values — with a
//     flat JSON form such as {"type":"number","x":"amount","opt":"gte","y":100}. The
//     left operand may be any expression, not only a field. All validation —
//     unknown types, opt values, fields, kind mismatches, constant division
//     by zero, nesting depth — happens in rule.Parse and Schema.Check and is
//     reported as a *CompileError carrying the JSON path of the bad node:
//
//     JSON ─rule.Parse─> rule.Cond ─Schema.Check─> *Checked ─┬─ Schema.Build     -> Condition[T] (Match)
//     ├─ sqlquery.Where   -> SQL WHERE + args
//     └─ mongoquery.Filter -> MongoDB filter
//
//     Every consumer reads the same *Checked, so evaluation and generated
//     queries cannot disagree about what a rule means. New backends (ES,
//     ClickHouse, ...) are written the same way: a type switch over the rule
//     node types, with Checked.Field for the resolved field of a reference.
//     No node carries an untyped value: literals are *rule.Number,
//     *rule.Strings and so on, decoded according to the comparison type.
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
//   - Fields marked with Schema.Optional may be missing. A comparison that
//     reads a missing scalar is false; ne and nin, being the negations of eq
//     and in, are true; exists is false. A missing list is empty. Every
//     backend implements the same two-valued semantics.
//   - Dates compare as instants: the same moment in two time zones is equal.
//     Date literals in JSON are RFC 3339 strings.
//   - A Call invokes a macro registered with Schema.Macro. Check replaces
//     it with the macro's expansion, so a *Checked never contains a Call
//     and backends need no support for macros.
//   - A list of lists is a List whose element schema registers the inner
//     list as SelfField ("$"); rules nest array rules over "$".
//
// Every Condition and Expr in this package is immutable after construction and
// safe for concurrent use. A Schema must be fully registered before it is used
// to compile, and must not be mutated concurrently with Compile.
package condition
