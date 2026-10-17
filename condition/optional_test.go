package condition

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/k0923/go/condition/rule"
)

// doc is the shape of schemaless data (xfield, Mongo documents).
type doc = map[string]any

func has(key string) func(doc) bool {
	return func(d doc) bool { v, ok := d[key]; return ok && v != nil }
}

func sub(key string) func(doc) doc {
	return func(d doc) doc { m, _ := d[key].(doc); return m }
}

// docSchema registers fields that may be missing, the way an adapter over
// xfield would. Accessors panic on a missing value, so the test also proves
// they are never called for one.
func docSchema() *Schema[doc] {
	buyer := NewSchema[doc]().
		Number("level", func(d doc) float64 { return d["level"].(float64) }).
		Optional("level", has("level"))
	return NewSchema[doc]().
		Number("amount", func(d doc) float64 { return d["amount"].(float64) }).
		Number("discount", func(d doc) float64 { return d["discount"].(float64) }).
		Optional("discount", has("discount")).
		String("note", func(d doc) string { return d["note"].(string) }).
		Optional("note", has("note")).
		Date("paid_at", func(d doc) time.Time { return d["paid_at"].(time.Time) }).
		Optional("paid_at", has("paid_at")).
		Strings("tags", func(d doc) []string { t, _ := d["tags"].([]string); return t }).
		Optional("tags", has("tags")).
		Object("buyer", sub("buyer"), buyer).
		Optional("buyer", has("buyer")).
		List("matrix", func(d doc) [][]float64 { m, _ := d["matrix"].([][]float64); return m },
			NewSchema[[]float64]().Numbers(SelfField, func(r []float64) []float64 { return r }))
}

