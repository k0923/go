package condition

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/k0923/go/condition/rule"
)

func num(name string, op rule.Op, y rule.Expr) *rule.Compare {
	return &rule.Compare{Type: rule.TypeNumber, X: &rule.Field{Name: name}, Op: op, Y: y}
}

// macroSchema: user-level macros, an item-level macro used inside array
// rules, and macros lifted through Object.
func macroSchema() *Schema[order] {
	items := itemSchema().
		Macro("cheap", nil, func([]rule.Expr) rule.Cond {
			return num("price", rule.OpLt, &rule.Number{V: 10})
		})
	users := NewSchema[user]().
		Number("age", func(u user) int { return u.Age }).
		String("city", func(u user) string { return u.City }).
		Bool("vip", func(u user) bool { return u.VIP }).
		List("items", func(u user) []item { return u.Items }, items).
		Macro("adult", nil, func([]rule.Expr) rule.Cond {
			return num("age", rule.OpGte, &rule.Number{V: 18})
		}).
		// older_than(n): age > n. The argument may be a literal or any
		// number expression of the caller, including another field.
		Macro("older_than", []Kind{KindNumber}, func(a []rule.Expr) rule.Cond {
			return &rule.Compare{Type: rule.TypeNumber, X: &rule.Field{Name: "age"}, Op: rule.OpGt, Y: a[0]}
		}).
		// valued(cities): an adult VIP in one of cities with a cheap item.
		// Macros compose: it calls adult and the element macro cheap.
		Macro("valued", []Kind{KindStrings}, func(a []rule.Expr) rule.Cond {
			return &rule.And{Items: []rule.Cond{
				&rule.Call{Name: "adult"},
				&rule.Compare{Type: rule.TypeBoolean, X: &rule.Field{Name: "vip"}, Op: rule.OpEq, Y: &rule.Bool{V: true}},
				&rule.Compare{Type: rule.TypeString, X: &rule.Field{Name: "city"}, Op: rule.OpIn, Y: a[0]},
				&rule.Quant{X: &rule.Field{Name: "items"}, Op: rule.OpAny, Y: &rule.Call{Name: "cheap"}},
			}}
		})
	type order2 = order
	return NewSchema[order2]().
		Object("buyer", func(o order) buyer { return o.Buyer }, NewSchema[buyer]().
			Number("level", func(b buyer) int { return b.Level }).
			Object("owner", func(b buyer) user { return b.Owner }, users)).
		Object("seller", func(o order) user { return o.Seller }, users)
}

