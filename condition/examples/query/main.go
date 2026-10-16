// Run with: go run ./condition/examples/query
// 运行方式：go run ./condition/examples/query
//
// One JSON rule (rule.json), three consumers:
//
//	                       ┌─> Compile  -> Condition[Order]  (in-memory Match)
//	rule.json -> Check ────┼─> sqlquery.Where   -> WHERE ... + args
//	         (TypedNode)   └─> mongoquery.Filter -> {"$and": [...]}
//
// Check validates once against the Schema and produces a TypedNode; every
// backend consumes that same TypedNode, so they cannot disagree about what a
// rule means. The SQL is executed against an in-memory SQLite database and
// compared with in-memory matching to show the results are identical.
//
// 同一份 JSON 规则（rule.json）供三方使用：
// Check 基于 Schema 校验一次并生成 TypedNode；所有后端都消费同一个 TypedNode，
// 因此不会对规则含义产生分歧。示例会在内存 SQLite 中执行生成的 SQL，
// 并与内存匹配结果比对，证明两者一致。
package main

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"

	_ "modernc.org/sqlite"

	c "github.com/k0923/go/condition"
	"github.com/k0923/go/condition/mongoquery"
	"github.com/k0923/go/condition/sqlquery"
)

//go:embed rule.json
var ruleJSON []byte

type Item struct {
	SKU      string
	Category string
	Price    float64
	Qty      int
}

type Order struct {
	ID       int
	Status   string
	Amount   float64
	Discount float64
	Country  string
	Email    string
	VIP      bool
	Coupons  []string
	Tags     []string
	Items    []Item
}

// orderSchema is the single source of truth for field names and types.
// It is shared by in-memory evaluation and every query backend.
// orderSchema 是字段名与类型的唯一来源，内存求值与所有查询后端共用。
func orderSchema() *c.Schema[Order] {
	items := c.NewSchema[Item]().
		String("sku", func(i Item) string { return i.SKU }).
		String("category", func(i Item) string { return i.Category }).
		Number("price", func(i Item) float64 { return i.Price }).
		Number("qty", func(i Item) int { return i.Qty })
	return c.NewSchema[Order]().
		Number("id", func(o Order) int { return o.ID }).
		String("status", func(o Order) string { return o.Status }).
		Number("amount", func(o Order) float64 { return o.Amount }).
		Number("discount", func(o Order) float64 { return o.Discount }).
		String("country", func(o Order) string { return o.Country }).
		String("email", func(o Order) string { return o.Email }).
		Bool("vip", func(o Order) bool { return o.VIP }).
		Strings("coupons", func(o Order) []string { return o.Coupons }).
		Strings("tags", func(o Order) []string { return o.Tags }).
		List("items", func(o Order) []Item { return o.Items }, items)
}

var orders = []Order{
	{ID: 1, Status: "paid", Amount: 300, Country: "CN", Email: "a@corp.com", VIP: true,
		Items: []Item{{"BK-001", "book", 45, 1}, {"FD-9", "food", 5, 2}}},
	{ID: 2, Status: "paid", Amount: 300, Country: "CN", Email: "b@corp.com", Coupons: []string{"WELCOME10"},
		Items: []Item{{"BK-002", "book", 25, 1}}},
	{ID: 3, Status: "paid", Amount: 900, Discount: 100, Country: "SG", Email: "c@corp.com",
		Items: []Item{{"BK-003", "music", 30, 2}}},
	{ID: 4, Status: "paid", Amount: 300, Country: "CN", Email: "qa@test.local", VIP: true,
		Items: []Item{{"BK-004", "book", 45, 1}}}, // test account
	{ID: 5, Status: "created", Amount: 300, Country: "CN", Email: "d@corp.com", VIP: true,
		Items: []Item{{"BK-005", "book", 45, 1}}}, // not paid
	{ID: 6, Status: "shipped", Amount: 300, Country: "CN", Email: "e@corp.com", VIP: true, Tags: []string{"fraud"},
		Items: []Item{{"BK-006", "book", 45, 1}}}, // flagged
	{ID: 7, Status: "paid", Amount: 300, Country: "CN", Email: "f@corp.com", VIP: true,
		Items: []Item{{"BK-007", "book", 45, 1}, {"BK-008", "book", 45, 0}}}, // a line with qty 0
	{ID: 8, Status: "shipped", Amount: 120, Country: "JP", Email: "g@corp.com", VIP: true,
		Items: []Item{{"MU-1", "music", 30, 1}}}, // SKU prefix does not match
}

