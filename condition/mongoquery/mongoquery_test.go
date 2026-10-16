package mongoquery_test

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/k0923/go/condition"
	"github.com/k0923/go/condition/mongoquery"
	"github.com/k0923/go/condition/rule"
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
	"le sub":            `{"type":"number","x":{"type":"arith","x":"amount","opt":"sub","y":{"type":"field","name":"id"}},"opt":"lte","y":120}`,
	"le string":         `{"type":"string","x":{"type":"literal","value":"SG"},"opt":"lte","y":{"type":"field","name":"country"}}`,
	"compare":           `{"type":"and","items":[{"type":"number","x":"amount","opt":"gte","y":100},{"type":"string","x":"country","opt":"lt","y":"SG"}]}`,
	"flipped":           `{"type":"number","x":100,"opt":"lt","y":{"type":"field","name":"amount"}}`,
	"bool field":        `{"type":"or","items":[{"type":"boolean","x":"vip","opt":"eq","y":true},{"type":"boolean","x":"vip","opt":"eq","y":false}]}`,
	"bool eq expr":      `{"type":"boolean","x":"vip","opt":"ne","y":false}`,
	"not":               `{"type":"not","x":{"type":"string","x":"country","opt":"in","y":["CN","SG"]}}`,
	"regex meta":        `{"type":"string","x":"note","opt":"contains","y":".5*"}`,
	"starts_with":       `{"type":"string","x":"note","opt":"starts_with","y":"^g"}`,
	"ends_with":         `{"type":"string","x":"note","opt":"ends_with","y":"(b)"}`,
	"ends_with field":   `{"type":"string","x":"country","opt":"ends_with","y":{"type":"field","name":"note"}}`,
	"dollar literal":    `{"type":"array","x":"tags","opt":"any","y":{"type":"string","x":"$","opt":"ne","y":"$note"}}`,
	"dollar str expr":   `{"type":"or","items":[{"type":"string","x":"note","opt":"eq","y":{"type":"field","name":"country"}},{"type":"string","x":"note","opt":"starts_with","y":"$x"}]}`,
	"field vs field":    `{"type":"string","x":"note","opt":"gte","y":{"type":"field","name":"country"}}`,
	"arith":             `{"type":"number","x":{"type":"arith","x":{"type":"arith","x":"amount","opt":"mul","y":3},"opt":"div","y":2},"opt":"gt","y":{"type":"arith","x":"id","opt":"add","y":0.5}}`,
	"in empty":          `{"type":"string","x":"country","opt":"in","y":[]}`,
	"not_in empty":      `{"type":"string","x":"country","opt":"nin","y":[]}`,
	"tag member":        `{"type":"string","x":{"type":"literal","value":"sale"},"opt":"in","y":{"type":"field","name":"tags"}}`,
	"tag not member":    `{"type":"string","x":{"type":"literal","value":"new"},"opt":"nin","y":{"type":"field","name":"tags"}}`,
	"len":               `{"type":"number","x":{"type":"len","x":"items"},"opt":"gte","y":2}`,
	"len tags zero":     `{"type":"number","x":{"type":"len","x":"tags"},"opt":"eq","y":0}`,
	"any item":          `{"type":"array","x":"items","opt":"any","y":{"type":"and","items":[{"type":"string","x":"category","opt":"eq","y":"book"},{"type":"number","x":"price","opt":"gt","y":50}]}}`,
	"all items":         `{"type":"array","x":"items","opt":"all","y":{"type":"number","x":"price","opt":"lt","y":60}}`,
	"none items":        `{"type":"array","x":"items","opt":"none","y":{"type":"string","x":"category","opt":"in","y":["food","toy"]}}`,
	"all items expr":    `{"type":"array","x":"items","opt":"all","y":{"type":"number","x":{"type":"arith","x":"price","opt":"mul","y":2},"opt":"lt","y":100}}`,
	"all tags":          `{"type":"array","x":"tags","opt":"all","y":{"type":"string","x":"$","opt":"ne","y":"NEW"}}`,
	"any scores":        `{"type":"array","x":"scores","opt":"any","y":{"type":"number","x":"$","opt":"gte","y":90}}`,
	"none tags prefix":  `{"type":"array","x":"tags","opt":"none","y":{"type":"string","x":"$","opt":"starts_with","y":"ne"}}`,
	"empty and/or":      `{"type":"or","items":[{"type":"and","items":[]},{"type":"or","items":[]}]}`,
	"mixed native+expr": `{"type":"and","items":[{"type":"or","items":[{"type":"boolean","x":"vip","opt":"eq","y":true},{"type":"number","x":"amount","opt":"gte","y":150}]},{"type":"string","x":"country","opt":"nin","y":["US"]},{"type":"number","x":{"type":"len","x":"items"},"opt":"gt","y":{"type":"len","x":"tags"}},{"type":"array","x":"tags","opt":"none","y":{"type":"string","x":"$","opt":"eq","y":"gift"}}]}`,
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
	typed, _ := schema().CheckJSON([]byte(`{"type":"and","items":[{"type":"number","x":"amount","opt":"gte","y":100},{"type":"string","x":"country","opt":"in","y":["CN","SG"]},{"type":"string","x":"note","opt":"starts_with","y":"a.b"},{"type":"array","x":"items","opt":"any","y":{"type":"number","x":"price","opt":"gt","y":50}},{"type":"number","x":{"type":"len","x":"tags"},"opt":"gt","y":1}]}`))
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
