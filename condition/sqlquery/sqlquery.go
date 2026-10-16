// Package sqlquery translates a checked rule (condition.Checked) into a
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
	"github.com/k0923/go/condition/rule"
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

// Where translates a checked rule into a WHERE clause body and its arguments.
func Where(chk *condition.Checked, opt Options) (string, []any, error) {
	if chk == nil {
		return "", nil, fmt.Errorf("sqlquery: nil rule")
	}
	w := &writer{opt: opt, chk: chk}
	root := &scope{alias: opt.Table, columns: opt.Columns, relations: opt.Relations}
	sql, err := w.cond(chk.Root(), root)
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
	chk     *condition.Checked
	args    []any
	aliases int
}

func (w *writer) unsupported(n rule.Node, format string, a ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrUnsupported, w.chk.Path(n), fmt.Sprintf(format, a...))
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

func (w *writer) column(f *rule.Field, sc *scope) (string, error) {
	name := f.Name
	if name == condition.SelfField {
		if sc.self == "" {
			return "", w.unsupported(f, "no value column mapped for %q", name)
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

// cond renders a condition as a predicate.
func (w *writer) cond(n rule.Cond, sc *scope) (string, error) {
	group := func(items []rule.Cond, empty, sep string) (string, error) {
		if len(items) == 0 {
			return empty, nil
		}
		parts := make([]string, len(items))
		for i, it := range items {
			s, err := w.cond(it, sc)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		if len(parts) == 1 {
			return parts[0], nil
		}
		return "(" + strings.Join(parts, sep) + ")", nil
	}
	switch n := n.(type) {
	case *rule.And:
		return group(n.Items, "TRUE", " AND ")
	case *rule.Or:
		return group(n.Items, "FALSE", " OR ")
	case *rule.Not:
		s, err := w.cond(n.X, sc)
		return "NOT (" + s + ")", err
	case *rule.Quant:
		return w.quant(n, sc)
	case *rule.Compare:
		switch op := n.Op; {
		case op == rule.OpContains || op == rule.OpStartsWith || op == rule.OpEndsWith:
			return w.stringOp(n, sc)
		case op.IsList():
			s, err := w.in(n, sc)
			if err != nil || op == rule.OpIn {
				return s, err
			}
			return "NOT (" + s + ")", nil
		}
		a, b, err := w.pair(n.X, n.Y, sc)
		if err != nil {
			return "", err
		}
		if n.Type == rule.TypeString {
			a = w.collate(a)
		}
		return a + " " + cmpOps[n.Op] + " " + b, nil
	}
	return "", w.unsupported(n, "condition %T", n)
}

var (
	cmpOps   = map[rule.Op]string{rule.OpEq: "=", rule.OpNe: "<>", rule.OpLt: "<", rule.OpLte: "<=", rule.OpGt: ">", rule.OpGte: ">="}
	arithOps = map[rule.Op]string{rule.OpAdd: "+", rule.OpSub: "-", rule.OpMul: "*"}
)

func (w *writer) pair(x, y rule.Expr, sc *scope) (string, string, error) {
	a, err := w.val(x, sc)
	if err != nil {
		return "", "", err
	}
	b, err := w.val(y, sc)
	return a, b, err
}

// val renders a scalar expression (number, string or bool).
func (w *writer) val(e rule.Expr, sc *scope) (string, error) {
	switch e := e.(type) {
	case *rule.Field:
		return w.column(e, sc)
	case *rule.Bool:
		return strings.ToUpper(strconv.FormatBool(e.V)), nil
	case *rule.Number:
		return w.bindNum(e.V), nil
	case *rule.String:
		return w.bind(e.V), nil
	case *rule.Arith:
		a, b, err := w.pair(e.X, e.Y, sc)
		if err != nil {
			return "", err
		}
		if e.Op == rule.OpDiv {
			// Force floating-point division to match the evaluator.
			return "(" + a + " * 1.0 / " + b + ")", nil
		}
		return "(" + a + " " + arithOps[e.Op] + " " + b + ")", nil
	case *rule.Len:
		return w.length(e.X, sc)
	}
	return "", w.unsupported(e, "expression %T as a value", e)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (w *writer) stringOp(n *rule.Compare, sc *scope) (string, error) {
	x, err := w.val(n.X, sc)
	if err != nil {
		return "", err
	}
	// A literal pattern uses LIKE on PostgreSQL and MySQL so prefix searches
	// can use an index. SQLite's LIKE is case-insensitive, so it never does.
	if pat, ok := n.Y.(*rule.String); ok && w.opt.Dialect != SQLite {
		s := escapeLike(pat.V)
		switch n.Op {
		case rule.OpContains:
			s = "%" + s + "%"
		case rule.OpStartsWith:
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
	p := func() (string, error) { return w.val(n.Y, sc) }
	ln := w.strLen()
	switch n.Op {
	case rule.OpContains:
		fn := map[Dialect]string{SQLite: "instr", Postgres: "strpos", MySQL: "INSTR"}[w.opt.Dialect]
		y, err := p()
		return fn + "(" + x + ", " + y + ") > 0", err
	case rule.OpStartsWith:
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

func (w *writer) in(n *rule.Compare, sc *scope) (string, error) {
	var phs []string
	switch l := n.Y.(type) {
	case *rule.Numbers:
		for _, v := range l.V {
			phs = append(phs, w.bindNum(v))
		}
	case *rule.Strings:
		for _, v := range l.V {
			phs = append(phs, w.bind(v))
		}
	case *rule.Field:
		return w.inField(n, l, sc)
	default:
		return "", w.unsupported(n.Y, "list %T", l)
	}
	if len(phs) == 0 {
		return "FALSE", nil
	}
	x, err := w.val(n.X, sc)
	if err != nil {
		return "", err
	}
	if n.Type == rule.TypeString {
		x = w.collate(x)
	}
	return x + " IN (" + strings.Join(phs, ", ") + ")", nil
}

// inField renders membership in a list field: "some element equals x".
func (w *writer) inField(n *rule.Compare, list *rule.Field, sc *scope) (string, error) {
	x, err := w.val(n.X, sc)
	if err != nil {
		return "", err
	}
	if _, ok := sc.relations[list.Name]; !ok && w.opt.Dialect == Postgres {
		col, err := w.column(list, sc)
		return x + " = ANY(" + col + ")", err
	}
	return w.exists(list, sc, func(el *scope) (string, error) {
		if el.self == "" {
			return "", w.unsupported(list, "no value column mapped for %q", condition.SelfField)
		}
		self := el.self
		if n.Type == rule.TypeString {
			self = w.collate(self)
		}
		return self + " = " + x, nil
	})
}

func (w *writer) quant(n *rule.Quant, sc *scope) (string, error) {
	negatePred := n.Op == rule.OpAll
	s, err := w.exists(n.X, sc, func(el *scope) (string, error) {
		p, err := w.cond(n.Y, el)
		if negatePred {
			p = "NOT (" + p + ")"
		}
		return p, err
	})
	if err != nil || n.Op == rule.OpAny {
		return s, err
	}
	// all = no element fails the predicate; none = no element matches it.
	return "NOT " + s, nil
}

// exists renders EXISTS (SELECT 1 FROM <elements of list> WHERE <where(el)>).
func (w *writer) exists(list *rule.Field, sc *scope, where func(*scope) (string, error)) (string, error) {
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

func (w *writer) length(list *rule.Field, sc *scope) (string, error) {
	if _, ok := sc.relations[list.Name]; !ok && w.opt.Dialect == Postgres && w.chk.Field(list).Kind != condition.KindObjects {
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
func (w *writer) elements(list *rule.Field, sc *scope) (source, *scope, error) {
	w.aliases++
	alias := "t" + strconv.Itoa(w.aliases)
	qa := w.quote(alias)
	if rel, ok := sc.relations[list.Name]; ok {
		if sc.alias == "" && strings.Contains(rel.On, "{parent}") {
			return source{}, nil, w.unsupported(list, "Options.Table (or the parent relation) must name the parent alias")
		}
		on := strings.NewReplacer("{parent}", w.quote(sc.alias), "{child}", qa).Replace(rel.On)
		el := &scope{alias: alias, columns: rel.Columns, relations: rel.Relations}
		if c, ok := rel.Columns[condition.SelfField]; ok {
			el.self = qa + "." + w.quote(c)
		}
		return source{table: w.quote(rel.Table) + " AS " + qa, on: on}, el, nil
	}
	if w.opt.Dialect == Postgres && w.chk.Field(list).Kind != condition.KindObjects {
		col, err := w.column(list, sc)
		if err != nil {
			return source{}, nil, err
		}
		return source{table: "unnest(" + col + ") AS " + qa + "(v)"}, &scope{self: qa + ".v"}, nil
	}
	return source{}, nil, w.unsupported(list, "list field %q needs a Relation", list.Name)
}
