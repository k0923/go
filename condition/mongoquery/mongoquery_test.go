package mongoquery_test

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/k0923/go/condition"
	"github.com/k0923/go/condition/mongoquery"
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
	Scores  []float64
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
		Numbers("scores", func(o order) []float64 { return o.Scores }).
		List("items", func(o order) []item { return o.Items }, items)
}

// Document paths differ from schema names on purpose, to exercise Paths.
var opts = mongoquery.Options{Paths: map[string]string{
	"amount": "total", "items": "lines", "items.price": "unit_price",
}}

func toDoc(o order) map[string]any {
	tags := make([]any, len(o.Tags))
	for i, t := range o.Tags {
		tags[i] = t
	}
	scores := make([]any, len(o.Scores))
	for i, s := range o.Scores {
		scores[i] = s
	}
	lines := make([]any, len(o.Items))
	for i, it := range o.Items {
		lines[i] = map[string]any{"category": it.Category, "unit_price": it.Price}
	}
	d := map[string]any{
		"id": float64(o.ID), "total": o.Amount, "country": o.Country, "note": o.Note,
		"vip": o.VIP, "tags": tags, "scores": scores, "lines": lines,
	}
	if len(o.Tags) == 0 && o.ID%2 == 0 {
		delete(d, "tags") // a missing array must behave like an empty one
	}
	return d
}

func fixture() []order {
	r := rand.New(rand.NewPCG(3, 4))
	countries := []string{"CN", "cn", "SG", "US", "C"}
	notes := []string{"", "1.5*off", "VIP gift", "vip", "a(b)", "gift wrap", "$x", "^g"}
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
		for range r.IntN(3) {
			o.Scores = append(o.Scores, float64(r.IntN(100)))
		}
		for range r.IntN(4) {
			o.Items = append(o.Items, item{Category: cats[r.IntN(len(cats))], Price: float64(r.IntN(100))})
		}
		out = append(out, o)
	}
	return out
}

var rules = map[string]string{
	"le sub":           `{"op":"le","args":[{"op":"sub","args":[{"field":"amount"},{"field":"id"}]},{"value":120}]}`,
	"le string":        `{"op":"le","args":[{"value":"SG"},{"field":"country"}]}`,
	"compare":          `{"op":"and","args":[{"op":"ge","args":[{"field":"amount"},{"value":100}]},{"op":"lt","args":[{"field":"country"},{"value":"SG"}]}]}`,
	"flipped":          `{"op":"lt","args":[{"value":100},{"field":"amount"}]}`,
	"bool field":       `{"op":"or","args":[{"field":"vip"},{"op":"eq","args":[{"field":"vip"},{"value":false}]}]}`,
	"bool eq expr":     `{"op":"eq","args":[{"field":"vip"},{"op":"gt","args":[{"field":"amount"},{"value":50}]}]}`,
	"not":              `{"op":"not","args":[{"op":"in","args":[{"field":"country"},{"value":["CN","SG"]}]}]}`,
	"regex meta":       `{"op":"contains","args":[{"field":"note"},{"value":".5*"}]}`,
	"starts_with":      `{"op":"starts_with","args":[{"field":"note"},{"value":"^g"}]}`,
	"ends_with":        `{"op":"ends_with","args":[{"field":"note"},{"value":"(b)"}]}`,
	"ends_with field":  `{"op":"ends_with","args":[{"field":"country"},{"field":"note"}]}`,
	"dollar literal":   `{"op":"any","args":[{"field":"tags"},{"op":"ne","args":[{"field":"$"},{"value":"$note"}]}]}`,
	"dollar str expr":  `{"op":"or","args":[{"op":"eq","args":[{"field":"note"},{"field":"country"}]},{"op":"starts_with","args":[{"field":"note"},{"value":"$x"}]}]}`,
	"field vs field":   `{"op":"ge","args":[{"field":"note"},{"field":"country"}]}`,
	"arith":            `{"op":"gt","args":[{"op":"div","args":[{"op":"mul","args":[{"field":"amount"},{"value":3}]},{"value":2}]},{"op":"add","args":[{"field":"id"},{"value":0.5}]}]}`,
	"in empty":         `{"op":"in","args":[{"field":"country"},{"value":[]}]}`,
	"not_in empty":     `{"op":"not_in","args":[{"field":"country"},{"value":[]}]}`,
	"tag member":       `{"op":"in","args":[{"value":"sale"},{"field":"tags"}]}`,
	"tag not member":   `{"op":"not_in","args":[{"value":"new"},{"field":"tags"}]}`,
	"len":              `{"op":"ge","args":[{"op":"len","args":[{"field":"items"}]},{"value":2}]}`,
	"len tags zero":    `{"op":"eq","args":[{"op":"len","args":[{"field":"tags"}]},{"value":0}]}`,
	"any item":         `{"op":"any","args":[{"field":"items"},{"op":"and","args":[{"op":"eq","args":[{"field":"category"},{"value":"book"}]},{"op":"gt","args":[{"field":"price"},{"value":50}]}]}]}`,
	"all items":        `{"op":"all","args":[{"field":"items"},{"op":"lt","args":[{"field":"price"},{"value":60}]}]}`,
	"none items":       `{"op":"none","args":[{"field":"items"},{"op":"in","args":[{"field":"category"},{"value":["food","toy"]}]}]}`,
	"all items expr":   `{"op":"all","args":[{"field":"items"},{"op":"lt","args":[{"op":"mul","args":[{"field":"price"},{"value":2}]},{"value":100}]}]}`,
	"all tags":         `{"op":"all","args":[{"field":"tags"},{"op":"ne","args":[{"field":"$"},{"value":"NEW"}]}]}`,
	"any scores":       `{"op":"any","args":[{"field":"scores"},{"op":"ge","args":[{"field":"$"},{"value":90}]}]}`,
	"none tags prefix": `{"op":"none","args":[{"field":"tags"},{"op":"starts_with","args":[{"field":"$"},{"value":"ne"}]}]}`,
	"empty and/or":     `{"op":"or","args":[{"op":"and"},{"op":"or"}]}`,
	"mixed native+expr": `{"op":"and","args":[
		{"op":"or","args":[{"field":"vip"},{"op":"ge","args":[{"field":"amount"},{"value":150}]}]},
		{"op":"not_in","args":[{"field":"country"},{"value":["US"]}]},
		{"op":"gt","args":[{"op":"len","args":[{"field":"items"}]},{"op":"len","args":[{"field":"tags"}]}]},
		{"op":"none","args":[{"field":"tags"},{"op":"eq","args":[{"field":"$"},{"value":"gift"}]}]}]}`,
}

