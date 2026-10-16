package condition

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
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
		{"ge", `{"op":"ge","args":[{"field":"age"},{"value":18}]}`, true},
		{"bool field", `{"field":"vip"}`, true},
		{"bool eq", `{"op":"eq","args":[{"field":"vip"},{"value":false}]}`, false},
		{"not", `{"op":"not","args":[{"field":"vip"}]}`, false},
		{"string order", `{"op":"lt","args":[{"field":"name"},{"value":"bob"}]}`, true},
		{"starts_with", `{"op":"starts_with","args":[{"field":"name"},{"value":"al"}]}`, true},
		{"in literal", `{"op":"in","args":[{"field":"city"},{"value":["sh","bj"]}]}`, true},
		{"in empty", `{"op":"in","args":[{"field":"city"},{"value":[]}]}`, false},
		{"not_in empty", `{"op":"not_in","args":[{"field":"age"},{"value":[]}]}`, true},
		{"in number list", `{"op":"in","args":[{"field":"age"},{"value":[1,30]}]}`, true},
		{"in field list", `{"op":"in","args":[{"value":"beta"},{"field":"tags"}]}`, true},
		{"or false", `{"op":"or","args":[{"value":false},{"op":"eq","args":[{"field":"city"},{"value":"sh"}]}]}`, false},
		{"and empty", `{"op":"and"}`, true},
		{"any object", `{"op":"any","args":[{"field":"items"},{"op":"lt","args":[{"field":"price"},{"value":10}]}]}`, true},
		{"all object", `{"op":"all","args":[{"field":"items"},{"op":"lt","args":[{"field":"price"},{"value":10}]}]}`, false},
		{"nested quant", `{"op":"any","args":[{"field":"items"},{"op":"in","args":[{"value":"sale"},{"field":"tags"}]}]}`, true},
		{"none scalar", `{"op":"none","args":[{"field":"scores"},{"op":"lt","args":[{"field":"$"},{"value":60}]}]}`, true},
		{"len", `{"op":"eq","args":[{"op":"len","args":[{"field":"items"}]},{"value":2}]}`, true},
		{"len empty literal", `{"op":"eq","args":[{"op":"len","args":[{"value":[]}]},{"value":0}]}`, true},
		{"arith", `{"op":"eq","args":[{"op":"div","args":[{"op":"mul","args":[{"field":"age"},{"value":2}]},{"value":4}]},{"value":15}]}`, true},
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
		{"unknown op", `{"op":"xor","args":[]}`, ErrUnknownOp, "$"},
		{"unknown field", `{"op":"and","args":[{"field":"vip"},{"field":"agee"}]}`, ErrUnknownField, "$.args[1]"},
		{"kind mismatch", `{"op":"gt","args":[{"field":"age"},{"value":"18"}]}`, ErrTypeMismatch, "$"},
		{"ordered bool", `{"op":"gt","args":[{"field":"vip"},{"value":true}]}`, ErrTypeMismatch, "$"},
		{"non bool root", `{"field":"age"}`, ErrTypeMismatch, "$"},
		{"non bool in and", `{"op":"and","args":[{"field":"vip"},{"field":"name"}]}`, ErrTypeMismatch, "$.args[1]"},
		{"arity", `{"op":"eq","args":[{"field":"age"}]}`, ErrArity, "$"},
		{"quant arity", `{"op":"any","args":[{"field":"items"}]}`, ErrArity, "$"},
		{"quant on scalar", `{"op":"any","args":[{"field":"age"},{"value":true}]}`, ErrTypeMismatch, "$.args[0]"},
		{"quant pred field from parent", `{"op":"any","args":[{"field":"items"},{"op":"gt","args":[{"field":"age"},{"value":1}]}]}`, ErrUnknownField, "$.args[1].args[0]"},
		{"in wrong element", `{"op":"in","args":[{"field":"city"},{"value":[1,2]}]}`, ErrTypeMismatch, "$"},
		{"mixed list", `{"op":"in","args":[{"field":"city"},{"value":["a",1]}]}`, ErrTypeMismatch, "$.args[1]"},
		{"object literal", `{"op":"eq","args":[{"field":"city"},{"value":{"a":1}}]}`, ErrInvalidNode, "$.args[1]"},
		{"empty node", `{}`, ErrInvalidNode, "$"},
		{"two kinds", `{"op":"and","field":"vip"}`, ErrInvalidNode, "$"},
		{"args without op", `{"field":"vip","args":[{"value":true}]}`, ErrInvalidNode, "$"},
		{"literal div zero", `{"op":"gt","args":[{"op":"div","args":[{"field":"age"},{"value":0}]},{"value":1}]}`, ErrDivideByZero, "$.args[0].args[1]"},
		{"len scalar", `{"op":"eq","args":[{"op":"len","args":[{"field":"age"}]},{"value":0}]}`, ErrTypeMismatch, "$.args[0].args[0]"},
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

