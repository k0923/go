// Package mongoquery translates a checked rule (condition.Checked) into
// a MongoDB filter document, so the same rule that condition evaluates in
// memory can also be pushed down to MongoDB.
//
// The output uses only maps, slices and scalars, so it can be passed directly
// to the official driver (Find(ctx, filter)) or marshalled with encoding/json.
//
// Comparisons of a field with a literal become native query operators
// ($eq, $in, $regex, $elemMatch, ...) so indexes can be used. Anything that
// needs two fields, arithmetic or len falls back to $expr, which is correct
// but cannot use indexes. When a quantifier predicate needs $expr, the whole
// quantifier switches to aggregation form, because $elemMatch rejects $expr.
//
// Semantic alignment with in-memory evaluation:
//
//   - Documents are assumed to hold every registered field with the right
//     BSON type, except optional fields (condition.Schema.Optional), which
//     may be missing or null; both count as missing. A comparison with a
//     missing operand is false, ne/nin true, exists false. A missing or null
//     list field behaves like an empty list.
//   - Dates are emitted as time.Time, which the driver encodes as BSON dates.
//   - String operators are byte-wise and case-sensitive, like Go.
//   - Division by zero aborts the query with an error, like Match.
package mongoquery

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/k0923/go/condition"
	"github.com/k0923/go/condition/rule"
)

// ErrUnsupported is returned for nodes that cannot be translated.
var ErrUnsupported = errors.New("mongoquery: unsupported")

// Options configure the translation.
type Options struct {
	// Paths maps schema field names to document paths. Element fields are
	// keyed by "<list>.<field>" and map to a path relative to the element,
	// e.g. {"items": "lines", "items.price": "unit_price"}. Unmapped fields
	// use their schema name.
	Paths map[string]string
	// Prefix is prepended, with a dot, to every top-level document path,
	// including paths from Paths. Paths inside list elements are relative to
	// the element and are not prefixed. With Prefix "data", the field
	// "level" becomes "data.level" and the list "items" "data.items".
	Prefix string
}

// Filter translates a checked rule into a MongoDB filter document.
func Filter(chk *condition.Checked, opt Options) (map[string]any, error) {
	if chk == nil {
		return nil, fmt.Errorf("mongoquery: nil rule")
	}
	tr := &translator{opt: opt, chk: chk}
	return tr.filter(chk.Root())
}

// filter renders a root-level predicate, preferring native operators and
// wrapping only the parts that need aggregation in $expr.
func (tr *translator) filter(n rule.Cond) (map[string]any, error) {
	if f, ok, err := tr.query(n, scope{}); err != nil || ok {
		return f, err
	}
	group := func(op string, items []rule.Cond) (map[string]any, error) {
		parts := make([]any, len(items))
		for i, it := range items {
			f, err := tr.filter(it)
			if err != nil {
				return nil, err
			}
			parts[i] = f
		}
		return doc(op, parts), nil
	}
	switch n := n.(type) {
	case *rule.And:
		return group("$and", n.Items)
	case *rule.Or:
		return group("$or", n.Items)
	case *rule.Not:
		f, err := tr.filter(n.X)
		return doc("$nor", []any{f}), err
	}
	e, err := tr.cond(n, scope{})
	if err != nil {
		return nil, err
	}
	return doc("$expr", e), nil
}

// scope tracks where field references resolve.
//
//	key:     prefix for Options.Paths lookups ("" at the root, "items." inside)
//	elemVar: aggregation variable bound to the element ("" outside $map)
type scope struct {
	key     string
	elemVar string
}

type translator struct {
	opt  Options
	chk  *condition.Checked
	vars int
}

func doc(k string, v any) map[string]any { return map[string]any{k: v} }

func (tr *translator) unsupported(n rule.Node, format string, a ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrUnsupported, tr.chk.Path(n), fmt.Sprintf(format, a...))
}

func (tr *translator) path(f *rule.Field, sc scope) string {
	p, ok := tr.opt.Paths[sc.key+f.Name]
	if !ok {
		p = f.Name
	}
	if sc.key == "" && tr.opt.Prefix != "" {
		return tr.opt.Prefix + "." + p
	}
	return p
}

// lit renders a literal as a BSON-compatible value. Integral numbers become
// int64 so they print and compare naturally. ok is false for non-literals.
func lit(e rule.Expr) (v any, ok bool) {
	switch e := e.(type) {
	case *rule.Bool:
		return e.V, true
	case *rule.Date:
		return e.V, true
	case *rule.Number:
		return num(e.V), true
	case *rule.String:
		return e.V, true
	case *rule.Numbers:
		out := make([]any, len(e.V))
		for i, x := range e.V {
			out[i] = num(x)
		}
		return out, true
	case *rule.Strings:
		out := make([]any, len(e.V))
		for i, x := range e.V {
			out[i] = x
		}
		return out, true
	}
	return nil, false
}

