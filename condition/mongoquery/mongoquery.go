// Package mongoquery translates a checked condition (condition.TypedNode) into
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
//     BSON type. $ne/$nin also match documents where the field is missing.
//     A missing or null list field behaves like an empty list.
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
}

// Filter translates t into a MongoDB filter document.
func Filter(t *condition.TypedNode, opt Options) (map[string]any, error) {
	if t == nil || t.Kind != condition.KindBool {
		return nil, fmt.Errorf("mongoquery: want a checked bool node")
	}
	tr := &translator{opt: opt}
	return tr.filter(t)
}

// filter renders a root-level predicate, preferring native operators and
// wrapping only the parts that need aggregation in $expr.
func (tr *translator) filter(t *condition.TypedNode) (map[string]any, error) {
	if f, ok, err := tr.query(t, scope{}); err != nil || ok {
		return f, err
	}
	if t.Op == condition.OpAnd || t.Op == condition.OpOr {
		parts := make([]any, len(t.Args))
		for i, a := range t.Args {
			f, err := tr.filter(a)
			if err != nil {
				return nil, err
			}
			parts[i] = f
		}
		return doc(logicOps[t.Op], parts), nil
	}
	if t.Op == condition.OpNot {
		f, err := tr.filter(t.Args[0])
		return doc("$nor", []any{f}), err
	}
	e, err := tr.expr(t, scope{})
	if err != nil {
		return nil, err
	}
	return doc("$expr", e), nil
}

// scope tracks where field references resolve.
//
//	key:    prefix for Options.Paths lookups ("" at the root, "items." inside)
//	elemVar: aggregation variable bound to the element ("" outside $map)
type scope struct {
	key     string
	elemVar string
}

type translator struct {
	opt  Options
	vars int
}

func doc(k string, v any) map[string]any { return map[string]any{k: v} }

func unsupported(t *condition.TypedNode, format string, a ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrUnsupported, t.Path, fmt.Sprintf(format, a...))
}

func (tr *translator) path(f *condition.FieldInfo, sc scope) string {
	if p, ok := tr.opt.Paths[sc.key+f.Name]; ok {
		return p
	}
	return f.Name
}

// lit renders a literal as a BSON-compatible value. Integral numbers become
// int64 so they print and compare naturally.
func lit(l *condition.Literal) any {
	switch l.Kind() {
	case condition.KindBool:
		return l.Bool()
	case condition.KindNumber:
		return num(l.Number())
	case condition.KindString:
		return l.String()
	case condition.KindNumbers:
		out := make([]any, l.Len())
		for i, x := range l.Numbers() {
			out[i] = num(x)
		}
		return out
	case condition.KindStrings:
		out := make([]any, l.Len())
		for i, x := range l.Strings() {
			out[i] = x
		}
		return out
	}
	return []any{} // KindEmptyList
}

func num(f float64) any {
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return int64(f)
	}
	return f
}

var (
	logicOps = map[condition.Operator]string{condition.OpAnd: "$and", condition.OpOr: "$or"}
	cmpOps   = map[condition.Operator]string{condition.OpEq: "$eq", condition.OpNe: "$ne", condition.OpLt: "$lt", condition.OpLe: "$lte", condition.OpGt: "$gt", condition.OpGe: "$gte"}
	// flipped is cmpOps with the operands swapped (literal op field).
	flipped  = map[condition.Operator]string{condition.OpEq: "$eq", condition.OpNe: "$ne", condition.OpLt: "$gt", condition.OpLe: "$gte", condition.OpGt: "$lt", condition.OpGe: "$lte"}
	inOps    = map[condition.Operator]string{condition.OpIn: "$in", condition.OpNotIn: "$nin"}
	arithOps = map[condition.Operator]string{condition.OpAdd: "$add", condition.OpSub: "$subtract", condition.OpMul: "$multiply", condition.OpDiv: "$divide"}
)

var falseFilter = doc("$expr", false)

