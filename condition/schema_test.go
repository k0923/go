package condition

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/k0923/go/condition/rule"
)

func itemSchema() *Schema[item] {
	return NewSchema[item]().
		Number("price", func(i item) float64 { return i.Price }).
		Strings("tags", func(i item) []string { return i.Tags })
}

func userSchema() *Schema[user] {
	return NewSchema[user]().
		Number("age", func(u user) int { return u.Age }).
		String("name", func(u user) string { return u.Name }).
		String("city", func(u user) string { return u.City }).
		Bool("vip", func(u user) bool { return u.VIP }).
		Numbers("scores", func(u user) []float64 { return u.Scores }).
		Strings("tags", func(u user) []string { return u.Tags }).
		List("items", func(u user) []item { return u.Items }, itemSchema())
}

var alice = user{
	Age: 30, Name: "alice", City: "bj", VIP: true,
	Scores: []float64{80, 95},
	Tags:   []string{"new", "beta"},
	Items:  []item{{Price: 5, Tags: []string{"sale"}}, {Price: 20}},
}

func TestCompileJSON(t *testing.T) {
	s := userSchema()
	cases := []struct {
		name, src string
		want      bool
	}{
		{"gte", `{"type":"number","x":"age","opt":"gte","y":18}`, true},
		{"bool eq", `{"type":"boolean","x":"vip","opt":"eq","y":false}`, false},
		{"not", `{"type":"not","x":{"type":"boolean","x":"vip","opt":"eq","y":true}}`, false},
		{"string order", `{"type":"string","x":"name","opt":"lt","y":"bob"}`, true},
		{"starts_with", `{"type":"string","x":"name","opt":"starts_with","y":"al"}`, true},
		{"in literal", `{"type":"string","x":"city","opt":"in","y":["sh","bj"]}`, true},
		{"in empty", `{"type":"string","x":"city","opt":"in","y":[]}`, false},
		{"nin empty", `{"type":"number","x":"age","opt":"nin","y":[]}`, true},
		{"in number list", `{"type":"number","x":"age","opt":"in","y":[1,30]}`, true},
		{"in field list", `{"type":"string","x":{"type":"literal","value":"beta"},"opt":"in","y":{"type":"field","name":"tags"}}`, true},
		{"field vs field", `{"type":"string","x":"name","opt":"gt","y":{"type":"field","name":"city"}}`, false},
		{"explicit literal y", `{"type":"number","x":"age","opt":"eq","y":{"type":"literal","value":30}}`, true},
		{"explicit field x", `{"type":"number","x":{"type":"field","name":"age"},"opt":"eq","y":30}`, true},
		{"literal on the left", `{"type":"number","x":18,"opt":"lt","y":{"type":"field","name":"age"}}`, true},
		{"and empty", `{"type":"and","items":[]}`, true},
		{"and no items", `{"type":"and"}`, true},
		{"or empty", `{"type":"or"}`, false},
		{"any object", `{"type":"array","x":"items","opt":"any","y":{"type":"number","x":"price","opt":"lt","y":10}}`, true},
		{"all object", `{"type":"array","x":"items","opt":"all","y":{"type":"number","x":"price","opt":"lt","y":10}}`, false},
		{"nested quant", `{"type":"array","x":"items","opt":"any","y":{"type":"array","x":"tags","opt":"any","y":{"type":"string","x":"$","opt":"eq","y":"sale"}}}`, true},
		{"none scalar", `{"type":"array","x":"scores","opt":"none","y":{"type":"number","x":"$","opt":"lt","y":60}}`, true},
		{"len", `{"type":"number","x":{"type":"len","x":"items"},"opt":"eq","y":2}`, true},
		{"arith", `{"type":"number","x":{"type":"arith","x":{"type":"arith","x":"age","opt":"mul","y":2},"opt":"div","y":4},"opt":"eq","y":15}`, true},
		{"arith on the right", `{"type":"number","x":"age","opt":"eq","y":{"type":"arith","x":"age","opt":"add","y":0}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := s.CompileJSON([]byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			mustMatch(t, c, alice, tc.want)
		})
	}
}

func TestCompileErrors(t *testing.T) {
	s := userSchema()
	cases := []struct {
		name, src string
		kind      error
		path      string
	}{
		// Structure: rejected by rule.Parse.
		{"unknown type", `{"type":"xor","items":[]}`, ErrUnknownOp, "$.type"},
		{"unknown opt", `{"type":"number","x":"age","opt":"ge","y":1}`, ErrUnknownOp, "$.opt"},
		{"opt not for type", `{"type":"boolean","x":"vip","opt":"gt","y":true}`, ErrUnknownOp, "$.opt"},
		{"string op on number", `{"type":"number","x":"age","opt":"contains","y":1}`, ErrUnknownOp, "$.opt"},
		{"quant opt", `{"type":"array","x":"items","opt":"eq","y":{"type":"and"}}`, ErrUnknownOp, "$.opt"},
		{"missing type", `{"x":"age"}`, ErrInvalidNode, "$"},
		{"missing key", `{"type":"number","x":"age","opt":"eq"}`, ErrInvalidNode, "$"},
		{"unknown key", `{"type":"and","items":[],"opt":"eq"}`, ErrInvalidNode, "$.opt"},
		{"misspelt key", `{"type":"number","x":"age","op":"eq","y":1}`, ErrInvalidNode, "$.op"},
		{"literal type", `{"type":"number","x":"age","opt":"gt","y":"18"}`, ErrTypeMismatch, "$.y"},
		{"null literal", `{"type":"number","x":"age","opt":"gt","y":null}`, ErrTypeMismatch, "$.y"},
		{"in wants a list", `{"type":"string","x":"city","opt":"in","y":"bj"}`, ErrTypeMismatch, "$.y"},
		{"in wrong element", `{"type":"string","x":"city","opt":"in","y":[1,2]}`, ErrTypeMismatch, "$.y"},
		{"mixed list", `{"type":"string","x":"city","opt":"in","y":["a",1]}`, ErrTypeMismatch, "$.y"},
		{"eq wants a scalar", `{"type":"string","x":"city","opt":"eq","y":["bj"]}`, ErrTypeMismatch, "$.y"},
		{"unknown expr type", `{"type":"string","x":"city","opt":"eq","y":{"a":1}}`, ErrInvalidNode, "$.y"},
		{"condition as value", `{"type":"boolean","x":"vip","opt":"eq","y":{"type":"and"}}`, ErrTypeMismatch, "$.y"},
		{"value as condition", `{"type":"and","items":[{"type":"field","name":"vip"}]}`, ErrTypeMismatch, "$.items[0]"},
		{"not an object", `{"type":"and","items":[true]}`, ErrInvalidNode, "$.items[0]"},
		{"empty field", `{"type":"number","x":"","opt":"eq","y":1}`, ErrInvalidNode, "$.x"},
		{"array x not a field", `{"type":"array","x":{"type":"len","x":"items"},"opt":"any","y":{"type":"and"}}`, ErrTypeMismatch, "$.x"},
		{"trailing", `{"type":"and"} {}`, ErrInvalidNode, "$"},
		{"bad json", `{"type":`, ErrInvalidNode, "$"},
		// Schema: rejected by Check.
		{"unknown field", `{"type":"and","items":[{"type":"boolean","x":"vip","opt":"eq","y":true},{"type":"boolean","x":"agee","opt":"eq","y":true}]}`, ErrUnknownField, "$.items[1].x"},
		{"field kind", `{"type":"number","x":"name","opt":"gt","y":1}`, ErrTypeMismatch, "$.x"},
		{"field kind y", `{"type":"string","x":"name","opt":"eq","y":{"type":"field","name":"age"}}`, ErrTypeMismatch, "$.y"},
		{"in field not a list", `{"type":"string","x":"name","opt":"in","y":{"type":"field","name":"city"}}`, ErrTypeMismatch, "$.y"},
		{"quant on scalar", `{"type":"array","x":"age","opt":"any","y":{"type":"and"}}`, ErrTypeMismatch, "$.x"},
		{"quant pred field from parent", `{"type":"array","x":"items","opt":"any","y":{"type":"number","x":"age","opt":"gt","y":1}}`, ErrUnknownField, "$.y.x"},
		{"injection", `{"type":"string","x":"$where","opt":"eq","y":"1"}`, ErrUnknownField, "$.x"},
		{"literal div zero", `{"type":"number","x":{"type":"arith","x":"age","opt":"div","y":0},"opt":"gt","y":1}`, ErrDivideByZero, "$.x.y"},
		{"len scalar", `{"type":"number","x":{"type":"len","x":"age"},"opt":"eq","y":0}`, ErrTypeMismatch, "$.x.x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CompileJSON([]byte(tc.src))
			var ce *CompileError
			if !errors.As(err, &ce) {
				t.Fatalf("want *CompileError, got %v", err)
			}
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
			if ce.Path != tc.path {
				t.Fatalf("path: want %s, got %s (%v)", tc.path, ce.Path, err)
			}
		})
	}
}

func vipRule() rule.Cond {
	return &rule.Compare{Type: rule.TypeBoolean, X: &rule.Field{Name: "vip"}, Op: rule.OpEq, Y: &rule.Bool{V: true}}
}

func TestCompileDepthLimit(t *testing.T) {
	var c rule.Cond = vipRule()
	for range MaxDepth + 1 {
		c = &rule.Not{X: c}
	}
	if _, err := userSchema().Compile(c); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("want ErrTooDeep, got %v", err)
	}
	data, _ := json.Marshal(c)
	if _, err := rule.Parse(data); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("Parse: want ErrTooDeep, got %v", err)
	}
}

// TestCheckGoBuiltRules covers trees built in Go, which bypass Parse and so
// can hold things the JSON decoder never produces.
func TestCheckGoBuiltRules(t *testing.T) {
	s := userSchema()
	shared := &rule.Field{Name: "tags"}
	cases := map[string]struct {
		c    rule.Cond
		kind error
		path string
	}{
		"nil root":  {nil, ErrInvalidNode, "$"},
		"nil item":  {&rule.And{Items: []rule.Cond{vipRule(), nil}}, ErrInvalidNode, "$.items[1]"},
		"typed nil": {&rule.Not{X: (*rule.Or)(nil)}, ErrInvalidNode, "$.x"},
		"opt for type": {&rule.Compare{Type: rule.TypeNumber, X: &rule.Field{Name: "age"}, Op: rule.OpContains, Y: &rule.Number{V: 1}},
			ErrUnknownOp, "$.opt"},
		"literal kind": {&rule.Compare{Type: rule.TypeNumber, X: &rule.Field{Name: "age"}, Op: rule.OpEq, Y: &rule.String{V: "1"}},
			ErrTypeMismatch, "$.y"},
		"bad arith op": {&rule.Compare{Type: rule.TypeNumber, X: &rule.Arith{X: &rule.Number{V: 1}, Op: rule.OpEq, Y: &rule.Number{V: 1}}, Op: rule.OpEq, Y: &rule.Number{V: 1}},
			ErrUnknownOp, "$.x.opt"},
		// The same *Field resolves to tags of user at the root and to tags of
		// item inside the quantifier: Checked.Field could not answer for both.
		"shared field node": {&rule.And{Items: []rule.Cond{
			&rule.Compare{Type: rule.TypeNumber, X: &rule.Len{X: shared}, Op: rule.OpEq, Y: &rule.Number{V: 0}},
			&rule.Quant{X: &rule.Field{Name: "items"}, Op: rule.OpAny, Y: &rule.Compare{Type: rule.TypeNumber, X: &rule.Len{X: shared}, Op: rule.OpEq, Y: &rule.Number{V: 0}}},
		}}, ErrInvalidNode, "$.items[1].y.x.x"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.Check(tc.c)
			var ce *CompileError
			if !errors.As(err, &ce) || !errors.Is(err, tc.kind) || ce.Path != tc.path {
				t.Fatalf("want %v at %s, got %v", tc.kind, tc.path, err)
			}
		})
	}
}

// TestRoundTrip: a Go-built tree marshals to the canonical JSON form, parses
// back to an equal tree and compiles to the same behaviour.
func TestRoundTrip(t *testing.T) {
	age, city := &rule.Field{Name: "age"}, &rule.Field{Name: "city"}
	c := &rule.And{Items: []rule.Cond{
		&rule.Compare{Type: rule.TypeNumber, X: age, Op: rule.OpGte, Y: &rule.Number{V: 18}},
		&rule.Compare{Type: rule.TypeString, X: city, Op: rule.OpIn, Y: &rule.Strings{V: []string{"bj", "sh"}}},
		&rule.Compare{Type: rule.TypeBoolean, X: &rule.Field{Name: "vip"}, Op: rule.OpEq, Y: &rule.Bool{V: false}},
		&rule.Compare{Type: rule.TypeNumber, X: &rule.Len{X: &rule.Field{Name: "tags"}}, Op: rule.OpEq, Y: &rule.Number{V: 0}},
		&rule.Compare{Type: rule.TypeString, X: &rule.String{V: "x"}, Op: rule.OpNin, Y: &rule.Field{Name: "tags"}},
		&rule.Compare{Type: rule.TypeNumber, X: &rule.Field{Name: "age"}, Op: rule.OpNin, Y: &rule.Numbers{}},
		&rule.Quant{X: &rule.Field{Name: "items"}, Op: rule.OpAll, Y: &rule.Compare{
			Type: rule.TypeNumber, X: &rule.Arith{X: &rule.Field{Name: "price"}, Op: rule.OpMul, Y: &rule.Number{V: 2}}, Op: rule.OpLt, Y: &rule.Number{V: 100}}},
	}}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"x":"age"`, `"y":18`, `"y":["bj","sh"]`, `"y":false`, `"y":[]`,
		`"x":{"type":"literal","value":"x"}`, `"y":{"type":"field","name":"tags"}`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %s in %s", want, data)
		}
	}
	parsed, err := rule.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(parsed)
	if string(again) != string(data) {
		t.Fatalf("not canonical:\n%s\n%s", data, again)
	}
	s := userSchema()
	fromGo, err := s.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := s.Compile(parsed)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []user{alice, {Age: 20, City: "sh"}, {Age: 20, City: "gz"}} {
		a, _ := fromGo.Match(u)
		b, _ := fromJSON.Match(u)
		if a != b {
			t.Fatalf("Go and JSON trees disagree on %+v", u)
		}
	}
}

func TestSchemaRegistration(t *testing.T) {
	u2b := func(u user) bool { return u.VIP }
	for name, f := range map[string]func(){
		"duplicate": func() { NewSchema[user]().Bool("x", u2b).Bool("x", u2b) },
		"empty":     func() { NewSchema[user]().Bool("", u2b) },
		"nil get":   func() { NewSchema[user]().Bool("x", nil) },
		"nil num":   func() { NewSchema[user]().Number("x", (func(user) int)(nil)) },
		"nil elem":  func() { NewSchema[user]().List("items", func(u user) []item { return nil }, (*Schema[item])(nil)) },
		"foreign checked": func() {
			chk, _ := userSchema().Check(vipRule())
			userSchema().Build(chk)
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

func TestConcurrentMatch(t *testing.T) {
	c, err := userSchema().CompileJSON([]byte(`{"type":"and","items":[
		{"type":"number","x":"age","opt":"gte","y":18},
		{"type":"string","x":"city","opt":"in","y":["a","b","c","d","e","f","g","h","i","bj"]},
		{"type":"array","x":"items","opt":"any","y":{"type":"number","x":"price","opt":"lt","y":10}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 1000 {
				if ok, err := c.Match(alice); !ok || err != nil {
					t.Error(ok, err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// FuzzCompileJSON checks that arbitrary input never panics, that a compiled
// condition never panics when matched, and that every accepted rule
// round-trips through its canonical JSON form.
func FuzzCompileJSON(f *testing.F) {
	for _, seed := range []string{
		`{"type":"and","items":[{"type":"boolean","x":"vip","opt":"eq","y":true}]}`,
		`{"type":"array","x":"items","opt":"any","y":{"type":"number","x":"price","opt":"lt","y":10}}`,
		`{"type":"number","x":{"type":"arith","x":"age","opt":"div","y":{"type":"field","name":"age"}},"opt":"eq","y":1}`,
		`{"type":"string","x":"city","opt":"in","y":["a"]}`,
		`{"type":"string","x":{"type":"literal","value":"a"},"opt":"nin","y":{"type":"field","name":"tags"}}`,
		`{"type":"number","x":{"type":"len","x":"items"},"opt":"gte","y":1}`,
	} {
		f.Add([]byte(seed))
	}
	s := userSchema()
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := rule.Parse(data)
		if err != nil {
			return
		}
		out, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r2, err := rule.Parse(out)
		if err != nil {
			t.Fatalf("canonical form rejected: %v\n%s", err, out)
		}
		if out2, _ := json.Marshal(r2); string(out2) != string(out) {
			t.Fatalf("not canonical:\n%s\n%s", out, out2)
		}
		c, err := s.Compile(r)
		if err != nil {
			return
		}
		_, _ = c.Match(alice)
		_, _ = c.Match(user{})
	})
}

func TestChecked(t *testing.T) {
	chk, err := userSchema().CheckJSON([]byte(`{"type":"array","x":"items","opt":"any","y":{"type":"number","x":"price","opt":"gt","y":10}}`))
	if err != nil {
		t.Fatal(err)
	}
	q := chk.Root().(*rule.Quant)
	list := chk.Field(q.X)
	if list.Kind != KindObjects || list.Elem["price"] == nil {
		t.Fatalf("unexpected list field: %+v", list)
	}
	// Predicate fields resolve against the element schema.
	pred := q.Y.(*rule.Compare)
	if chk.Field(pred.X.(*rule.Field)) != list.Elem["price"] || chk.Path(pred) != "$.y" || chk.Path(pred.Y) != "$.y.y" {
		t.Fatalf("unexpected predicate resolution")
	}
	if f, ok := userSchema().Field("tags"); !ok || f.Kind != KindStrings || f.Elem[SelfField].Kind != KindString {
		t.Fatalf("scalar list element: %+v", f)
	}
}

func TestOps(t *testing.T) {
	for _, op := range rule.Ops() {
		if got, ok := rule.ParseOp(op.String()); !ok || got != op {
			t.Fatalf("round trip %s: got %v %v", op, got, ok)
		}
	}
	if _, ok := rule.ParseOp("ge"); ok {
		t.Fatal("unknown name accepted")
	}
	if rule.Op(0).String() != "invalid" {
		t.Fatal("zero op must print invalid")
	}
}
