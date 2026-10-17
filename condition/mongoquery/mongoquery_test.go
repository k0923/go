package mongoquery_test

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

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

// TestObjectFields: fields registered with Schema.Object are dotted paths,
// which MongoDB resolves into embedded documents natively.
func TestObjectFields(t *testing.T) {
	type buyer struct {
		Level int
		Items []item
	}
	type withBuyer struct{ Buyer buyer }
	items := condition.NewSchema[item]().Number("price", func(i item) float64 { return i.Price })
	s := condition.NewSchema[withBuyer]().Object("buyer", func(o withBuyer) buyer { return o.Buyer },
		condition.NewSchema[buyer]().
			Number("level", func(b buyer) int { return b.Level }).
			List("items", func(b buyer) []item { return b.Items }, items))
	src := `{"type":"and","items":[{"type":"number","x":"buyer.level","opt":"gte","y":3},{"type":"array","x":"buyer.items","opt":"any","y":{"type":"number","x":"price","opt":"gt","y":50}}]}`
	typed, err := s.CheckJSON([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	match, _ := s.Compile(typed.Root())
	f, err := mongoquery.Filter(typed, mongoquery.Options{Paths: map[string]string{"buyer.items.price": "unit_price"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f)
	if want := `{"$and":[{"buyer.level":{"$gte":3}},{"buyer.items":{"$elemMatch":{"unit_price":{"$gt":50}}}}]}`; string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	for _, o := range []withBuyer{
		{buyer{Level: 3, Items: []item{{Price: 60}}}},
		{buyer{Level: 3, Items: []item{{Price: 40}}}},
		{buyer{Level: 1, Items: []item{{Price: 60}}}},
		{buyer{Level: 5}},
	} {
		lines := make([]any, len(o.Buyer.Items))
		for i, it := range o.Buyer.Items {
			lines[i] = map[string]any{"unit_price": it.Price}
		}
		d := map[string]any{"buyer": map[string]any{"level": float64(o.Buyer.Level), "items": lines}}
		want, _ := match.Match(o)
		if got := evalFilter(decoded, d); got != want {
			t.Fatalf("%+v: memory=%v mongo=%v", o, want, got)
		}
	}
}

// TestOptionalDifferential runs rules over documents whose optional fields
// are missing, null or set, through both the native and the $expr paths,
// and requires memory and Mongo to agree. Filters are evaluated without a
// JSON round trip so dates stay time.Time.
func TestOptionalDifferential(t *testing.T) {
	type doc = map[string]any
	has := func(k string) func(doc) bool { return func(d doc) bool { v, ok := d[k]; return ok && v != nil } }
	num := func(k string) func(doc) float64 { return func(d doc) float64 { return d[k].(float64) } }
	buyer := condition.NewSchema[doc]().Number("level", num("level")).Optional("level", has("level"))
	s := condition.NewSchema[doc]().
		Number("amount", num("amount")).
		Number("discount", num("discount")).Optional("discount", has("discount")).
		String("note", func(d doc) string { return d["note"].(string) }).Optional("note", has("note")).
		Date("paid_at", func(d doc) time.Time { return d["paid_at"].(time.Time) }).Optional("paid_at", has("paid_at")).
		Date("due_at", func(d doc) time.Time { return d["due_at"].(time.Time) }).
		Strings("tags", func(d doc) []string {
			out := []string{}
			for _, v := range asSlice(d["tags"]) {
				out = append(out, v.(string))
			}
			return out
		}).Optional("tags", has("tags")).
		Object("buyer", func(d doc) doc { m, _ := d["buyer"].(doc); return m }, buyer).Optional("buyer", has("buyer"))

	due := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	var docs []doc
	for i := range 81 {
		d := doc{"amount": float64(i % 7 * 10), "due_at": due}
		// Each optional field cycles through missing, null and two values.
		set := func(k string, step int, a, b any) {
			switch (i / step) % 4 {
			case 1:
				d[k] = nil
			case 2:
				d[k] = a
			case 3:
				d[k] = b
			}
		}
		set("discount", 1, 10.0, 0.0)
		set("note", 4, "gift", "")
		set("paid_at", 16, due.Add(-time.Hour), due.Add(time.Hour))
		set("tags", 2, []any{"vip"}, []any{})
		switch i % 3 {
		case 1:
			d["buyer"] = doc{}
		case 2:
			d["buyer"] = doc{"level": float64(i % 5)}
		}
		docs = append(docs, d)
	}

	rules := []string{
		`{"type":"exists","x":"discount"}`,
		`{"type":"exists","x":"buyer"}`,
		`{"type":"exists","x":"buyer.level"}`,
		`{"type":"not","x":{"type":"exists","x":"note"}}`,
		`{"type":"number","x":"discount","opt":"eq","y":10}`,
		`{"type":"number","x":"discount","opt":"ne","y":10}`,
		`{"type":"number","x":"discount","opt":"lt","y":5}`,
		`{"type":"number","x":"discount","opt":"nin","y":[0]}`,
		`{"type":"number","x":"buyer.level","opt":"gte","y":2}`,
		`{"type":"string","x":"note","opt":"starts_with","y":""}`,
		`{"type":"string","x":"note","opt":"ne","y":"gift"}`,
		`{"type":"date","x":"paid_at","opt":"gt","y":"2024-05-01T08:00:00+08:00"}`,
		`{"type":"date","x":"paid_at","opt":"ne","y":"2024-04-30T23:00:00Z"}`,
		// $expr paths: field vs field, arithmetic, len.
		`{"type":"date","x":"paid_at","opt":"lt","y":{"type":"field","name":"due_at"}}`,
		`{"type":"date","x":"paid_at","opt":"ne","y":{"type":"field","name":"due_at"}}`,
		`{"type":"number","x":{"type":"arith","x":"amount","opt":"sub","y":{"type":"field","name":"discount"}},"opt":"gte","y":20}`,
		`{"type":"number","x":{"type":"arith","x":"amount","opt":"sub","y":{"type":"field","name":"discount"}},"opt":"ne","y":20}`,
		`{"type":"number","x":"discount","opt":"lt","y":{"type":"field","name":"amount"}}`,
		`{"type":"number","x":{"type":"len","x":"tags"},"opt":"eq","y":0}`,
		`{"type":"array","x":"tags","opt":"none","y":{"type":"string","x":"$","opt":"eq","y":"vip"}}`,
		`{"type":"string","x":{"type":"literal","value":"vip"},"opt":"nin","y":{"type":"field","name":"tags"}}`,
		`{"type":"or","items":[{"type":"number","x":"discount","opt":"ne","y":{"type":"field","name":"amount"}},{"type":"exists","x":"note"}]}`,
	}
	for _, src := range rules {
		typed, err := s.CheckJSON([]byte(src))
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		match := s.Build(typed)
		filter, err := mongoquery.Filter(typed, mongoquery.Options{})
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		for _, d := range docs {
			want, err := match.Match(d)
			if err != nil {
				t.Fatalf("%s: %v", src, err)
			}
			if got := evalFilter(filter, d); got != want {
				raw, _ := json.Marshal(filter)
				t.Fatalf("memory=%v mongo=%v\nrule:   %s\nfilter: %s\ndoc:    %v", want, got, src, raw, d)
			}
		}
	}
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

// TestListOfLists: an inner list is the element "$" itself, which has no
// document path, so it is translated through $map and must agree with Match.
func TestListOfLists(t *testing.T) {
	type doc = map[string]any
	rows := condition.NewSchema[[]float64]().Numbers(condition.SelfField, func(r []float64) []float64 { return r })
	s := condition.NewSchema[doc]().List("m", func(d doc) [][]float64 {
		var out [][]float64
		for _, r := range asSlice(d["m"]) {
			row := []float64{}
			for _, v := range asSlice(r) {
				row = append(row, v.(float64))
			}
			out = append(out, row)
		}
		return out
	}, rows)
	docs := []doc{{}, {"m": []any{}}, {"m": []any{[]any{}}}, {"m": []any{[]any{1.0, 5.0}, []any{3.0}}}, {"m": []any{[]any{4.0, 5.0}}}}
	for _, src := range []string{
		`{"type":"array","x":"m","opt":"any","y":{"type":"array","x":"$","opt":"all","y":{"type":"number","x":"$","opt":"gt","y":2}}}`,
		`{"type":"array","x":"m","opt":"all","y":{"type":"array","x":"$","opt":"any","y":{"type":"number","x":"$","opt":"eq","y":5}}}`,
		`{"type":"array","x":"m","opt":"none","y":{"type":"number","x":{"type":"len","x":"$"},"opt":"gte","y":2}}`,
	} {
		typed, err := s.CheckJSON([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		filter, err := mongoquery.Filter(typed, mongoquery.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range docs {
			want, _ := s.Build(typed).Match(d)
			if got := evalFilter(filter, d); got != want {
				raw, _ := json.Marshal(filter)
				t.Fatalf("%v: memory=%v mongo=%v\n%s\n%s", d, want, got, src, raw)
			}
		}
	}
}

// TestPrefix: with Prefix every top-level path moves under it, element paths
// stay relative, and results still agree with Match on nested documents.
func TestPrefix(t *testing.T) {
	s := schema()
	opt := mongoquery.Options{Paths: opts.Paths, Prefix: "data"}
	nest := func(o order) map[string]any { return map[string]any{"data": toDoc(o)} }
	for name, src := range rules {
		typed, err := s.CheckJSON([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		filter, err := mongoquery.Filter(typed, opt)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(filter)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		match := s.Build(typed)
		for _, o := range fixture() {
			want, err := match.Match(o)
			if err != nil {
				continue
			}
			if got := evalFilter(decoded, nest(o)); got != want {
				t.Fatalf("%s: order %d: memory=%v mongo=%v\n%s", name, o.ID, want, got, raw)
			}
		}
	}
	typed, _ := s.CheckJSON([]byte(`{"type":"and","items":[{"type":"number","x":"amount","opt":"gte","y":100},{"type":"array","x":"items","opt":"any","y":{"type":"number","x":"price","opt":"gt","y":{"type":"arith","x":1,"opt":"add","y":1}}},{"type":"number","x":{"type":"len","x":"tags"},"opt":"gt","y":1}]}`))
	f, _ := mongoquery.Filter(typed, opt)
	raw, _ := json.Marshal(f)
	want := `{"$and":[{"data.total":{"$gte":100}},{"$expr":{"$anyElementTrue":[{"$map":{"as":"e1","in":{"$gt":["$$e1.unit_price",{"$add":[1,1]}]},"input":{"$ifNull":["$data.lines",{"$literal":[]}]}}}]}},{"$expr":{"$gt":[{"$size":{"$ifNull":["$data.tags",{"$literal":[]}]}},1]}}]}`
	if string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
}