// query tries to express t in the native query language. ok is false when
// t needs aggregation expressions; the caller then falls back to $expr.
func (tr *translator) query(t *condition.TypedNode, sc scope) (map[string]any, bool, error) {
	if sc.elemVar != "" {
		return nil, false, nil
	}
	switch {
	case t.Field != nil:
		if t.Field.Name == condition.SelfField {
			return nil, false, nil
		}
		return doc(tr.path(t.Field, sc), true), true, nil
	case t.IsLiteral():
		if t.Lit.Bool() {
			return map[string]any{}, true, nil
		}
		return falseFilter, true, nil
	}

	switch t.Op {
	case condition.OpAnd, condition.OpOr:
		if len(t.Args) == 0 {
			if t.Op == condition.OpAnd {
				return map[string]any{}, true, nil
			}
			return falseFilter, true, nil
		}
		parts := make([]any, len(t.Args))
		for i, a := range t.Args {
			f, ok, err := tr.query(a, sc)
			if err != nil || !ok {
				return nil, ok, err
			}
			parts[i] = f
		}
		if len(parts) == 1 {
			return parts[0].(map[string]any), true, nil
		}
		return doc(logicOps[t.Op], parts), true, nil

	case condition.OpNot:
		f, ok, err := tr.query(t.Args[0], sc)
		if err != nil || !ok {
			return nil, ok, err
		}
		return doc("$nor", []any{f}), true, nil

	case condition.OpEq, condition.OpNe, condition.OpLt, condition.OpLe, condition.OpGt, condition.OpGe:
		field, value, op, ok := fieldVsLiteral(t)
		if !ok || field.Field.Name == condition.SelfField {
			return nil, false, nil
		}
		return doc(tr.path(field.Field, sc), doc(op, lit(value.Lit))), true, nil

	case condition.OpContains, condition.OpStartsWith, condition.OpEndsWith:
		x, pat := t.Args[0], t.Args[1]
		if x.Field == nil || x.Field.Name == condition.SelfField || !pat.IsLiteral() {
			return nil, false, nil
		}
		re := regexp.QuoteMeta(pat.Lit.String())
		switch t.Op {
		case condition.OpStartsWith:
			re = `\A` + re
		case condition.OpEndsWith:
			re = re + `\z`
		}
		return doc(tr.path(x.Field, sc), doc("$regex", re)), true, nil

	case condition.OpIn, condition.OpNotIn:
		x, list := t.Args[0], t.Args[1]
		if list.Kind == condition.KindEmptyList {
			if t.Op == condition.OpIn {
				return falseFilter, true, nil
			}
			return map[string]any{}, true, nil
		}
		switch {
		case x.Field != nil && x.Field.Name != condition.SelfField && list.IsLiteral():
			return doc(tr.path(x.Field, sc), doc(inOps[t.Op], lit(list.Lit))), true, nil
		case x.IsLiteral() && list.Field != nil:
			// Array membership: {tags: "x"} matches when any element equals "x".
			f := doc(tr.path(list.Field, sc), doc("$eq", lit(x.Lit)))
			if t.Op == condition.OpNotIn {
				f = doc("$nor", []any{f})
			}
			return f, true, nil
		}
		return nil, false, nil

	case condition.OpAny, condition.OpAll, condition.OpNone:
		list, pred := t.Args[0], t.Args[1]
		if list.Kind != condition.KindObjects {
			return tr.scalarQuant(t, sc)
		}
		elem := scope{key: sc.key + list.Field.Name + "."}
		p, ok, err := tr.query(pred, elem)
		if err != nil || !ok {
			return nil, ok, err
		}
		path := tr.path(list.Field, sc)
		switch t.Op {
		case condition.OpAny:
			return doc(path, doc("$elemMatch", p)), true, nil
		case condition.OpAll: // no element fails the predicate
			return doc(path, doc("$not", doc("$elemMatch", doc("$nor", []any{p})))), true, nil
		default: // no element satisfies it
			return doc(path, doc("$not", doc("$elemMatch", p))), true, nil
		}
	}
	return nil, false, nil
}

// scalarQuant handles any/all/none over a scalar list whose predicate is
// "$ op literal": it becomes a native $elemMatch on the array, which can use
// a multikey index. Other predicates fall back to $expr.
func (tr *translator) scalarQuant(t *condition.TypedNode, sc scope) (map[string]any, bool, error) {
	list, pred := t.Args[0], t.Args[1]
	// all(p) is none(!p); every supported operator has a native complement,
	// so no $not is ever needed inside $elemMatch.
	complement := map[string]string{"$eq": "$ne", "$ne": "$eq", "$lt": "$gte", "$gte": "$lt", "$gt": "$lte", "$lte": "$gt", "$in": "$nin", "$nin": "$in"}
	var op string
	var arg any
	switch pred.Op {
	case condition.OpEq, condition.OpNe, condition.OpLt, condition.OpLe, condition.OpGt, condition.OpGe:
		field, value, o, ok := fieldVsLiteral(pred)
		if !ok || field.Field.Name != condition.SelfField {
			return nil, false, nil
		}
		op, arg = o, lit(value.Lit)
	case condition.OpIn, condition.OpNotIn:
		x, l := pred.Args[0], pred.Args[1]
		if x.Field == nil || x.Field.Name != condition.SelfField || !l.IsLiteral() || l.Kind == condition.KindEmptyList {
			return nil, false, nil
		}
		op, arg = inOps[pred.Op], lit(l.Lit)
	default:
		return nil, false, nil
	}
	path := tr.path(list.Field, sc)
	switch t.Op {
	case condition.OpAny:
		return doc(path, doc("$elemMatch", doc(op, arg))), true, nil
	case condition.OpNone:
		return doc(path, doc("$not", doc("$elemMatch", doc(op, arg)))), true, nil
	default:
		return doc(path, doc("$not", doc("$elemMatch", doc(complement[op], arg)))), true, nil
	}
}

