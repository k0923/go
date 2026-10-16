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
	"github.com/k0923/go/condition/rule"
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
	"le sub":          `{"type":"number","x":{"type":"arith","x":"amount","opt":"sub","y":{"type":"field","name":"id"}},"opt":"lte","y":120}`,
	"le string":       `{"type":"string","x":{"type":"literal","value":"SG"},"opt":"lte","y":{"type":"field","name":"country"}}`,
	"compare":         `{"type":"and","items":[{"type":"number","x":"amount","opt":"gte","y":100},{"type":"string","x":"country","opt":"lt","y":"SG"}]}`,
	"bool field":      `{"type":"or","items":[{"type":"boolean","x":"vip","opt":"eq","y":true},{"type":"boolean","x":"vip","opt":"eq","y":false}]}`,
	"bool eq":         `{"type":"boolean","x":"vip","opt":"ne","y":false}`,
	"not":             `{"type":"not","x":{"type":"string","x":"country","opt":"in","y":["CN","SG"]}}`,
	"case sensitive":  `{"type":"string","x":"country","opt":"eq","y":"cn"}`,
	"contains meta":   `{"type":"string","x":"note","opt":"contains","y":"%_"}`,
	"contains case":   `{"type":"string","x":"note","opt":"contains","y":"VIP"}`,
	"starts_with":     `{"type":"string","x":"note","opt":"starts_with","y":"gift"}`,
	"ends_with":       `{"type":"string","x":"note","opt":"ends_with","y":"\\b"}`,
	"ends_with field": `{"type":"string","x":"country","opt":"ends_with","y":{"type":"field","name":"note"}}`,
	"starts empty":    `{"type":"string","x":"note","opt":"starts_with","y":""}`,
	"field vs field":  `{"type":"string","x":"note","opt":"gte","y":{"type":"field","name":"country"}}`,
	"arith":           `{"type":"number","x":{"type":"arith","x":{"type":"arith","x":"amount","opt":"mul","y":3},"opt":"div","y":2},"opt":"gt","y":{"type":"arith","x":"id","opt":"add","y":0.5}}`,
	"in empty":        `{"type":"string","x":"country","opt":"in","y":[]}`,
	"not_in empty":    `{"type":"string","x":"country","opt":"nin","y":[]}`,
	"tag member":      `{"type":"string","x":{"type":"literal","value":"sale"},"opt":"in","y":{"type":"field","name":"tags"}}`,
	"tag not member":  `{"type":"string","x":{"type":"literal","value":"new"},"opt":"nin","y":{"type":"field","name":"tags"}}`,
	"len":             `{"type":"number","x":{"type":"len","x":"items"},"opt":"gte","y":2}`,
	"len tags zero":   `{"type":"number","x":{"type":"len","x":"tags"},"opt":"eq","y":0}`,
	"any item":        `{"type":"array","x":"items","opt":"any","y":{"type":"and","items":[{"type":"string","x":"category","opt":"eq","y":"book"},{"type":"number","x":"price","opt":"gt","y":50}]}}`,
	"all items":       `{"type":"array","x":"items","opt":"all","y":{"type":"number","x":"price","opt":"lt","y":60}}`,
	"none items":      `{"type":"array","x":"items","opt":"none","y":{"type":"string","x":"category","opt":"in","y":["food","toy"]}}`,
	"all tags":        `{"type":"array","x":"tags","opt":"all","y":{"type":"string","x":"$","opt":"ne","y":"NEW"}}`,
	"empty and/or":    `{"type":"or","items":[{"type":"and","items":[]},{"type":"or","items":[]}]}`,
	"item vs order":   `{"type":"array","x":"items","opt":"any","y":{"type":"string","x":"category","opt":"starts_with","y":"Bo"}}`,
	"complex":         `{"type":"and","items":[{"type":"or","items":[{"type":"boolean","x":"vip","opt":"eq","y":true},{"type":"number","x":"amount","opt":"gte","y":150}]},{"type":"string","x":"country","opt":"nin","y":["US"]},{"type":"array","x":"items","opt":"any","y":{"type":"number","x":"price","opt":"gte","y":10}},{"type":"array","x":"tags","opt":"none","y":{"type":"string","x":"$","opt":"eq","y":"gift"}}]}`,
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
	typed, err := s.CheckJSON([]byte(`{"type":"and","items":[{"type":"number","x":"amount","opt":"gte","y":100},{"type":"string","x":"note","opt":"starts_with","y":"50%"},{"type":"string","x":{"type":"literal","value":"sale"},"opt":"in","y":{"type":"field","name":"tags"}}]}`))
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
	typed, err := schema().CheckJSON([]byte(`{"type":"array","x":"items","opt":"any","y":{"type":"boolean","x":"vip","opt":"eq","y":true}}`))
	if err == nil {
		t.Fatal("predicate must not see parent fields")
	}
	typed, err = schema().CheckJSON([]byte(`{"type":"array","x":"items","opt":"any","y":{"type":"number","x":"price","opt":"gt","y":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sqlquery.Where(typed, sqlquery.Options{Table: "o"}); err == nil || !strings.Contains(err.Error(), "needs a Relation") {
		t.Fatalf("want missing relation error, got %v", err)
	}
}

// TestIdentifierQuoting checks that hostile column names cannot break out.
func TestIdentifierQuoting(t *testing.T) {
	typed, _ := schema().CheckJSON([]byte(`{"type":"string","x":"country","opt":"eq","y":"x' OR 1=1 --"}`))
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

// TestEveryOperatorCovered fails when an operator is added to the rule
// package but no differential rule exercises it here.
func TestEveryOperatorCovered(t *testing.T) {
	seen := map[rule.Op]bool{}
	for _, src := range rules {
		r, err := rule.Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		rule.Walk(r, func(n rule.Node) {
			switch n := n.(type) {
			case *rule.Compare:
				seen[n.Op] = true
			case *rule.Quant:
				seen[n.Op] = true
			case *rule.Arith:
				seen[n.Op] = true
			}
		})
	}
	for _, op := range rule.Ops() {
		if !seen[op] {
			t.Errorf("no differential rule uses %s", op)
		}
	}
}
