package mongoquery_test

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// evalFilter is a small reference implementation of the MongoDB query
// semantics for exactly the operators the translator emits. It lets the
// differential test run without a server. Documents are map[string]any with
// float64 numbers, string, bool, []any and nested maps.
func evalFilter(f map[string]any, d map[string]any) bool {
	for k, v := range f {
		if !evalClause(k, v, d) {
			return false
		}
	}
	return true
}

func evalClause(k string, v any, d map[string]any) bool {
	switch k {
	case "$and":
		for _, s := range v.([]any) {
			if !evalFilter(s.(map[string]any), d) {
				return false
			}
		}
		return true
	case "$or":
		for _, s := range v.([]any) {
			if evalFilter(s.(map[string]any), d) {
				return true
			}
		}
		return false
	case "$nor":
		for _, s := range v.([]any) {
			if evalFilter(s.(map[string]any), d) {
				return false
			}
		}
		return true
	case "$expr":
		return truthy(evalExpr(v, d, nil))
	}
	val, present := lookup(d, k)
	ops, isOps := v.(map[string]any)
	if !isOps || !hasOperator(ops) {
		return matchValue(val, present, func(x any) bool { return equal(x, v) })
	}
	for op, arg := range ops {
		if !evalFieldOp(op, arg, val, present) {
			return false
		}
	}
	return true
}

// hasOperator reports whether m is a field-operator document such as
// {"$gt": 1}, as opposed to a query document ({"a": 1}, {"$and": [...]}).
func hasOperator(m map[string]any) bool {
	for k := range m {
		switch k {
		case "$and", "$or", "$nor", "$expr":
			return false
		}
		return strings.HasPrefix(k, "$")
	}
	return false
}

func lookup(d map[string]any, path string) (any, bool) {
	var cur any = d
	for part := range strings.SplitSeq(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// matchValue applies Mongo's implicit array traversal: a predicate on an
// array field matches if it matches the array itself or any element.
func matchValue(val any, present bool, pred func(any) bool) bool {
	if !present {
		return false
	}
	if arr, ok := val.([]any); ok {
		return slices.ContainsFunc(arr, pred) || pred(val)
	}
	return pred(val)
}

func evalFieldOp(op string, arg, val any, present bool) bool {
	switch op {
	case "$eq":
		return matchValue(val, present, func(x any) bool { return equal(x, arg) })
	case "$ne":
		return !matchValue(val, present, func(x any) bool { return equal(x, arg) })
	case "$lt", "$lte", "$gt", "$gte":
		return matchValue(val, present, func(x any) bool {
			c, ok := compare(x, arg)
			return ok && map[string]bool{"$lt": c < 0, "$lte": c <= 0, "$gt": c > 0, "$gte": c >= 0}[op]
		})
	case "$in":
		return matchValue(val, present, func(x any) bool {
			return slices.ContainsFunc(arg.([]any), func(a any) bool { return equal(x, a) })
		})
	case "$nin":
		return !evalFieldOp("$in", arg, val, present)
	case "$regex":
		re := regexp.MustCompile(arg.(string))
		return matchValue(val, present, func(x any) bool { s, ok := x.(string); return ok && re.MatchString(s) })
	case "$elemMatch":
		arr, _ := val.([]any)
		spec := arg.(map[string]any)
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok && !hasOperator(spec) {
				if evalFilter(spec, m) {
					return true
				}
				continue
			}
			// Operator form on scalar elements: every operator applies to
			// the element itself, without implicit array traversal.
			all := true
			for o, a := range spec {
				if !evalFieldOp(o, a, e, true) {
					all = false
				}
			}
			if all && hasOperator(spec) {
				return true
			}
		}
		return false
	case "$not":
		ops := arg.(map[string]any)
		for o, a := range ops {
			if !evalFieldOp(o, a, val, present) {
				return true
			}
		}
		return false
	}
	panic("reference evaluator: unknown operator " + op)
}

func num(v any) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	}
	return 0, false
}

