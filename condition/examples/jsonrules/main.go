// Run with: go run ./condition/examples/jsonrules
// 运行方式：go run ./condition/examples/jsonrules
//
// Rules stored as JSON (database, config centre, admin UI) are compiled once
// against a Schema. All configuration errors surface at compile time with the
// JSON path of the offending node; Match never re-validates.
// 以 JSON 形式存储的规则（数据库、配置中心、管理后台）基于 Schema 编译一次。
// 所有配置错误都在编译期暴露，并附带出错节点的 JSON 路径；Match 不会重复校验。
package main

import (
	"encoding/json"
	"errors"
	"fmt"

	c "github.com/k0923/go/condition"
	"github.com/k0923/go/condition/rule"
)

type Item struct {
	Category string
	Price    float64
}

type Order struct {
	ID      string
	Amount  int64
	Country string
	VIP     bool
	Coupons []string
	Scores  []float64
	Items   []Item
}

// newOrderSchema whitelists the fields a rule may reference. Only registered
// fields are reachable, so untrusted rules cannot read anything else.
// newOrderSchema 定义规则可以引用的字段白名单。只有注册过的字段可被访问，
// 因此不可信的规则无法读取其他数据。
func newOrderSchema() *c.Schema[Order] {
	items := c.NewSchema[Item]().
		String("category", func(i Item) string { return i.Category }).
		Number("price", func(i Item) float64 { return i.Price })

	return c.NewSchema[Order]().
		Number("amount", func(o Order) int64 { return o.Amount }). // any numeric type, compared as float64
		String("country", func(o Order) string { return o.Country }).
		Bool("vip", func(o Order) bool { return o.VIP }).
		Strings("coupons", func(o Order) []string { return o.Coupons }).
		Numbers("scores", func(o Order) []float64 { return o.Scores }).
		List("items", func(o Order) []Item { return o.Items }, items) // nested object list
}

// Flat JSON: every node is one object discriminated by "type". In an x
// position a string is a field; in a y position a scalar or array is a
// literal whose type comes from the comparison type and opt.
// 扁平 JSON：每个节点是一个对象，用 "type" 区分。x 位置的字符串是字段；
// y 位置的标量或数组是字面量，其类型由比较的 type 与 opt 决定。
const promotionRule = `{
  "type": "and",
  "items": [
    {"type": "number", "x": "amount", "opt": "gte", "y": 100},
    {"type": "string", "x": "country", "opt": "in", "y": ["CN", "SG", "JP"]},
    {"type": "or", "items": [
      {"type": "boolean", "x": "vip", "opt": "eq", "y": true},
      {"type": "string", "x": {"type": "literal", "value": "WELCOME"}, "opt": "in", "y": {"type": "field", "name": "coupons"}}
    ]},
    {"type": "array", "x": "items", "opt": "any", "y": {"type": "and", "items": [
      {"type": "string", "x": "category", "opt": "eq", "y": "book"},
      {"type": "number", "x": "price", "opt": "gt", "y": 20}
    ]}},
    {"type": "array", "x": "scores", "opt": "none", "y": {"type": "number", "x": "$", "opt": "lt", "y": 60}},
    {"type": "number", "x": {"type": "arith", "x": "amount", "opt": "sub", "y": {"type": "len", "x": "items"}}, "opt": "gt", "y": 0}
  ]
}`