func num(f float64) any {
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return int64(f)
	}
	return f
}

var (
	cmpOps = map[rule.Op]string{rule.OpEq: "$eq", rule.OpNe: "$ne", rule.OpLt: "$lt", rule.OpLte: "$lte", rule.OpGt: "$gt", rule.OpGte: "$gte"}
	// flipped is cmpOps with the operands swapped (literal op field).
	flipped  = map[rule.Op]string{rule.OpEq: "$eq", rule.OpNe: "$ne", rule.OpLt: "$gt", rule.OpLte: "$gte", rule.OpGt: "$lt", rule.OpGte: "$lte"}
	inOps    = map[rule.Op]string{rule.OpIn: "$in", rule.OpNin: "$nin"}
	arithOps = map[rule.Op]string{rule.OpAdd: "$add", rule.OpSub: "$subtract", rule.OpMul: "$multiply", rule.OpDiv: "$divide"}
)

var falseFilter = doc("$expr", false)

// docField reports whether e is a field addressable by a native query path,
// that is, not the scalar element "$".
func docField(e rule.Expr) (*rule.Field, bool) {
	f, ok := e.(*rule.Field)
	return f, ok && f.Name != condition.SelfField
}

// query tries to express n in the native query language. ok is false when
// n needs aggregation expressions; the caller then falls back to $expr.
func (tr *translator) query(n rule.Cond, sc scope) (map[string]any, bool, error) {
	if sc.elemVar != "" {
		return nil, false, nil
	}
	group := func(op string, items []rule.Cond, empty map[string]any) (map[string]any, bool, error) {
		if len(items) == 0 {
			return empty, true, nil
		}
		parts := make([]any, len(items))
		for i, it := range items {
			f, ok, err := tr.query(it, sc)
			if err != nil || !ok {
				return nil, ok, err
			}
			parts[i] = f
		}
		if len(parts) == 1 {
			return parts[0].(map[string]any), true, nil
		}
		return doc(op, parts), true, nil
	}

	switch n := n.(type) {
	case *rule.And:
		return group("$and", n.Items, map[string]any{})
	case *rule.Or:
		return group("$or", n.Items, falseFilter)
	case *rule.Not:
		f, ok, err := tr.query(n.X, sc)
		if err != nil || !ok {
			return nil, ok, err
		}
		return doc("$nor", []any{f}), true, nil
	case *rule.Quant:
		if n.X.Name == condition.SelfField {
			// A list nested in a list has no document path of its own.
			return nil, false, nil
		}
		if tr.chk.Field(n.X).Kind != condition.KindObjects {
			return tr.scalarQuant(n, sc)
		}
		elem := scope{key: sc.key + n.X.Name + "."}
		p, ok, err := tr.query(n.Y, elem)
		if err != nil || !ok {
			return nil, ok, err
		}
		path := tr.path(n.X, sc)
		switch n.Op {
		case rule.OpAny:
			return doc(path, doc("$elemMatch", p)), true, nil
		case rule.OpAll: // no element fails the predicate
			return doc(path, doc("$not", doc("$elemMatch", doc("$nor", []any{p})))), true, nil
		default: // no element satisfies it
			return doc(path, doc("$not", doc("$elemMatch", p))), true, nil
		}
	case *rule.Exists:
		if !tr.chk.Field(n.X).Optional {
			return map[string]any{}, true, nil
		}
		f, ok := docField(n.X)
		if !ok {
			return nil, false, nil
		}
		return doc(tr.path(f, sc), doc("$ne", nil)), true, nil
	case *rule.Compare:
		// Native operators already give missing fields the required
		// semantics: $eq/$in/$lt/$regex/... fail, $ne/$nin hold.
		return tr.compare(n, sc)
	}
	return nil, false, nil
}

func (tr *translator) compare(n *rule.Compare, sc scope) (map[string]any, bool, error) {
	switch op := n.Op; {
	case op == rule.OpContains || op == rule.OpStartsWith || op == rule.OpEndsWith:
		x, okX := docField(n.X)
		pat, okY := n.Y.(*rule.String)
		if !okX || !okY {
			return nil, false, nil
		}
		re := regexp.QuoteMeta(pat.V)
		switch op {
		case rule.OpStartsWith:
			re = `\A` + re
		case rule.OpEndsWith:
			re = re + `\z`
		}
		return doc(tr.path(x, sc), doc("$regex", re)), true, nil

	case op.IsList():
		if list, ok := n.Y.(*rule.Field); ok {
			v, isLit := lit(n.X)
			if !isLit {
				return nil, false, nil
			}
			// Array membership: {tags: "x"} matches when any element equals "x".
			f := doc(tr.path(list, sc), doc("$eq", v))
			if op == rule.OpNin {
				f = doc("$nor", []any{f})
			}
			return f, true, nil
		}
		values, _ := lit(n.Y)
		if len(values.([]any)) == 0 {
			if op == rule.OpIn {
				return falseFilter, true, nil
			}
			return map[string]any{}, true, nil
		}
		x, ok := docField(n.X)
		if !ok {
			return nil, false, nil
		}
		return doc(tr.path(x, sc), doc(inOps[op], values)), true, nil
	}
	field, value, op, ok := fieldVsLiteral(n)
	if !ok {
		return nil, false, nil
	}
	return doc(tr.path(field, sc), doc(op, value)), true, nil
}

