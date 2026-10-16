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

const promotionRule = `{
  "op": "and",
  "args": [
    {"op": "ge", "args": [{"field": "amount"}, {"value": 100}]},
    {"op": "in", "args": [{"field": "country"}, {"value": ["CN", "SG", "JP"]}]},
    {"op": "or", "args": [
      {"field": "vip"},
      {"op": "in", "args": [{"value": "WELCOME"}, {"field": "coupons"}]}
    ]},
    {"op": "any", "args": [
      {"field": "items"},
      {"op": "and", "args": [
        {"op": "eq", "args": [{"field": "category"}, {"value": "book"}]},
        {"op": "gt", "args": [{"field": "price"}, {"value": 20}]}
      ]}
    ]},
    {"op": "none", "args": [
      {"field": "scores"},
      {"op": "lt", "args": [{"field": "$"}, {"value": 60}]}
    ]}
  ]
}`

func main() {
	schema := newOrderSchema()

	// 1. Compile once, match many times. The compiled rule is immutable and
	//    safe to share across goroutines.
	// 1. 编译一次，多次匹配。编译后的规则不可变，可在多个 goroutine 间共享。
	rule, err := schema.CompileJSON([]byte(promotionRule))
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
		ok, matchErr := rule.Match(o)
		fmt.Printf("promotion(%s) = %v %v\n", o.ID, ok, errOrEmpty(matchErr))
	}

	// 2. Build the same tree in Go with Op/Ref/LitXxx and serialise it, e.g. for
	//    an admin UI that saves rules. Zero literals (false, 0) are preserved.
	// 2. 使用 Op/Ref/LitXxx 在 Go 中构建同样的树并序列化，
	//    例如用于保存规则的管理后台。零值字面量（false、0）会被保留。
	node := c.Op(c.OpAnd,
		c.Op(c.OpEq, c.Ref("vip"), c.LitBool(false)),
		c.Op(c.OpEq, c.Op(c.OpLen, c.Ref("coupons")), c.LitNum(0)),
		c.Op(c.OpGt, c.Op(c.OpMul, c.Ref("amount"), c.LitNum(0.9)), c.LitNum(100)),
	)
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
		{"unknown field", `{"op":"and","args":[{"field":"vip"},{"field":"amout"}]}`},
		{"type mismatch", `{"op":"gt","args":[{"field":"country"},{"value":1}]}`},
		{"unknown op", `{"op":"xor","args":[]}`},
		{"wrong arity", `{"op":"not","args":[]}`},
		{"div by zero", `{"op":"gt","args":[{"op":"div","args":[{"field":"amount"},{"value":0}]},{"value":1}]}`},
		{"parent field", `{"op":"any","args":[{"field":"items"},{"field":"vip"}]}`}, // predicate sees Item fields only
		{"misspelt key", `{"opt":"and"}`},                                           // unknown JSON keys are rejected
		{"not a bool", `{"field":"amount"}`},
		{"mixed list", `{"op":"in","args":[{"field":"country"},{"value":["CN",1]}]}`},
		{"trailing input", `{"op":"and"} {"op":"or"}`},
	}
	for _, b := range bad {
		_, err := schema.CompileJSON([]byte(b.src))
		if ce, ok := errors.AsType[*c.CompileError](err); ok {
			fmt.Printf("%-15s path=%-18s %v\n", b.name, ce.Path, ce.Err)
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