func main() {
	schema := newOrderSchema()

	// 1. Compile once, match many times. The compiled rule is immutable and
	//    safe to share across goroutines.
	// 1. 编译一次，多次匹配。编译后的规则不可变，可在多个 goroutine 间共享。
	promo, err := schema.CompileJSON([]byte(promotionRule))
	if err != nil {
		panic(err)
	}
	orders := []Order{
		{ID: "o1", Amount: 150, Country: "CN", VIP: true, Scores: []float64{80, 90}, Items: []Item{{"book", 35}}},
		{ID: "o2", Amount: 150, Country: "CN", Coupons: []string{"WELCOME"}, Items: []Item{{"book", 35}}},
		{ID: "o3", Amount: 150, Country: "US", VIP: true, Items: []Item{{"book", 35}}},
		{ID: "o4", Amount: 150, Country: "CN", VIP: true, Scores: []float64{50}, Items: []Item{{"book", 35}}},
	}
	for _, o := range orders {
		ok, matchErr := promo.Match(o)
		fmt.Printf("promotion(%s) = %v %v\n", o.ID, ok, errOrEmpty(matchErr))
	}

	// 2. Build a rule in Go with the rule package and serialise it, e.g. for
	//    an admin UI that saves rules. Each node shape is its own type, so a
	//    comparison cannot be missing its y and And cannot have an x.
	// 2. 用 rule 包在 Go 中构建规则并序列化，例如用于保存规则的管理后台。
	//    每种节点形态各是一个类型：比较节点不会缺 y，And 也不会有 x。
	node := &rule.And{Items: []rule.Cond{
		&rule.Compare{Type: rule.TypeBoolean, X: &rule.Field{Name: "vip"}, Op: rule.OpEq, Y: &rule.Bool{V: false}},
		&rule.Compare{Type: rule.TypeNumber, X: &rule.Len{X: &rule.Field{Name: "coupons"}}, Op: rule.OpEq, Y: &rule.Number{V: 0}},
		&rule.Compare{Type: rule.TypeNumber,
			X:  &rule.Arith{X: &rule.Field{Name: "amount"}, Op: rule.OpMul, Y: &rule.Number{V: 0.9}},
			Op: rule.OpGt, Y: &rule.Number{V: 100}},
	}}
	data, _ := json.Marshal(node)
	fmt.Println("serialised:", string(data))
	built, err := schema.Compile(node)
	if err != nil {
		panic(err)
	}
	ok, _ := built.Match(Order{Amount: 120})
	fmt.Println("built rule(amount=120) =", ok)

	// 3. Compile errors carry a sentinel (errors.Is) and a JSON path.
	// 3. 编译错误携带哨兵错误（可用 errors.Is 判断）和 JSON 路径。
	bad := []struct{ name, src string }{
		{"unknown field", `{"type":"and","items":[{"type":"boolean","x":"vip","opt":"eq","y":true},{"type":"number","x":"amout","opt":"gt","y":1}]}`},
		{"field type", `{"type":"number","x":"country","opt":"gt","y":1}`},
		{"literal type", `{"type":"string","x":"country","opt":"eq","y":1}`},
		{"unknown type", `{"type":"xor","items":[]}`},
		{"opt for type", `{"type":"boolean","x":"vip","opt":"gt","y":true}`},
		{"missing key", `{"type":"not"}`},
		{"div by zero", `{"type":"number","x":{"type":"arith","x":"amount","opt":"div","y":0},"opt":"gt","y":1}`},
		{"parent field", `{"type":"array","x":"items","opt":"any","y":{"type":"boolean","x":"vip","opt":"eq","y":true}}`}, // predicate sees Item fields only
		{"misspelt key", `{"type":"number","x":"amount","op":"gt","y":1}`},                                                // unknown JSON keys are rejected
		{"not a cond", `{"type":"len","x":"items"}`},
		{"mixed list", `{"type":"string","x":"country","opt":"in","y":["CN",1]}`},
		{"trailing input", `{"type":"and"} {"type":"or"}`},
	}
	for _, b := range bad {
		_, err := schema.CompileJSON([]byte(b.src))
		if ce, ok := errors.AsType[*c.CompileError](err); ok {
			fmt.Printf("%-15s path=%-12s %v\n", b.name, ce.Path, ce.Err)
		} else {
			fmt.Printf("%-15s decode error: %v\n", b.name, err)
		}
	}
	_, err = schema.CompileJSON([]byte(bad[0].src))
	fmt.Println("errors.Is(err, ErrUnknownField) =", errors.Is(err, c.ErrUnknownField))
}

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return "error: " + err.Error()
}