// TestDifferential compares in-memory evaluation with the generated filter
// interpreted by the reference evaluator, after a JSON round trip.
func TestDifferential(t *testing.T) {
	orders := fixture()
	s := schema()
	for name, src := range rules {
		t.Run(name, func(t *testing.T) {
			typed, err := s.CheckJSON([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			match, _ := s.CompileJSON([]byte(src))
			f, err := mongoquery.Filter(typed, opts)
			if err != nil {
				t.Fatal(err)
			}
			// Round-trip through JSON: what a driver or a mongosh user would see.
			raw, _ := json.Marshal(f)
			var filter map[string]any
			if err := json.Unmarshal(raw, &filter); err != nil {
				t.Fatal(err)
			}
			for _, o := range orders {
				want, _ := match.Match(o)
				if got := evalFilter(filter, toDoc(o)); got != want {
					t.Fatalf("order %d: memory=%v mongo=%v\nfilter: %s\norder: %+v", o.ID, want, got, raw, o)
				}
			}
		})
	}
}

func TestNativeOperatorsPreferred(t *testing.T) {
	typed, _ := schema().CheckJSON([]byte(`{"op":"and","args":[
		{"op":"ge","args":[{"field":"amount"},{"value":100}]},
		{"op":"in","args":[{"field":"country"},{"value":["CN","SG"]}]},
		{"op":"starts_with","args":[{"field":"note"},{"value":"a.b"}]},
		{"op":"any","args":[{"field":"items"},{"op":"gt","args":[{"field":"price"},{"value":50}]}]},
		{"op":"gt","args":[{"op":"len","args":[{"field":"tags"}]},{"value":1}]}]}`))
	f, err := mongoquery.Filter(typed, opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f)
	want := `{"$and":[{"total":{"$gte":100}},{"country":{"$in":["CN","SG"]}},{"note":{"$regex":"\\Aa\\.b"}},{"lines":{"$elemMatch":{"unit_price":{"$gt":50}}}},{"$expr":{"$gt":[{"$size":{"$ifNull":["$tags",{"$literal":[]}]}},1]}}]}`
	if string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
}

// FuzzDifferential: any checked rule must translate and agree with Match
// under the reference evaluator.
func FuzzDifferential(f *testing.F) {
	for _, src := range rules {
		f.Add([]byte(src))
	}
	orders := fixture()[:60]
	s := schema()
	f.Fuzz(func(t *testing.T, data []byte) {
		typed, err := s.CheckJSON(data)
		if err != nil {
			return
		}
		match, _ := s.CompileJSON(data)
		filter, err := mongoquery.Filter(typed, opts)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(filter)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		for _, o := range orders {
			want, err := match.Match(o)
			if err != nil {
				return // division by zero: Mongo errors as well
			}
			if got := evalFilter(decoded, toDoc(o)); got != want {
				t.Fatalf("order %d: memory=%v mongo=%v\nrule: %s\nfilter: %s", o.ID, want, got, data, raw)
			}
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