// fieldVsLiteral normalises "field op literal" and "literal op field".
func fieldVsLiteral(t *condition.TypedNode) (field, value *condition.TypedNode, op string, ok bool) {
	a, b := t.Args[0], t.Args[1]
	switch {
	case a.Field != nil && b.IsLiteral():
		return a, b, cmpOps[t.Op], true
	case a.IsLiteral() && b.Field != nil:
		return b, a, flipped[t.Op], true
	}
	return nil, nil, "", false
}

// expr translates t into an aggregation expression (for $expr).
func (tr *translator) expr(t *condition.TypedNode, sc scope) (any, error) {
	switch {
	case t.Field != nil:
		return tr.fieldRef(t, sc), nil
	case t.IsLiteral():
		if t.Kind == condition.KindString && strings.HasPrefix(t.Lit.String(), "$") {
			return doc("$literal", t.Lit.String()), nil // otherwise read as a field path
		}
		if t.Kind.IsList() {
			return doc("$literal", lit(t.Lit)), nil
		}
		return lit(t.Lit), nil
	}

	args := func() ([]any, error) {
		out := make([]any, len(t.Args))
		for i, a := range t.Args {
			e, err := tr.expr(a, sc)
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	}
	list := func(t *condition.TypedNode) (any, error) {
		e, err := tr.expr(t, sc)
		return doc("$ifNull", []any{e, doc("$literal", []any{})}), err
	}

	switch t.Op {
	case condition.OpAnd, condition.OpOr:
		if len(t.Args) == 0 {
			return t.Op == condition.OpAnd, nil
		}
		as, err := args()
		return doc(logicOps[t.Op], as), err
	case condition.OpNot:
		as, err := args()
		return doc("$not", as), err
	case condition.OpEq, condition.OpNe, condition.OpLt, condition.OpLe, condition.OpGt, condition.OpGe:
		as, err := args()
		return doc(cmpOps[t.Op], as), err
	case condition.OpAdd, condition.OpSub, condition.OpMul, condition.OpDiv:
		as, err := args()
		return doc(arithOps[t.Op], as), err
	case condition.OpContains, condition.OpStartsWith, condition.OpEndsWith:
		as, err := args()
		if err != nil {
			return nil, err
		}
		x, y := as[0], as[1]
		idx := doc("$indexOfBytes", []any{x, y})
		switch t.Op {
		case condition.OpContains:
			return doc("$gte", []any{idx, 0}), nil
		case condition.OpStartsWith:
			return doc("$eq", []any{idx, 0}), nil
		}
		lx, ly := doc("$strLenBytes", x), doc("$strLenBytes", y)
		tail := doc("$substrBytes", []any{x, doc("$subtract", []any{lx, ly}), ly})
		return doc("$and", []any{doc("$gte", []any{lx, ly}), doc("$eq", []any{tail, y})}), nil
	case condition.OpIn, condition.OpNotIn:
		if t.Args[1].Kind == condition.KindEmptyList {
			return t.Op == condition.OpNotIn, nil
		}
		x, err := tr.expr(t.Args[0], sc)
		if err != nil {
			return nil, err
		}
		l, err := list(t.Args[1])
		in := doc("$in", []any{x, l})
		if t.Op == condition.OpNotIn {
			return doc("$not", []any{in}), err
		}
		return in, err
	case condition.OpLen:
		l, err := list(t.Args[0])
		return doc("$size", l), err
	case condition.OpAny, condition.OpAll, condition.OpNone:
		items, err := list(t.Args[0])
		if err != nil {
			return nil, err
		}
		tr.vars++
		v := "e" + strconv.Itoa(tr.vars)
		elem := scope{key: sc.key + t.Args[0].Field.Name + ".", elemVar: v}
		pred, err := tr.expr(t.Args[1], elem)
		if err != nil {
			return nil, err
		}
		mapped := []any{doc("$map", map[string]any{"input": items, "as": v, "in": pred})}
		switch t.Op {
		case condition.OpAny:
			return doc("$anyElementTrue", mapped), nil
		case condition.OpAll:
			return doc("$allElementsTrue", mapped), nil
		default:
			return doc("$not", []any{doc("$anyElementTrue", mapped)}), nil
		}
	}
	return nil, unsupported(t, "operator %s", t.Op)
}

func (tr *translator) fieldRef(t *condition.TypedNode, sc scope) string {
	if sc.elemVar != "" {
		if t.Field.Name == condition.SelfField {
			return "$$" + sc.elemVar
		}
		return "$$" + sc.elemVar + "." + tr.path(t.Field, sc)
	}
	return "$" + tr.path(t.Field, sc)
}