func TestCompileDepthLimit(t *testing.T) {
	n := Ref("vip")
	for range MaxDepth + 1 {
		n = Op(OpNot, n)
	}
	if _, err := userSchema().Compile(n); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("want ErrTooDeep, got %v", err)
	}
}

func TestParseNodeStrict(t *testing.T) {
	for name, src := range map[string]string{
		"misspelt key": `{"opt":"and"}`, // the old API's field name must not be silently ignored
		"trailing":     `{"op":"and"} {}`,
		"bad json":     `{"op":`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseNode([]byte(src)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestNodeRoundTrip(t *testing.T) {
	n := Op(OpAnd,
		Op(OpGe, Ref("age"), LitNum(18)),
		Op(OpIn, Ref("city"), LitStrs("bj", "sh")),
		Op(OpEq, Ref("vip"), LitBool(false)),
		Op(OpEq, Op(OpLen, Ref("tags")), LitNum(0)),
	)
	data, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"value":null`) || strings.Contains(string(data), `"field":""`) {
		t.Fatalf("empty members should be omitted: %s", data)
	}
	// false and 0 must survive omitzero: they are meaningful literals.
	if !strings.Contains(string(data), `"value":false`) || !strings.Contains(string(data), `"value":0`) {
		t.Fatalf("zero literals dropped: %s", data)
	}
	s := userSchema()
	fromGo, err := s.Compile(n)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := s.CompileJSON(data)
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
	c, err := userSchema().CompileJSON([]byte(`{"op":"and","args":[
		{"op":"ge","args":[{"field":"age"},{"value":18}]},
		{"op":"in","args":[{"field":"city"},{"value":["a","b","c","d","e","f","g","h","i","bj"]}]},
		{"op":"any","args":[{"field":"items"},{"op":"lt","args":[{"field":"price"},{"value":10}]}]}]}`))
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

// FuzzCompileJSON checks that arbitrary input never panics and that a
// successfully compiled condition never panics when matched.
func FuzzCompileJSON(f *testing.F) {
	for _, seed := range []string{
		`{"op":"and","args":[{"field":"vip"}]}`,
		`{"op":"any","args":[{"field":"items"},{"op":"lt","args":[{"field":"price"},{"value":10}]}]}`,
		`{"op":"eq","args":[{"op":"div","args":[{"field":"age"},{"field":"age"}]},{"value":1}]}`,
		`{"op":"in","args":[{"field":"city"},{"value":["a"]}]}`,
	} {
		f.Add([]byte(seed))
	}
	s := userSchema()
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := s.CompileJSON(data)
		if err != nil {
			return
		}
		_, _ = c.Match(alice)
		_, _ = c.Match(user{})
		// Translators must accept every checked tree without panicking;
		// that is covered by their own packages, here we pin Check == Compile.
		if _, err := s.CheckJSON(data); err != nil {
			t.Fatalf("Compile accepted but Check rejected: %v", err)
		}
	})
}

func TestCheckTypedNode(t *testing.T) {
	typed, err := userSchema().CheckJSON([]byte(`{"op":"any","args":[{"field":"items"},{"op":"gt","args":[{"field":"price"},{"value":10}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	list, pred := typed.Args[0], typed.Args[1]
	if typed.Kind != KindBool || list.Field.Kind != KindObjects || list.Field.Elem["price"] == nil {
		t.Fatalf("unexpected typed tree: %+v", typed)
	}
	// Predicate fields resolve against the element schema.
	if f := pred.Args[0].Field; f != list.Field.Elem["price"] || pred.Args[1].Lit.Number() != 10 || pred.Path != "$.args[1]" {
		t.Fatalf("unexpected predicate: %+v", pred)
	}
	if f, ok := userSchema().Field("tags"); !ok || f.Kind != KindStrings || f.Elem[SelfField].Kind != KindString {
		t.Fatalf("scalar list element: %+v", f)
	}
}
