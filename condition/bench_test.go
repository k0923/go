package condition

import (
	"strings"
	"testing"
)

type rec struct {
	Age      float64
	Name     string
	City     string
	Children []rec
}

var (
	cities   = strings.Fields("a b c d e f g h i j k l m n o p q r s bj")
	benchRec = rec{Age: 30, Name: "alice", City: "bj", Children: []rec{{Age: 9}, {Age: 7}, {Age: 5}, {Age: 4}, {Age: 2}}}
)

// benchCond mirrors the rule used to benchmark the previous implementation:
// age >= 18 && name starts with "al" && city in 20 cities && any child age < 3.
func benchCond() Condition[rec] {
	recAge := Field(func(r rec) float64 { return r.Age })
	return And(
		Ge(recAge, Const[rec](18.0)),
		HasPrefix(Field(func(r rec) string { return r.Name }), Const[rec]("al")),
		In(Field(func(r rec) string { return r.City }), cities...),
		Any(Field(func(r rec) []rec { return r.Children }), Lt(recAge, Const[rec](3.0))),
	)
}

func BenchmarkMatch(b *testing.B) {
	c := benchCond()
	b.ReportAllocs()
	for b.Loop() {
		if ok, err := c.Match(benchRec); !ok || err != nil {
			b.Fatal(ok, err)
		}
	}
}

func BenchmarkMatchCompiled(b *testing.B) {
	var s *Schema[rec]
	s = NewSchema[rec]().
		Number("age", func(r rec) float64 { return r.Age }).
		String("name", func(r rec) string { return r.Name }).
		String("city", func(r rec) string { return r.City })
	s.List("children", func(r rec) []rec { return r.Children }, s)
	c, err := s.CompileJSON([]byte(`{"op":"and","args":[
		{"op":"ge","args":[{"field":"age"},{"value":18}]},
		{"op":"starts_with","args":[{"field":"name"},{"value":"al"}]},
		{"op":"in","args":[{"field":"city"},{"value":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q","r","s","bj"]}]},
		{"op":"any","args":[{"field":"children"},{"op":"lt","args":[{"field":"age"},{"value":3}]}]}]}`))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if ok, err := c.Match(benchRec); !ok || err != nil {
			b.Fatal(ok, err)
		}
	}
}

func BenchmarkInLarge(b *testing.B) {
	set := make([]string, 200)
	for i := range set {
		set[i] = "city-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('A'+i/26))
	}
	c := In(Field(func(r rec) string { return r.City }), set...)
	b.ReportAllocs()
	for b.Loop() {
		if ok, _ := c.Match(benchRec); ok {
			b.Fatal("unexpected")
		}
	}
}