func equal(a, b any) bool {
	if x, ok := num(a); ok {
		y, ok := num(b)
		return ok && x == y
	}
	return fmt.Sprint(a) == fmt.Sprint(b) && fmt.Sprintf("%T", a) == fmt.Sprintf("%T", b)
}

func compare(a, b any) (int, bool) {
	if x, ok := num(a); ok {
		if y, ok := num(b); ok {
			return map[bool]int{true: -1, false: 0}[x < y] + map[bool]int{true: 1, false: 0}[x > y], true
		}
		return 0, false
	}
	x, ok1 := a.(string)
	y, ok2 := b.(string)
	if ok1 && ok2 {
		return strings.Compare(x, y), true
	}
	return 0, false
}

func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	}
	if n, ok := num(v); ok {
		return n != 0
	}
	return true
}

// evalExpr evaluates aggregation expressions. vars holds $$name bindings.
func evalExpr(e any, d map[string]any, vars map[string]any) any {
	switch e := e.(type) {
	case string:
		if strings.HasPrefix(e, "$$") {
			name, rest, _ := strings.Cut(e[2:], ".")
			v := vars[name]
			if rest != "" {
				v, _ = lookup(v.(map[string]any), rest)
			}
			return v
		}
		if strings.HasPrefix(e, "$") {
			v, _ := lookup(d, e[1:])
			return v
		}
		return e
	case map[string]any:
		for op, arg := range e {
			return evalOp(op, arg, d, vars)
		}
	}
	return e
}

func evalOp(op string, arg any, d map[string]any, vars map[string]any) any {
	if op == "$literal" {
		return arg
	}
	if op == "$map" {
		spec := arg.(map[string]any)
		in, _ := evalExpr(spec["input"], d, vars).([]any)
		out := make([]any, len(in))
		for i, x := range in {
			nv := maps.Clone(vars)
			if nv == nil {
				nv = map[string]any{}
			}
			nv[spec["as"].(string)] = x
			out[i] = evalExpr(spec["in"], d, nv)
		}
		return out
	}
	var args []any
	if list, ok := arg.([]any); ok {
		for _, a := range list {
			args = append(args, evalExpr(a, d, vars))
		}
	} else {
		args = []any{evalExpr(arg, d, vars)}
	}
	f := func(i int) float64 { n, _ := num(args[i]); return n }
	s := func(i int) string { v, _ := args[i].(string); return v }
	switch op {
	case "$and":
		return !slices.ContainsFunc(args, func(a any) bool { return !truthy(a) })
	case "$or":
		return slices.ContainsFunc(args, truthy)
	case "$not":
		return !truthy(args[0])
	case "$eq":
		return equal(args[0], args[1])
	case "$ne":
		return !equal(args[0], args[1])
	case "$lt", "$lte", "$gt", "$gte":
		c, _ := compare(args[0], args[1])
		return map[string]bool{"$lt": c < 0, "$lte": c <= 0, "$gt": c > 0, "$gte": c >= 0}[op]
	case "$add":
		return f(0) + f(1)
	case "$subtract":
		return f(0) - f(1)
	case "$multiply":
		return f(0) * f(1)
	case "$divide":
		if f(1) == 0 {
			panic("divide by zero")
		}
		return f(0) / f(1)
	case "$ifNull":
		if args[0] == nil {
			return args[1]
		}
		return args[0]
	case "$size":
		return float64(len(args[0].([]any)))
	case "$in":
		return slices.ContainsFunc(args[1].([]any), func(a any) bool { return equal(args[0], a) })
	case "$indexOfBytes":
		return float64(strings.Index(s(0), s(1)))
	case "$strLenBytes":
		return float64(len(s(0)))
	case "$substrBytes":
		str, start, n := s(0), int(f(1)), int(f(2))
		start = max(0, min(start, len(str)))
		return str[start:min(len(str), start+n)]
	case "$anyElementTrue":
		return slices.ContainsFunc(args[0].([]any), truthy)
	case "$allElementsTrue":
		return !slices.ContainsFunc(args[0].([]any), func(a any) bool { return !truthy(a) })
	}
	panic("reference evaluator: unknown expression " + op)
}
