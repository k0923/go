// Package sqlquery translates a checked condition (condition.TypedNode) into a
// parameterised SQL WHERE clause, so the same rule that condition evaluates in
// memory can also be pushed down to a relational database.
//
// Values are always bound as parameters; identifiers come only from the
// Schema and from Options, never from rule input, so translated rules are not
// an injection vector.
//
// Semantic alignment with in-memory evaluation:
//
//   - Columns are assumed NOT NULL. SQL three-valued logic differs from Go
//     for NULLs; map nullable columns to COALESCE expressions via a view or
//     keep them out of the schema.
//   - String equality and ordering follow the column collation. Set
//     Options.Collate (for example "C" on PostgreSQL, "utf8mb4_bin" on MySQL)
//     to get Go's byte-wise semantics. SQLite's default BINARY already matches.
//   - contains/starts_with/ends_with are case-sensitive on every dialect
//     (SQLite uses instr/substr because its LIKE ignores ASCII case).
//   - Division is done in floating point like the evaluator. Division by
//     zero is an error in memory and on PostgreSQL, but yields NULL (row not
//     matched) on SQLite and MySQL.
//   - List fields need either a Relation (child table) or, on PostgreSQL, an
//     array column for scalar lists.
package sqlquery

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/k0923/go/condition"
)

// ErrUnsupported is returned when a rule cannot be expressed for the given
// dialect and options, for example a list field without a Relation.
var ErrUnsupported = errors.New("sqlquery: unsupported")

// Dialect selects placeholder style, identifier quoting and functions.
type Dialect uint8

const (
	SQLite Dialect = iota
	Postgres
	MySQL
)

// Options configure the translation of the root scope.
type Options struct {
	Dialect Dialect
	// Table is the root table name or alias. Columns are qualified with it,
	// and child subqueries correlate through it. Required when the rule uses
	// list fields backed by a Relation.
	Table string
	// Columns maps schema field names to column names (default: field name).
	Columns map[string]string
	// Relations maps list fields to child tables.
	Relations map[string]Relation
	// Collate, when set, is appended to string comparisons.
	Collate string
	// ArgOffset shifts PostgreSQL placeholders ($1 becomes $ArgOffset+1) so
	// the clause can be appended to a query that already has arguments.
	ArgOffset int
}

// Relation backs a list field with a child table.
type Relation struct {
	Table string
	// On correlates a child row with its parent. {parent} and {child} are
	// replaced by the quoted aliases, e.g. "{child}.order_id = {parent}.id".
	On string
	// Columns maps element field names to child columns. For scalar lists
	// (Strings/Numbers) map condition.SelfField ("$") to the value column.
	Columns map[string]string
	// Relations backs list fields of the element.
	Relations map[string]Relation
}

// Where translates t into a WHERE clause body and its arguments.
func Where(t *condition.TypedNode, opt Options) (string, []any, error) {
	if t == nil || t.Kind != condition.KindBool {
		return "", nil, fmt.Errorf("sqlquery: want a checked bool node")
	}
	w := &writer{opt: opt}
	root := &scope{alias: opt.Table, columns: opt.Columns, relations: opt.Relations}
	sql, err := w.cond(t, root)
	if err != nil {
		return "", nil, err
	}
	return sql, w.args, nil
}

type scope struct {
	alias     string
	columns   map[string]string
	relations map[string]Relation
	self      string // expression for condition.SelfField, if any
}

type writer struct {
	opt     Options
	args    []any
	aliases int
}

func unsupported(t *condition.TypedNode, format string, a ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrUnsupported, t.Path, fmt.Sprintf(format, a...))
}