func main() {
	schema := orderSchema()

	// 1. Check once. All configuration errors would surface here.
	// 1. 只校验一次，所有配置错误都会在这里暴露。
	typed, err := schema.CheckJSON(ruleJSON)
	if err != nil {
		panic(err)
	}

	// 2. In memory: e.g. validating an order in a request handler.
	// 2. 内存求值：例如在请求处理中校验一笔订单。
	match, err := schema.CompileJSON(ruleJSON)
	if err != nil {
		panic(err)
	}
	var inMemory []int
	for _, o := range orders {
		if ok, _ := match.Match(o); ok {
			inMemory = append(inMemory, o.ID)
		}
	}
	fmt.Println("== in-memory matches:", inMemory)

	// 3. SQL: list fields are backed by child tables via Relations.
	// 3. SQL：列表字段通过 Relations 映射到子表。
	where, args, err := sqlquery.Where(typed, sqlquery.Options{
		Dialect: sqlquery.SQLite,
		Table:   "o",
		Columns: map[string]string{"amount": "total_amount"},
		Relations: map[string]sqlquery.Relation{
			"coupons": {Table: "order_coupons", On: "{child}.order_id = {parent}.id", Columns: map[string]string{c.SelfField: "code"}},
			"tags":    {Table: "order_tags", On: "{child}.order_id = {parent}.id", Columns: map[string]string{c.SelfField: "tag"}},
			"items":   {Table: "order_items", On: "{child}.order_id = {parent}.id"},
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("\n== SQL (SQLite):")
	fmt.Println("SELECT id FROM orders AS o WHERE", where)
	fmt.Printf("args: %q\n", args)
	inSQL := querySQLite(where, args)
	fmt.Println("sqlite matches:", inSQL, "| identical to in-memory:", slices.Equal(inSQL, inMemory))

	// PostgreSQL can keep scalar lists as array columns: no Relation needed.
	// PostgreSQL 可以把标量列表存为数组列，无需配置 Relation。
	pgWhere, pgArgs, err := sqlquery.Where(typed, sqlquery.Options{
		Dialect: sqlquery.Postgres,
		Table:   "o",
		Relations: map[string]sqlquery.Relation{
			"items": {Table: "order_items", On: "{child}.order_id = {parent}.id"},
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("\n== SQL (PostgreSQL, tags/coupons as text[]):")
	fmt.Println(pgWhere)
	fmt.Printf("args: %q\n", pgArgs)

	// 4. MongoDB: the filter is a plain map, ready for collection.Find.
	//    Paths remap schema names to document paths.
	// 4. MongoDB：filter 是普通 map，可直接传给 collection.Find。
	//    Paths 用于把 Schema 字段名映射为文档路径。
	filter, err := mongoquery.Filter(typed, mongoquery.Options{Paths: map[string]string{
		"items": "lines", "items.price": "unit_price",
	}})
	if err != nil {
		panic(err)
	}
	pretty, _ := json.MarshalIndent(filter, "", "  ")
	fmt.Println("\n== MongoDB filter:")
	fmt.Println(string(pretty))
}

func querySQLite(where string, args []any) []int {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	exec := func(q string, a ...any) {
		if _, err := db.Exec(q, a...); err != nil {
			panic(fmt.Sprintf("%s: %v", q, err))
		}
	}
	exec(`CREATE TABLE orders (id INTEGER PRIMARY KEY, status TEXT NOT NULL, total_amount REAL NOT NULL,
		discount REAL NOT NULL, country TEXT NOT NULL, email TEXT NOT NULL, vip BOOLEAN NOT NULL)`)
	exec(`CREATE TABLE order_coupons (order_id INTEGER NOT NULL, code TEXT NOT NULL)`)
	exec(`CREATE TABLE order_tags (order_id INTEGER NOT NULL, tag TEXT NOT NULL)`)
	exec(`CREATE TABLE order_items (order_id INTEGER NOT NULL, sku TEXT NOT NULL, category TEXT NOT NULL,
		price REAL NOT NULL, qty INTEGER NOT NULL)`)
	for _, o := range orders {
		exec(`INSERT INTO orders VALUES (?, ?, ?, ?, ?, ?, ?)`, o.ID, o.Status, o.Amount, o.Discount, o.Country, o.Email, o.VIP)
		for _, v := range o.Coupons {
			exec(`INSERT INTO order_coupons VALUES (?, ?)`, o.ID, v)
		}
		for _, v := range o.Tags {
			exec(`INSERT INTO order_tags VALUES (?, ?)`, o.ID, v)
		}
		for _, it := range o.Items {
			exec(`INSERT INTO order_items VALUES (?, ?, ?, ?, ?)`, o.ID, it.SKU, it.Category, it.Price, it.Qty)
		}
	}
	rows, err := db.Query(`SELECT id FROM orders AS "o" WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if scanErr := rows.Scan(&id); scanErr != nil {
			panic(scanErr)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		panic(err)
	}
	return ids
}
