package condition_test

import (
	"errors"
	"fmt"

	"github.com/k0923/go/condition"
)

type Order struct {
	Amount  float64
	Country string
	Lines   []Line
}

type Line struct {
	SKU string
	Qty int
}

func Example() {
	amount := condition.Field(func(o Order) float64 { return o.Amount })
	country := condition.Field(func(o Order) string { return o.Country })
	lines := condition.Field(func(o Order) []Line { return o.Lines })
	qty := condition.Field(func(l Line) int { return l.Qty })

	bulk := condition.And(
		condition.Ge(amount, condition.Const[Order](100.0)),
		condition.In(country, "CN", "SG"),
		condition.Any(lines, condition.Gt(qty, condition.Const[Line](10))),
	)

	ok, err := bulk.Match(Order{Amount: 120, Country: "CN", Lines: []Line{{"a", 3}, {"b", 12}}})
	fmt.Println(ok, err)
	// Output: true <nil>
}

func ExampleSchema_CompileJSON() {
	lineSchema := condition.NewSchema[Line]().
		String("sku", func(l Line) string { return l.SKU }).
		Number("qty", func(l Line) int { return l.Qty })
	schema := condition.NewSchema[Order]().
		Number("amount", func(o Order) float64 { return o.Amount }).
		String("country", func(o Order) string { return o.Country }).
		List("lines", func(o Order) []Line { return o.Lines }, lineSchema)

	rule, err := schema.CompileJSON([]byte(`{
		"op": "and",
		"args": [
			{"op": "ge", "args": [{"field": "amount"}, {"value": 100}]},
			{"op": "all", "args": [{"field": "lines"}, {"op": "starts_with", "args": [{"field": "sku"}, {"value": "A-"}]}]}
		]
	}`))
	if err != nil {
		panic(err)
	}
	ok, _ := rule.Match(Order{Amount: 150, Lines: []Line{{"A-1", 1}, {"A-2", 2}}})
	fmt.Println(ok)

	// Configuration mistakes are caught once, at compile time, with a path.
	_, err = schema.CompileJSON([]byte(`{"op":"gt","args":[{"field":"country"},{"value":1}]}`))
	fmt.Println(errors.Is(err, condition.ErrTypeMismatch), err)
	// Output:
	// true
	// true condition: $: type mismatch: gt on string and number
}