func TestMacro(t *testing.T) {
	s := macroSchema()
	young := user{Age: 16, City: "bj", VIP: true, Items: []item{{Price: 5}}}
	o := order{Buyer: buyer{Level: 2, Owner: alice}, Seller: young}
	for name, tc := range map[string]struct {
		src  string
		want bool
	}{
		"no args":         {`{"type":"call","name":"seller.adult"}`, false},
		"lifted twice":    {`{"type":"call","name":"buyer.owner.adult"}`, true},
		"literal arg":     {`{"type":"call","name":"buyer.owner.older_than","args":[29]}`, true},
		"field arg":       {`{"type":"call","name":"seller.older_than","args":[{"type":"field","name":"buyer.level"}]}`, true},
		"expr arg":        {`{"type":"call","name":"buyer.owner.older_than","args":[{"type":"arith","x":"seller.age","opt":"mul","y":2}]}`, false},
		"composed":        {`{"type":"call","name":"buyer.owner.valued","args":[["sh","bj"]]}`, true},
		"composed fails":  {`{"type":"call","name":"seller.valued","args":[["bj"]]}`, false},
		"empty list arg":  {`{"type":"call","name":"buyer.owner.valued","args":[[]]}`, false},
		"in a rule":       {`{"type":"or","items":[{"type":"call","name":"seller.adult"},{"type":"not","x":{"type":"call","name":"buyer.owner.valued","args":[["gz"]]}}]}`, true},
		"element macro":   {`{"type":"array","x":"seller.items","opt":"all","y":{"type":"call","name":"cheap"}}`, true},
		"element macro 2": {`{"type":"array","x":"buyer.owner.items","opt":"all","y":{"type":"call","name":"cheap"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := s.CompileJSON([]byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if got, err := c.Match(o); err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestMacroErrors(t *testing.T) {
	s := macroSchema().
		Macro("loop", nil, func([]rule.Expr) rule.Cond { return &rule.Call{Name: "loop"} }).
		Macro("bad_field", nil, func([]rule.Expr) rule.Cond { return num("nope", rule.OpEq, &rule.Number{V: 1}) }).
		Macro("nothing", nil, func([]rule.Expr) rule.Cond { return nil }).
		Macro("since", []Kind{KindDate}, func(a []rule.Expr) rule.Cond { return &rule.And{} })
	for name, tc := range map[string]struct {
		src, path string
		kind      error
	}{
		"unknown":         {`{"type":"call","name":"adult"}`, "$.name", ErrUnknownMacro},
		"outside element": {`{"type":"call","name":"cheap"}`, "$.name", ErrUnknownMacro},
		"arity":           {`{"type":"call","name":"seller.older_than"}`, "$.args", ErrTypeMismatch},
		"arg kind":        {`{"type":"call","name":"seller.older_than","args":["18"]}`, "$.args[0]", ErrTypeMismatch},
		"arg field kind":  {`{"type":"call","name":"seller.older_than","args":[{"type":"field","name":"seller.city"}]}`, "$.args[0]", ErrTypeMismatch},
		"arg date":        {`{"type":"call","name":"since","args":["2024-05-01"]}`, "$.args[0]", ErrTypeMismatch},
		"recursion":       {`{"type":"call","name":"loop"}`, "", ErrTooDeep},
		"bad expansion":   {`{"type":"and","items":[{"type":"call","name":"bad_field"}]}`, "$.items[0]<bad_field>.x", ErrUnknownField},
		"nil expansion":   {`{"type":"call","name":"nothing"}`, "$", ErrInvalidNode},
		"parse name":      {`{"type":"call","name":""}`, "$.name", ErrInvalidNode},
		"parse args":      {`{"type":"call","name":"x","args":{}}`, "$.args", ErrInvalidNode},
		"parse arg":       {`{"type":"call","name":"x","args":[null]}`, "$.args[0]", ErrTypeMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.CompileJSON([]byte(tc.src))
			var ce *CompileError
			if !errors.As(err, &ce) || !errors.Is(err, tc.kind) || tc.path != "" && ce.Path != tc.path {
				t.Fatalf("want %v at %q, got %v", tc.kind, tc.path, err)
			}
		})
	}
	if _, err := s.CompileJSON([]byte(`{"type":"call","name":"since","args":["2024-05-01T00:00:00Z"]}`)); err != nil {
		t.Fatalf("RFC 3339 string must convert to a date argument: %v", err)
	}
}

// TestMacroChecked: backends see the expansion, never a Call, and the caller
// rule is left untouched.
func TestMacroChecked(t *testing.T) {
	s := macroSchema()
	call := &rule.Call{Name: "buyer.owner.older_than", Args: []rule.Expr{&rule.Number{V: 3}}}
	src := &rule.Not{X: call}
	chk, err := s.Check(src)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(chk.Root())
	if want := `{"type":"not","x":{"type":"number","x":"buyer.owner.age","opt":"gt","y":3}}`; string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if src.X != call {
		t.Fatal("Check modified the caller's tree")
	}
	var cmp *rule.Compare
	rule.Walk(chk.Root(), func(n rule.Node) {
		if c, ok := n.(*rule.Compare); ok {
			cmp = c
		}
	})
	if p := chk.Path(cmp.X); p != "$.x<buyer.owner.older_than>.x" {
		t.Fatalf("path %q", p)
	}
	if chk.Path(cmp.Y) != "$.x.args[0]" {
		t.Fatalf("argument keeps the caller's path, got %q", chk.Path(cmp.Y))
	}
	data, _ := json.Marshal(src)
	if !strings.Contains(string(data), `{"type":"call","name":"buyer.owner.older_than","args":[3]}`) {
		t.Fatalf("call marshal: %s", data)
	}
	back, err := rule.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(back)
	if string(again) != string(data) {
		t.Fatalf("not canonical: %s vs %s", data, again)
	}
}

func TestMacroRegistration(t *testing.T) {
	cond := func([]rule.Expr) rule.Cond { return &rule.And{} }
	for name, f := range map[string]func(){
		"empty":       func() { NewSchema[user]().Macro("", nil, cond) },
		"nil":         func() { NewSchema[user]().Macro("m", nil, nil) },
		"dup":         func() { NewSchema[user]().Macro("m", nil, cond).Macro("m", nil, cond) },
		"object kind": func() { NewSchema[user]().Macro("m", []Kind{KindObjects}, cond) },
		"zero kind":   func() { NewSchema[user]().Macro("m", []Kind{0}, cond) },
		"lift clash": func() {
			u := NewSchema[user]().Macro("m", nil, cond)
			NewSchema[order]().Macro("seller.m", nil, cond).Object("seller", func(o order) user { return o.Seller }, u)
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
}