func (w *writer) quote(ident string) string {
	if w.opt.Dialect == MySQL {
		return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// bind appends an argument and returns its placeholder.
func (w *writer) bind(v any) string {
	w.args = append(w.args, v)
	if w.opt.Dialect == Postgres {
		return "$" + strconv.Itoa(w.opt.ArgOffset+len(w.args))
	}
	return "?"
}

// bindNum binds integral numbers as int64 so PostgreSQL does not reject them
// for integer columns.
func (w *writer) bindNum(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return w.bind(int64(f))
	}
	return w.bind(f)
}

func (w *writer) strLen() string {
	if w.opt.Dialect == MySQL {
		return "CHAR_LENGTH"
	}
	return "length"
}

func (w *writer) collate(s string) string {
	if w.opt.Collate == "" {
		return s
	}
	return s + " COLLATE " + w.quote(w.opt.Collate)
}

func (w *writer) column(t *condition.TypedNode, sc *scope) (string, error) {
	name := t.Field.Name
	if name == condition.SelfField {
		if sc.self == "" {
			return "", unsupported(t, "no value column mapped for %q", name)
		}
		return sc.self, nil
	}
	col := name
	if c, ok := sc.columns[name]; ok {
		col = c
	}
	if sc.alias == "" {
		return w.quote(col), nil
	}
	return w.quote(sc.alias) + "." + w.quote(col), nil
}

// cond renders a bool-kinded node as a predicate.
func (w *writer) cond(t *condition.TypedNode, sc *scope) (string, error) {
	switch {
	case t.Field != nil:
		return w.column(t, sc)
	case t.IsLiteral():
		if t.Lit.Bool() {
			return "TRUE", nil
		}
		return "FALSE", nil
	}

	switch t.Op {
	case condition.OpAnd, condition.OpOr:
		empty, sep := "TRUE", " AND "
		if t.Op == condition.OpOr {
			empty, sep = "FALSE", " OR "
		}
		if len(t.Args) == 0 {
			return empty, nil
		}
		parts := make([]string, len(t.Args))
		for i, a := range t.Args {
			s, err := w.cond(a, sc)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		if len(parts) == 1 {
			return parts[0], nil
		}
		return "(" + strings.Join(parts, sep) + ")", nil

	case condition.OpNot:
		s, err := w.cond(t.Args[0], sc)
		return "NOT (" + s + ")", err

	case condition.OpEq, condition.OpNe, condition.OpLt, condition.OpLe, condition.OpGt, condition.OpGe:
		a, b, err := w.pair(t, sc)
		if err != nil {
			return "", err
		}
		if t.Args[0].Kind == condition.KindString {
			a = w.collate(a)
		}
		return a + " " + cmpOps[t.Op] + " " + b, nil

	case condition.OpContains, condition.OpStartsWith, condition.OpEndsWith:
		return w.stringOp(t, sc)

	case condition.OpIn, condition.OpNotIn:
		s, err := w.in(t, sc)
		if err != nil || t.Op == condition.OpIn {
			return s, err
		}
		return "NOT (" + s + ")", nil

	case condition.OpAny, condition.OpAll, condition.OpNone:
		return w.quant(t, sc)
	}
	return "", unsupported(t, "operator %s", t.Op)
}

var (
	cmpOps   = map[condition.Operator]string{condition.OpEq: "=", condition.OpNe: "<>", condition.OpLt: "<", condition.OpLe: "<=", condition.OpGt: ">", condition.OpGe: ">="}
	arithOps = map[condition.Operator]string{condition.OpAdd: "+", condition.OpSub: "-", condition.OpMul: "*"}
)

func (w *writer) pair(t *condition.TypedNode, sc *scope) (string, string, error) {
	a, err := w.val(t.Args[0], sc)
	if err != nil {
		return "", "", err
	}
	b, err := w.val(t.Args[1], sc)
	return a, b, err
}

// val renders a scalar-kinded node (number, string or bool) as an expression.
func (w *writer) val(t *condition.TypedNode, sc *scope) (string, error) {
	switch {
	case t.Field != nil:
		return w.column(t, sc)
	case t.IsLiteral():
		switch t.Kind {
		case condition.KindBool:
			return strings.ToUpper(strconv.FormatBool(t.Lit.Bool())), nil
		case condition.KindNumber:
			return w.bindNum(t.Lit.Number()), nil
		default:
			return w.bind(t.Lit.String()), nil
		}
	case t.Kind == condition.KindBool:
		s, err := w.cond(t, sc)
		return "(" + s + ")", err
	}
	switch t.Op {
	case condition.OpAdd, condition.OpSub, condition.OpMul, condition.OpDiv:
		a, b, err := w.pair(t, sc)
		if err != nil {
			return "", err
		}
		if t.Op == condition.OpDiv {
			// Force floating-point division to match the evaluator.
			return "(" + a + " * 1.0 / " + b + ")", nil
		}
		return "(" + a + " " + arithOps[t.Op] + " " + b + ")", nil
	case condition.OpLen:
		return w.length(t.Args[0], sc)
	}
	return "", unsupported(t, "operator %s as a value", t.Op)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (w *writer) stringOp(t *condition.TypedNode, sc *scope) (string, error) {
	x, err := w.val(t.Args[0], sc)
	if err != nil {
		return "", err
	}
	pat := t.Args[1]
	// A literal pattern uses LIKE on PostgreSQL and MySQL so prefix searches
	// can use an index. SQLite's LIKE is case-insensitive, so it never does.
	if pat.IsLiteral() && w.opt.Dialect != SQLite {
		s := escapeLike(pat.Lit.String())
		switch t.Op {
		case condition.OpContains:
			s = "%" + s + "%"
		case condition.OpStartsWith:
			s += "%"
		default:
			s = "%" + s
		}
		like := " LIKE "
		if w.opt.Dialect == MySQL {
			like = " LIKE BINARY " // case- and accent-sensitive
		}
		return x + like + w.bind(s) + ` ESCAPE '\'`, nil
	}
	// p renders the pattern again for every use, rebinding literals.
	p := func() (string, error) { return w.val(pat, sc) }
	ln := w.strLen()
	switch t.Op {
	case condition.OpContains:
		fn := map[Dialect]string{SQLite: "instr", Postgres: "strpos", MySQL: "INSTR"}[w.opt.Dialect]
		y, err := p()
		return fn + "(" + x + ", " + y + ") > 0", err
	case condition.OpStartsWith:
		y1, err := p()
		if err != nil {
			return "", err
		}
		y2, err := p()
		return fmt.Sprintf("substr(%s, 1, %s(%s)) = %s", x, ln, y1, y2), err
	default:
		y1, err := p()
		if err != nil {
			return "", err
		}
		y2, err := p()
		if err != nil {
			return "", err
		}
		y3, err := p()
		return fmt.Sprintf("(%s(%s) >= %s(%s) AND substr(%s, %s(%s) - %s(%s) + 1) = %s)",
			ln, x, ln, y1, x, ln, x, ln, y2, y3), err
	}
}

func (w *writer) in(t *condition.TypedNode, sc *scope) (string, error) {
	list := t.Args[1]
	if list.Kind == condition.KindEmptyList {
		return "FALSE", nil
	}
	x, err := w.val(t.Args[0], sc)
	if err != nil {
		return "", err
	}
	if list.IsLiteral() {
		var phs []string
		if list.Kind == condition.KindNumbers {
			for _, v := range list.Lit.Numbers() {
				phs = append(phs, w.bindNum(v))
			}
		} else {
			for _, v := range list.Lit.Strings() {
				phs = append(phs, w.bind(v))
			}
		}
		if t.Args[0].Kind == condition.KindString {
			x = w.collate(x)
		}
		return x + " IN (" + strings.Join(phs, ", ") + ")", nil
	}
	// A list field: membership is "some element equals x".
	if _, ok := sc.relations[list.Field.Name]; !ok && w.opt.Dialect == Postgres {
		col, err := w.column(list, sc)
		return x + " = ANY(" + col + ")", err
	}
	return w.exists(list, sc, func(el *scope) (string, error) {
		if el.self == "" {
			return "", unsupported(list, "no value column mapped for %q", condition.SelfField)
		}
		return w.collateIf(t.Args[0].Kind, el.self) + " = " + x, nil
	})
}

func (w *writer) collateIf(k condition.Kind, s string) string {
	if k == condition.KindString {
		return w.collate(s)
	}
	return s
}

func (w *writer) quant(t *condition.TypedNode, sc *scope) (string, error) {
	list, pred := t.Args[0], t.Args[1]
	negatePred := t.Op == condition.OpAll
	s, err := w.exists(list, sc, func(el *scope) (string, error) {
		p, err := w.cond(pred, el)
		if negatePred {
			p = "NOT (" + p + ")"
		}
		return p, err
	})
	if err != nil || t.Op == condition.OpAny {
		return s, err
	}
	// all = no element fails the predicate; none = no element matches it.
	return "NOT " + s, nil
}

// exists renders EXISTS (SELECT 1 FROM <elements of list> WHERE <where(el)>).
func (w *writer) exists(list *condition.TypedNode, sc *scope, where func(*scope) (string, error)) (string, error) {
	from, el, err := w.elements(list, sc)
	if err != nil {
		return "", err
	}
	cond, err := where(el)
	if err != nil {
		return "", err
	}
	return "EXISTS (SELECT 1 FROM " + from.table + " WHERE " + joinWhere(from.on, cond) + ")", nil
}

func (w *writer) length(list *condition.TypedNode, sc *scope) (string, error) {
	if list.IsLiteral() {
		return strconv.Itoa(list.Lit.Len()), nil
	}
	if _, ok := sc.relations[list.Field.Name]; !ok && w.opt.Dialect == Postgres && list.Kind != condition.KindObjects {
		col, err := w.column(list, sc)
		return "cardinality(" + col + ")", err
	}
	from, _, err := w.elements(list, sc)
	if err != nil {
		return "", err
	}
	q := "(SELECT COUNT(*) FROM " + from.table
	if from.on != "" {
		q += " WHERE " + from.on
	}
	return q + ")", nil
}

type source struct{ table, on string }

func joinWhere(on, cond string) string {
	if on == "" {
		return cond
	}
	return on + " AND " + cond
}

// elements resolves a list field to a FROM source and the element scope.
func (w *writer) elements(list *condition.TypedNode, sc *scope) (source, *scope, error) {
	w.aliases++
	alias := "t" + strconv.Itoa(w.aliases)
	qa := w.quote(alias)
	if rel, ok := sc.relations[list.Field.Name]; ok {
		if sc.alias == "" && strings.Contains(rel.On, "{parent}") {
			return source{}, nil, unsupported(list, "Options.Table (or the parent relation) must name the parent alias")
		}
		on := strings.NewReplacer("{parent}", w.quote(sc.alias), "{child}", qa).Replace(rel.On)
		el := &scope{alias: alias, columns: rel.Columns, relations: rel.Relations}
		if c, ok := rel.Columns[condition.SelfField]; ok {
			el.self = qa + "." + w.quote(c)
		}
		return source{table: w.quote(rel.Table) + " AS " + qa, on: on}, el, nil
	}
	if w.opt.Dialect == Postgres && list.Kind != condition.KindObjects {
		col, err := w.column(list, sc)
		if err != nil {
			return source{}, nil, err
		}
		return source{table: "unnest(" + col + ") AS " + qa + "(v)"}, &scope{self: qa + ".v"}, nil
	}
	return source{}, nil, unsupported(list, "list field %q needs a Relation", list.Field.Name)
}