// scalarQuant handles any/all/none over a scalar list whose predicate is
// "$ op literal": it becomes a native $elemMatch on the array, which can use
// a multikey index. Other predicates fall back to $expr.
func (tr *translator) scalarQuant(n *rule.Quant, sc scope) (map[string]any, bool, error) {
	// all(p) is none(!p); every supported operator has a native complement,
	// so no $not is ever needed inside $elemMatch.
	complement := map[string]string{"$eq": "$ne", "$ne": "$eq", "$lt": "$gte", "$gte": "$lt", "$gt": "$lte", "$lte": "$gt", "$in": "$nin", "$nin": "$in"}
	pred, ok := n.Y.(*rule.Compare)
	if !ok {
		return nil, false, nil
	}
	isSelf := func(e rule.Expr) bool { f, ok := e.(*rule.Field); return ok && f.Name == condition.SelfField }
	var op string
	var arg any
	switch {
	case pred.Op.IsList():
		v, isLit := lit(pred.Y)
		if !isSelf(pred.X) || !isLit || len(v.([]any)) == 0 {
			return nil, false, nil
		}
		op, arg = inOps[pred.Op], v
	case cmpOps[pred.Op] != "":
		var x rule.Expr
		if x, arg, op, ok = selfVsLiteral(pred); !ok || !isSelf(x) {
			return nil, false, nil
		}
	default:
		return nil, false, nil
	}
	path := tr.path(n.X, sc)
	switch n.Op {
	case rule.OpAny:
		return doc(path, doc("$elemMatch", doc(op, arg))), true, nil
	case rule.OpNone:
		return doc(path, doc("$not", doc("$elemMatch", doc(op, arg)))), true, nil
	default:
		return doc(path, doc("$not", doc("$elemMatch", doc(complement[op], arg)))), true, nil
	}
}

// selfVsLiteral normalises "x op literal" and "literal op x" for the six
// comparison operators.
func selfVsLiteral(n *rule.Compare) (x rule.Expr, value any, op string, ok bool) {
	if cmpOps[n.Op] == "" {
		return nil, nil, "", false
	}
	if v, isLit := lit(n.Y); isLit {
		if _, isField := n.X.(*rule.Field); isField {
			return n.X, v, cmpOps[n.Op], true
		}
	}
	if v, isLit := lit(n.X); isLit {
		if _, isField := n.Y.(*rule.Field); isField {
			return n.Y, v, flipped[n.Op], true
		}
	}
	return nil, nil, "", false
}

// fieldVsLiteral is selfVsLiteral restricted to document fields.
func fieldVsLiteral(n *rule.Compare) (*rule.Field, any, string, bool) {
	x, v, op, ok := selfVsLiteral(n)
	if !ok {
		return nil, nil, "", false
	}
	f, ok := docField(x)
	return f, v, op, ok
}

