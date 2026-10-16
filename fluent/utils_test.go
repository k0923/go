package fluent

import (
	"testing"
)

type tagged struct {
	Name     string   `form:"name,omitempty"`
	Age      int      `form:"age"`
	Tags     []string `form:"tags"`
	Nick     string   `form:"nick,omitempty"`
	internal string
	Inner    struct {
		City string `form:"city"`
	}
	Ptr *struct {
		Zip string `form:"zip"`
	}
}

func TestSetMapStruct(t *testing.T) {
	v := tagged{Name: "bob", Age: 3, Tags: []string{"x", "y"}, internal: "secret"}
	v.Inner.City = "sh"
	got := map[string]string{}
	SetMap(&v, got, "form")
	want := map[string]string{"name": "bob", "age": "3", "tags": "x,y", "city": "sh"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("got[%q] = %q, want %q (all: %v)", k, got[k], w, got)
		}
	}
}

func TestSetMapNilPointer(t *testing.T) {
	var p *tagged
	SetMap(p, map[string]string{}, "form")
}

func TestSetMapScalarsAndMap(t *testing.T) {
	got := map[string]string{}
	SetMap("flag", got, "form")
	SetMap(42, got, "form")
	SetMap(map[int][]int{1: {2, 3}}, got, "form")
	if _, ok := got["flag"]; !ok {
		t.Fatal("string key missing")
	}
	if _, ok := got["42"]; !ok {
		t.Fatal("int key missing")
	}
	if got["1"] != "2,3" {
		t.Fatalf("map value = %q", got["1"])
	}
}

func TestGetValue(t *testing.T) {
	type named int
	cases := []struct {
		in   any
		want string
	}{
		{"s", "s"},
		{12, "12"},
		{int64(-3), "-3"},
		{uint32(7), "7"},
		{true, "true"},
		{1.5, "1.5"},
		{float32(0.1), "0.1"},
		{1e21, "1e+21"},
		{named(5), "5"},
		{[]int{1, 2, 3}, "1,2,3"},
		{[2]string{"a", "b"}, "a,b"},
		{[]any{1, "x", 2.5}, "1,x,2.5"},
		{[]int{}, ""},
		{nil, "<nil>"},
	}
	for _, c := range cases {
		if got := GetValue(c.in); got != c.want {
			t.Errorf("GetValue(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

type benchQ struct {
	Name  string   `form:"name"`
	Age   int      `form:"age"`
	Tags  []string `form:"tags"`
	Email string   `form:"email"`
	Inner struct {
		City string `form:"city"`
	}
}

func BenchmarkSetMapStruct(b *testing.B) {
	q := benchQ{Name: "a", Age: 3, Tags: []string{"x", "y", "z"}, Email: "e"}
	q.Inner.City = "c"
	for b.Loop() {
		SetMap(q, map[string]string{}, "form")
	}
}

func BenchmarkGetValueSlice(b *testing.B) {
	s := make([]int, 64)
	for b.Loop() {
		GetValue(s)
	}
}
