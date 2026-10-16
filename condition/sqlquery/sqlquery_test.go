package sqlquery_test

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/k0923/go/condition"
	"github.com/k0923/go/condition/sqlquery"
)

type item struct {
	Category string
	Price    float64
}

type order struct {
	ID      int
	Amount  float64
	Country string
	Note    string
	VIP     bool
	Tags    []string
	Items   []item
}

func schema() *condition.Schema[order] {
	items := condition.NewSchema[item]().
		String("category", func(i item) string { return i.Category }).
		Number("price", func(i item) float64 { return i.Price })
	return condition.NewSchema[order]().
		Number("id", func(o order) int { return o.ID }).
		Number("amount", func(o order) float64 { return o.Amount }).
		String("country", func(o order) string { return o.Country }).
		String("note", func(o order) string { return o.Note }).
		Bool("vip", func(o order) bool { return o.VIP }).
		Strings("tags", func(o order) []string { return o.Tags }).
		List("items", func(o order) []item { return o.Items }, items)
}

var opts = sqlquery.Options{
	Dialect: sqlquery.SQLite,
	Table:   "o",
	Relations: map[string]sqlquery.Relation{
		"tags":  {Table: "order_tags", On: "{child}.order_id = {parent}.id", Columns: map[string]string{condition.SelfField: "tag"}},
		"items": {Table: "order_items", On: "{child}.order_id = {parent}.id"},
	},
}

// fixture produces orders that exercise edge cases: empty lists, LIKE
// metacharacters, mixed case, prefixes that equal the whole string.
func fixture() []order {
	r := rand.New(rand.NewPCG(1, 2))
	countries := []string{"CN", "cn", "SG", "US", "C"}
	notes := []string{"", "100%_off", "VIP gift", "vip", "a\\b", "gift wrap", "%"}
	tags := []string{"new", "sale", "NEW", "gift"}
	cats := []string{"book", "food", "Book", "toy"}
	var out []order
	for id := 1; id <= 300; id++ {
		o := order{
			ID: id, Amount: float64(r.IntN(400)) / 2,
			Country: countries[r.IntN(len(countries))], Note: notes[r.IntN(len(notes))],
			VIP: r.IntN(2) == 0,
		}
		for range r.IntN(3) {
			if t := tags[r.IntN(len(tags))]; !slices.Contains(o.Tags, t) {
				o.Tags = append(o.Tags, t)
			}
		}
		for range r.IntN(4) {
			o.Items = append(o.Items, item{Category: cats[r.IntN(len(cats))], Price: float64(r.IntN(100))})
		}
		out = append(out, o)
	}
	return out
}