// cond translates a condition into an aggregation expression (for $expr).
func (tr *translator) cond(n rule.Cond, sc scope) (any, error) {
	group := func(op string, items []rule.Cond, empty bool) (any, error) {
		if len(items) == 0 {
			return empty, nil
		}
		out := make([]any, len(items))
		for i, it := range items {
			e, err := tr.cond(it, sc)
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return doc(op, out), nil
	}
	switch n := n.(type) {
	case *rule.And:
		return group("$and", n.Items, true)
	case *rule.Or:
		return group("$or", n.Items, false)
	case *rule.Not:
		e, err := tr.cond(n.X, sc)
		return doc("$not", []any{e}), err
	case *rule.Quant:
		return tr.quant(n, sc)
	case *rule.Exists:
		if !tr.chk.Field(n.X).Optional {
			return true, nil
		}
		return tr.present(n.X, sc), nil
	case *rule.Compare:
		guards := tr.guards(nil, sc, n.X, n.Y)
		if len(guards) == 0 {
			return tr.compareExpr(n, n.Op, sc)
		}
		// Aggregation orders null below every value and some operators fail
		// on null, so test presence first ($and short-circuits) and negate
		// the guarded positive form for ne/nin.
		op, neg := n.Op, false
		switch op {
		case rule.OpNe:
			op, neg = rule.OpEq, true
		case rule.OpNin:
			op, neg = rule.OpIn, true
		}
		e, err := tr.compareExpr(n, op, sc)
		if err != nil {
			return nil, err
		}
		e = doc("$and", append(guards, e))
		if neg {
			return doc("$not", []any{e}), nil
		}
		return e, nil
	}
	return nil, tr.unsupported(n, "condition %T", n)
}

// present is an aggregation test that f is neither missing nor null.
func (tr *translator) present(f *rule.Field, sc scope) any {
	return doc("$ne", []any{doc("$ifNull", []any{tr.fieldRef(f, sc), nil}), nil})
}

// guards collects presence tests for the optional scalar fields an operand
// reads. Lists need none: a missing list already behaves like an empty one.
func (tr *translator) guards(out []any, sc scope, es ...rule.Expr) []any {
	for _, e := range es {
		switch e := e.(type) {
		case *rule.Field:
			if info := tr.chk.Field(e); info.Optional && !info.Kind.IsList() {
				out = append(out, tr.present(e, sc))
			}
		case *rule.Arith:
			out = tr.guards(out, sc, e.X, e.Y)
		}
	}
	return out
}

func (tr *translator) compareExpr(n *rule.Compare, op rule.Op, sc scope) (any, error) {
	if op.IsList() {
		if l, ok := lit(n.Y); ok && len(l.([]any)) == 0 {
			return op == rule.OpNin, nil
		}
	}
	x, err := tr.expr(n.X, sc)
	if err != nil {
		return nil, err
	}
	y, err := tr.expr(n.Y, sc)
	if err != nil {
		return nil, err
	}
	switch op {
	case rule.OpContains:
		return doc("$gte", []any{doc("$indexOfBytes", []any{x, y}), 0}), nil
	case rule.OpStartsWith:
		return doc("$eq", []any{doc("$indexOfBytes", []any{x, y}), 0}), nil
	case rule.OpEndsWith:
		lx, ly := doc("$strLenBytes", x), doc("$strLenBytes", y)
		tail := doc("$substrBytes", []any{x, doc("$subtract", []any{lx, ly}), ly})
		return doc("$and", []any{doc("$gte", []any{lx, ly}), doc("$eq", []any{tail, y})}), nil
	case rule.OpIn, rule.OpNin:
		in := doc("$in", []any{x, orEmpty(y)})
		if op == rule.OpNin {
			return doc("$not", []any{in}), nil
		}
		return in, nil
	}
	return doc(cmpOps[op], []any{x, y}), nil
}

// orEmpty treats a missing or null list as an empty one.
func orEmpty(list any) any { return doc("$ifNull", []any{list, doc("$literal", []any{})}) }

func (tr *translator) quant(n *rule.Quant, sc scope) (any, error) {
	items, err := tr.expr(n.X, sc)
	if err != nil {
		return nil, err
	}
	tr.vars++
	v := "e" + strconv.Itoa(tr.vars)
	elem := scope{key: sc.key + n.X.Name + ".", elemVar: v}
	pred, err := tr.cond(n.Y, elem)
	if err != nil {
		return nil, err
	}
	mapped := []any{doc("$map", map[string]any{"input": orEmpty(items), "as": v, "in": pred})}
	switch n.Op {
	case rule.OpAny:
		return doc("$anyElementTrue", mapped), nil
	case rule.OpAll:
		return doc("$allElementsTrue", mapped), nil
	default:
		return doc("$not", []any{doc("$anyElementTrue", mapped)}), nil
	}
}

// expr translates a value into an aggregation expression.
func (tr *translator) expr(e rule.Expr, sc scope) (any, error) {
	switch e := e.(type) {
	case *rule.Field:
		return tr.fieldRef(e, sc), nil
	case *rule.String:
		if strings.HasPrefix(e.V, "$") {
			return doc("$literal", e.V), nil // otherwise read as a field path
		}
		return e.V, nil
	case *rule.Numbers, *rule.Strings:
		v, _ := lit(e)
		return doc("$literal", v), nil
	case *rule.Number, *rule.Bool, *rule.Date:
		v, _ := lit(e)
		return v, nil
	case *rule.Arith:
		x, err := tr.expr(e.X, sc)
		if err != nil {
			return nil, err
		}
		y, err := tr.expr(e.Y, sc)
		return doc(arithOps[e.Op], []any{x, y}), err
	case *rule.Len:
		return doc("$size", orEmpty(tr.fieldRef(e.X, sc))), nil
	}
	return nil, tr.unsupported(e, "expression %T", e)
}

func (tr *translator) fieldRef(f *rule.Field, sc scope) string {
	if sc.elemVar != "" {
		if f.Name == condition.SelfField {
			return "$$" + sc.elemVar
		}
		return "$$" + sc.elemVar + "." + tr.path(f, sc)
	}
	return "$" + tr.path(f, sc)
}