func TestOptional(t *testing.T) {
	paid := time.Date(2024, 5, 1, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
	full := doc{"amount": 100.0, "discount": 10.0, "note": "gift", "paid_at": paid, "tags": []string{"vip"},
		"buyer": doc{"level": 3.0}, "matrix": [][]float64{{1, 2}, {3, 4}}}
	bare := doc{"amount": 100.0, "buyer": doc{}}
	null := doc{"amount": 100.0, "discount": nil, "note": nil, "buyer": nil}

	s := docSchema()
	cases := []struct {
		name, src        string
		full, bare, null bool
	}{
		{"exists", `{"type":"exists","x":"discount"}`, true, false, false},
		{"not exists", `{"type":"not","x":{"type":"exists","x":"note"}}`, false, true, true},
		{"exists required", `{"type":"exists","x":"amount"}`, true, true, true},
		{"exists object", `{"type":"exists","x":"buyer"}`, true, true, false},
		{"exists leaf", `{"type":"exists","x":"buyer.level"}`, true, false, false},
		{"eq", `{"type":"number","x":"discount","opt":"eq","y":10}`, true, false, false},
		{"ne", `{"type":"number","x":"discount","opt":"ne","y":10}`, false, true, true},
		{"lt", `{"type":"number","x":"discount","opt":"lt","y":50}`, true, false, false},
		{"gte", `{"type":"number","x":"discount","opt":"gte","y":0}`, true, false, false},
		{"in", `{"type":"number","x":"discount","opt":"in","y":[0,10]}`, true, false, false},
		{"nin", `{"type":"number","x":"discount","opt":"nin","y":[0,10]}`, false, true, true},
		{"starts_with", `{"type":"string","x":"note","opt":"starts_with","y":""}`, true, false, false},
		{"arith", `{"type":"number","x":{"type":"arith","x":"amount","opt":"sub","y":{"type":"field","name":"discount"}},"opt":"eq","y":90}`, true, false, false},
		{"field vs field", `{"type":"number","x":"amount","opt":"ne","y":{"type":"field","name":"discount"}}`, true, true, true},
		{"object leaf", `{"type":"number","x":"buyer.level","opt":"gte","y":1}`, true, false, false},
		{"missing list empty", `{"type":"number","x":{"type":"len","x":"tags"},"opt":"eq","y":0}`, false, true, true},
		{"none on missing list", `{"type":"array","x":"tags","opt":"none","y":{"type":"string","x":"$","opt":"eq","y":"vip"}}`, false, true, true},
		{"date gt", `{"type":"date","x":"paid_at","opt":"gt","y":"2024-04-30T23:59:59Z"}`, true, false, false},
		{"date eq other zone", `{"type":"date","x":"paid_at","opt":"eq","y":"2024-05-01T00:00:00Z"}`, true, false, false},
		{"date ne", `{"type":"date","x":"paid_at","opt":"ne","y":"2024-05-01T00:00:00Z"}`, false, true, true},
		{"list of lists", `{"type":"array","x":"matrix","opt":"any","y":{"type":"array","x":"$","opt":"all","y":{"type":"number","x":"$","opt":"gt","y":2}}}`, true, false, false},
		{"len of inner list", `{"type":"array","x":"matrix","opt":"all","y":{"type":"number","x":{"type":"len","x":"$"},"opt":"eq","y":2}}`, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := s.CompileJSON([]byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			for _, in := range []struct {
				name string
				d    doc
				want bool
			}{{"full", full, tc.full}, {"bare", bare, tc.bare}, {"null", null, tc.null}} {
				if got, err := c.Match(in.d); err != nil || got != in.want {
					t.Errorf("%s: got %v, %v; want %v", in.name, got, err, in.want)
				}
			}
		})
	}
}

func TestDateAndExistsRules(t *testing.T) {
	s := docSchema()
	for name, tc := range map[string]struct {
		src  string
		kind error
	}{
		"date contains":    {`{"type":"date","x":"paid_at","opt":"contains","y":"x"}`, ErrUnknownOp},
		"date in":          {`{"type":"date","x":"paid_at","opt":"in","y":[]}`, ErrUnknownOp},
		"date not rfc3339": {`{"type":"date","x":"paid_at","opt":"gt","y":"2024-05-01"}`, ErrTypeMismatch},
		"date number":      {`{"type":"date","x":"paid_at","opt":"gt","y":1714521600}`, ErrTypeMismatch},
		"date on number":   {`{"type":"date","x":"amount","opt":"gt","y":"2024-05-01T00:00:00Z"}`, ErrTypeMismatch},
		"exists literal":   {`{"type":"exists","x":{"type":"literal","value":1}}`, ErrTypeMismatch},
		"exists unknown":   {`{"type":"exists","x":"nope"}`, ErrUnknownField},
		"exists extra key": {`{"type":"exists","x":"note","y":1}`, ErrInvalidNode},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.CompileJSON([]byte(tc.src)); !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
		})
	}

	// Go-built date and exists nodes round-trip through JSON.
	c := &rule.And{Items: []rule.Cond{
		&rule.Exists{X: &rule.Field{Name: "note"}},
		&rule.Compare{Type: rule.TypeDate, X: &rule.Field{Name: "paid_at"}, Op: rule.OpLt,
			Y: &rule.Date{V: time.Date(2024, 5, 1, 8, 0, 0, 500, time.FixedZone("", 8*3600))}},
	}}
	data, _ := json.Marshal(c)
	want := `{"type":"and","items":[{"type":"exists","x":"note"},{"type":"date","x":"paid_at","opt":"lt","y":"2024-05-01T08:00:00.0000005+08:00"}]}`
	if string(data) != want {
		t.Fatalf("got  %s\nwant %s", data, want)
	}
	if _, err := s.CompileJSON(data); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalRegistration(t *testing.T) {
	for name, f := range map[string]func(){
		"unregistered": func() { NewSchema[doc]().Optional("x", has("x")) },
		"nil has": func() {
			NewSchema[doc]().Bool("x", func(doc) bool { return true }).Optional("x", nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			f()
		})
	}
	if f, _ := docSchema().Field("buyer.level"); !f.Optional {
		t.Fatal("leaf of an optional object must be optional")
	}
	if f, _ := docSchema().Field("amount"); f.Optional {
		t.Fatal("amount is required")
	}
}
