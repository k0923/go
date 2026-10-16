// Run with: go run ./condition/examples/collections
// 运行方式：go run ./condition/examples/collections
//
// Quantifiers (Any/All/None), Len, arithmetic expressions and error handling.
// 量词（Any/All/None）、Len、算术表达式与错误处理。
package main

import (
	"errors"
	"fmt"

	c "github.com/k0923/go/condition"
)

type Line struct {
	SKU   string
	Price float64
	Qty   int
}

type Order struct {
	ID       string
	Discount float64
	Lines    []Line
}

var (
	lines    = c.Field(func(o Order) []Line { return o.Lines })
	discount = c.Field(func(o Order) float64 { return o.Discount })
	price    = c.Field(func(l Line) float64 { return l.Price })
	qty      = c.Field(func(l Line) int { return l.Qty })
	sku      = c.Field(func(l Line) string { return l.SKU })
)

func main() {
	big := Order{ID: "big", Discount: 10, Lines: []Line{{"A-1", 30, 2}, {"A-2", 50, 1}, {"B-9", 5, 20}}}
	empty := Order{ID: "empty"}

	// 1. Quantifiers: the element type (Line) differs from the input type
	//    (Order); the predicate is an ordinary Condition[Line].
	// 1. 量词：元素类型（Line）与输入类型（Order）不同，
	//    谓词就是普通的 Condition[Line]。
	cheap := c.Lt(price, c.Const[Line](10.0))
	show("any line cheap", c.Any(lines, cheap), big, empty)
	show("all lines A-*", c.All(lines, c.HasPrefix(sku, c.Const[Line]("A-"))), big, empty)
	show("no line qty > 50", c.None(lines, c.Gt(qty, c.Const[Line](50))), big, empty)
	// Empty lists: Any is false, All and None are vacuously true.
	// 空列表：Any 为 false，All 和 None 为 true（空真）。

	// 2. Len turns a list into a number that can be compared.
	// 2. Len 把列表转换为可比较的数值。
	show("has >= 3 lines", c.Ge(c.Len(lines), c.Const[Order](3)), big, empty)

	// 3. Arithmetic: Add/Sub/Mul/Div compose expressions; each line's total
	//    is price * qty, and the order total is computed with ExprFunc.
	// 3. 算术：Add/Sub/Mul/Div 组合表达式；每行金额为 price * qty，
	//    订单总额通过 ExprFunc 计算。
	lineTotal := c.Mul(price, c.Field(func(l Line) float64 { return float64(l.Qty) }))
	orderTotal := c.ExprFunc[Order, float64](func(o Order) (float64, error) {
		var sum float64
		for _, l := range o.Lines {
			v, err := lineTotal.Eval(l)
			if err != nil {
				return 0, err
			}
			sum += v
		}
		return sum, nil
	})
	payable := c.Sub(orderTotal, discount)
	total, _ := payable.Eval(big)
	fmt.Printf("%-22s %v\n", "payable(big):", total)
	show("payable >= 200", c.Ge(payable, c.Const[Order](200.0)), big, empty)

	// 4. Errors: Div reports ErrDivideByZero; And/Or propagate the first error
	//    together with a false result.
	// 4. 错误：Div 返回 ErrDivideByZero；And/Or 会携带 false 结果透传第一个错误。
	avgPrice := c.Div(orderTotal, c.Field(func(o Order) float64 { return float64(len(o.Lines)) }))
	_, err := c.Gt(avgPrice, c.Const[Order](0.0)).Match(empty)
	fmt.Printf("%-22s is ErrDivideByZero=%v\n", "avg price(empty):", errors.Is(err, c.ErrDivideByZero))

	// Guard with And: the left side short-circuits before the division runs.
	// 使用 And 做保护：左侧短路后不会执行除法。
	safe := c.And(c.Gt(c.Len(lines), c.Const[Order](0)), c.Gt(avgPrice, c.Const[Order](20.0)))
	show("avg price > 20", safe, big, empty)
}

func show(label string, cond c.Condition[Order], orders ...Order) {
	fmt.Printf("%-22s", label+":")
	for _, o := range orders {
		ok, err := cond.Match(o)
		if err != nil {
			fmt.Printf(" %s=error(%v)", o.ID, err)
			continue
		}
		fmt.Printf(" %s=%v", o.ID, ok)
	}
	fmt.Println()
}