func load(t *testing.T, orders []order) *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE orders (id INTEGER PRIMARY KEY, amount REAL NOT NULL, country TEXT NOT NULL, note TEXT NOT NULL, vip BOOLEAN NOT NULL)`)
	exec(`CREATE TABLE order_tags (order_id INTEGER NOT NULL, tag TEXT NOT NULL)`)
	exec(`CREATE TABLE order_items (order_id INTEGER NOT NULL, category TEXT NOT NULL, price REAL NOT NULL)`)
	for _, o := range orders {
		exec(`INSERT INTO orders VALUES (?, ?, ?, ?, ?)`, o.ID, o.Amount, o.Country, o.Note, o.VIP)
		for _, tag := range o.Tags {
			exec(`INSERT INTO order_tags VALUES (?, ?)`, o.ID, tag)
		}
		for _, it := range o.Items {
			exec(`INSERT INTO order_items VALUES (?, ?, ?)`, o.ID, it.Category, it.Price)
		}
	}
	return db
}

var rules = map[string]string{
	"le sub":          `{"op":"le","args":[{"op":"sub","args":[{"field":"amount"},{"field":"id"}]},{"value":120}]}`,
	"le string":       `{"op":"le","args":[{"value":"SG"},{"field":"country"}]}`,
	"compare":         `{"op":"and","args":[{"op":"ge","args":[{"field":"amount"},{"value":100}]},{"op":"lt","args":[{"field":"country"},{"value":"SG"}]}]}`,
	"bool field":      `{"op":"or","args":[{"field":"vip"},{"op":"eq","args":[{"field":"vip"},{"value":false}]}]}`,
	"bool eq":         `{"op":"eq","args":[{"field":"vip"},{"op":"gt","args":[{"field":"amount"},{"value":50}]}]}`,
	"not":             `{"op":"not","args":[{"op":"in","args":[{"field":"country"},{"value":["CN","SG"]}]}]}`,
	"case sensitive":  `{"op":"eq","args":[{"field":"country"},{"value":"cn"}]}`,
	"contains meta":   `{"op":"contains","args":[{"field":"note"},{"value":"%_"}]}`,
	"contains case":   `{"op":"contains","args":[{"field":"note"},{"value":"VIP"}]}`,
	"starts_with":     `{"op":"starts_with","args":[{"field":"note"},{"value":"gift"}]}`,
	"ends_with":       `{"op":"ends_with","args":[{"field":"note"},{"value":"\\b"}]}`,
	"ends_with field": `{"op":"ends_with","args":[{"field":"country"},{"field":"note"}]}`,
	"starts empty":    `{"op":"starts_with","args":[{"field":"note"},{"value":""}]}`,
	"field vs field":  `{"op":"ge","args":[{"field":"note"},{"field":"country"}]}`,
	"arith":           `{"op":"gt","args":[{"op":"div","args":[{"op":"mul","args":[{"field":"amount"},{"value":3}]},{"value":2}]},{"op":"add","args":[{"field":"id"},{"value":0.5}]}]}`,
	"in empty":        `{"op":"in","args":[{"field":"country"},{"value":[]}]}`,
	"not_in empty":    `{"op":"not_in","args":[{"field":"country"},{"value":[]}]}`,
	"tag member":      `{"op":"in","args":[{"value":"sale"},{"field":"tags"}]}`,
	"tag not member":  `{"op":"not_in","args":[{"value":"new"},{"field":"tags"}]}`,
	"len":             `{"op":"ge","args":[{"op":"len","args":[{"field":"items"}]},{"value":2}]}`,
	"len tags zero":   `{"op":"eq","args":[{"op":"len","args":[{"field":"tags"}]},{"value":0}]}`,
	"any item":        `{"op":"any","args":[{"field":"items"},{"op":"and","args":[{"op":"eq","args":[{"field":"category"},{"value":"book"}]},{"op":"gt","args":[{"field":"price"},{"value":50}]}]}]}`,
	"all items":       `{"op":"all","args":[{"field":"items"},{"op":"lt","args":[{"field":"price"},{"value":60}]}]}`,
	"none items":      `{"op":"none","args":[{"field":"items"},{"op":"in","args":[{"field":"category"},{"value":["food","toy"]}]}]}`,
	"all tags":        `{"op":"all","args":[{"field":"tags"},{"op":"ne","args":[{"field":"$"},{"value":"NEW"}]}]}`,
	"empty and/or":    `{"op":"or","args":[{"op":"and"},{"op":"or"}]}`,
	"item vs order":   `{"op":"any","args":[{"field":"items"},{"op":"starts_with","args":[{"field":"category"},{"value":"Bo"}]}]}`,
	"complex": `{"op":"and","args":[
		{"op":"or","args":[{"field":"vip"},{"op":"ge","args":[{"field":"amount"},{"value":150}]}]},
		{"op":"not_in","args":[{"field":"country"},{"value":["US"]}]},
		{"op":"any","args":[{"field":"items"},{"op":"ge","args":[{"field":"price"},{"value":10}]}]},
		{"op":"none","args":[{"field":"tags"},{"op":"eq","args":[{"field":"$"},{"value":"gift"}]}]}]}`,
}

// TestDifferentialSQLite runs every rule both in memory and in SQLite and
// requires identical result sets.
func TestDifferentialSQLite(t *testing.T) {
	orders := fixture()
	db := load(t, orders)
	s := schema()
	for name, src := range rules {
		t.Run(name, func(t *testing.T) {
			typed, err := s.CheckJSON([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			match, err := s.CompileJSON([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			var want []int
			for _, o := range orders {
				if ok, err := match.Match(o); err != nil {
					t.Fatal(err)
				} else if ok {
					want = append(want, o.ID)
				}
			}
			where, args, whereErr := sqlquery.Where(typed, opts)
			if whereErr != nil {
				t.Fatal(whereErr)
			}
			rows, err := db.Query(`SELECT id FROM orders AS "o" WHERE `+where+` ORDER BY id`, args...)
			if err != nil {
				t.Fatalf("%v\n%s", err, where)
			}
			defer rows.Close()
			var got []int
			for rows.Next() {
				var id int
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				got = append(got, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("mismatch: memory=%d rows sql=%d rows\n%s\n%v", len(want), len(got), where, args)
			}
			if len(want) == 0 && !strings.Contains(name, "empty") && name != "case sensitive" {
				t.Logf("note: rule matches nothing")
			}
		})
	}
}

func TestDialects(t *testing.T) {
	s := schema()
	typed, err := s.CheckJSON([]byte(`{"op":"and","args":[
		{"op":"ge","args":[{"field":"amount"},{"value":100}]},
		{"op":"starts_with","args":[{"field":"note"},{"value":"50%"}]},
		{"op":"in","args":[{"value":"sale"},{"field":"tags"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		opt  sqlquery.Options
		want string
		args []any
	}{
		{
			sqlquery.Options{Dialect: sqlquery.Postgres, Table: "o", ArgOffset: 2, Columns: map[string]string{"amount": "total"}},
			`("o"."total" >= $3 AND "o"."note" LIKE $4 ESCAPE '\' AND $5 = ANY("o"."tags"))`,
			[]any{int64(100), `50\%%`, "sale"},
		},
		{
			sqlquery.Options{Dialect: sqlquery.MySQL, Relations: opts.Relations, Table: "o"},
			"(`o`.`amount` >= ? AND `o`.`note` LIKE BINARY ? ESCAPE '\\' AND EXISTS (SELECT 1 FROM `order_tags` AS `t1` WHERE `t1`.order_id = `o`.id AND `t1`.`tag` = ?))",
			[]any{int64(100), `50\%%`, "sale"},
		},
	}
	for _, tc := range cases {
		got, args, err := sqlquery.Where(typed, tc.opt)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want || fmt.Sprint(args) != fmt.Sprint(tc.args) {
			t.Fatalf("got  %s %v\nwant %s %v", got, args, tc.want, tc.args)
		}
	}
}

func TestUnsupported(t *testing.T) {
	typed, err := schema().CheckJSON([]byte(`{"op":"any","args":[{"field":"items"},{"field":"vip"}]}`))
	if err == nil {
		t.Fatal("predicate must not see parent fields")
	}
	typed, err = schema().CheckJSON([]byte(`{"op":"any","args":[{"field":"items"},{"op":"gt","args":[{"field":"price"},{"value":1}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sqlquery.Where(typed, sqlquery.Options{Table: "o"}); err == nil || !strings.Contains(err.Error(), "needs a Relation") {
		t.Fatalf("want missing relation error, got %v", err)
	}
}

// TestIdentifierQuoting checks that hostile column names cannot break out.
func TestIdentifierQuoting(t *testing.T) {
	typed, _ := schema().CheckJSON([]byte(`{"op":"eq","args":[{"field":"country"},{"value":"x' OR 1=1 --"}]}`))
	where, args, err := sqlquery.Where(typed, sqlquery.Options{Columns: map[string]string{"country": `c"; DROP TABLE x; --`}})
	if err != nil {
		t.Fatal(err)
	}
	if where != `"c""; DROP TABLE x; --" = ?` || args[0] != "x' OR 1=1 --" {
		t.Fatalf("got %s %v", where, args)
	}
}

// FuzzDifferentialSQLite: any rule that passes Check must translate, run on
// SQLite and select exactly the rows Match selects (division by zero aside,
// which SQLite turns into NULL instead of an error).
func FuzzDifferentialSQLite(f *testing.F) {
	for _, src := range rules {
		f.Add([]byte(src))
	}
	orders := fixture()[:60]
	db := load(&testing.T{}, orders)
	s := schema()
	f.Fuzz(func(t *testing.T, data []byte) {
		typed, err := s.CheckJSON(data)
		if err != nil {
			return
		}
		match, _ := s.CompileJSON(data)
		var want []int
		for _, o := range orders {
			ok, err := match.Match(o)
			if err != nil {
				return
			}
			if ok {
				want = append(want, o.ID)
			}
		}
		where, args, err := sqlquery.Where(typed, opts)
		if err != nil {
			if strings.Contains(err.Error(), "unsupported") {
				return
			}
			t.Fatal(err)
		}
		rows, err := db.Query(`SELECT id FROM orders AS "o" WHERE `+where+` ORDER BY id`, args...)
		if err != nil {
			t.Fatalf("%v\n%s", err, where)
		}
		defer rows.Close()
		var got []int
		for rows.Next() {
			var id int
			_ = rows.Scan(&id)
			got = append(got, id)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("mismatch\nrule: %s\nsql: %s %v\nmemory=%v\nsql=%v", data, where, args, want, got)
		}
	})
}

// TestEveryOperatorCovered fails when an operator is added to condition but
// no differential rule exercises it here.
func TestEveryOperatorCovered(t *testing.T) {
	seen := map[condition.Operator]bool{}
	var walk func(*condition.TypedNode)
	walk = func(n *condition.TypedNode) {
		seen[n.Op] = true
		for _, a := range n.Args {
			walk(a)
		}
	}
	for _, src := range rules {
		typed, err := schema().CheckJSON([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		walk(typed)
	}
	for _, op := range condition.Operators() {
		if !seen[op] {
			t.Errorf("no differential rule uses %s", op)
		}
	}
}
