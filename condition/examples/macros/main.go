// Run with: go run ./condition/examples/macros
// 运行方式：go run ./condition/examples/macros
//
// Business concepts as macros, over schemaless records whose custom fields
// live under "data" in MongoDB.
//
//   - Schema.Macro names a condition ("high_risk", "active_since(date)").
//     Rules call it with {"type":"call","name":...,"args":[...]} instead of
//     repeating its definition; changing the macro changes every rule.
//   - Check expands calls into ordinary nodes, so memory, SQL and MongoDB
//     all support macros and cannot disagree about them.
//   - Object lifts the macros of a nested schema ("reviewer.senior"), and an
//     element schema's macros are available inside array rules over it.
//   - mongoquery.Options.Prefix moves every top-level path under "data".
//
// 用宏表达业务概念，数据是无固定结构的记录，自定义字段在 MongoDB 中位于 "data" 下：
//
//   - Schema.Macro 给条件命名（如 "high_risk"、"active_since(date)"）。规则通过
//     {"type":"call","name":...,"args":[...]} 调用，而不是重复其定义；修改宏即修改所有引用它的规则。
//   - Check 会把调用展开为普通节点，所以内存、SQL、MongoDB 都天然支持宏，且语义一致。
//   - Object 会提升嵌套 Schema 的宏（"reviewer.senior"），元素 Schema 的宏可在其数组规则中使用。
//   - mongoquery.Options.Prefix 把所有顶层路径移到 "data" 下。
package main

import (
	"encoding/json"
	"fmt"
	"time"

	c "github.com/k0923/go/condition"
	"github.com/k0923/go/condition/mongoquery"
	"github.com/k0923/go/condition/rule"
)

// Record is the custom data of one item, as decoded from MongoDB.
// Record 是一条数据的自定义字段，即从 MongoDB 读出的 data。
type Record = map[string]any

func get[V any](key string) func(Record) V {
	return func(r Record) V { v, _ := r[key].(V); return v }
}

func has(key string) func(Record) bool {
	return func(r Record) bool { v, ok := r[key]; return ok && v != nil }
}

// schema registers the fields of a labelling record and the macros that
// name its business rules. Optional fields may be missing in a record.
// schema 注册标注记录的字段，以及给业务规则命名的宏。Optional 字段在记录中可能缺失。
func schema() *c.Schema[Record] {
	field := func(name string) *rule.Field { return &rule.Field{Name: name} }
	num := func(n float64) *rule.Number { return &rule.Number{V: n} }

	// A reviewer is a nested object; its macro is lifted as "reviewer.senior".
	// reviewer 是嵌套对象，其宏会被提升为 "reviewer.senior"。
	reviewer := c.NewSchema[Record]().
		Number("level", get[float64]("level")).
		Strings("skills", get[[]string]("skills")).Optional("skills", has("skills")).
		Macro("senior", nil, func([]rule.Expr) rule.Cond {
			return &rule.Compare{Type: rule.TypeNumber, X: field("level"), Op: rule.OpGte, Y: num(3)}
		})

	// Labels are a list; "disputed" is usable inside array rules over them.
	// labels 是列表，"disputed" 可在针对它的数组规则中使用。
	labels := c.NewSchema[Record]().
		String("value", get[string]("value")).
		Number("confidence", get[float64]("confidence")).
		Macro("disputed", nil, func([]rule.Expr) rule.Cond {
			return &rule.Compare{Type: rule.TypeNumber, X: field("confidence"), Op: rule.OpLt, Y: num(0.6)}
		})

	return c.NewSchema[Record]().
		Number("score", get[float64]("score")).Optional("score", has("score")).
		String("source", get[string]("source")).
		Date("updated_at", get[time.Time]("updated_at")).Optional("updated_at", has("updated_at")).
		List("labels", func(r Record) []Record { return get[[]Record]("labels")(r) }, labels).
		Object("reviewer", get[Record]("reviewer"), reviewer).Optional("reviewer", has("reviewer")).
		// high_risk: a low score, or any disputed label. A missing score is
		// not low: comparisons on missing fields are false.
		// high_risk：分数低，或存在有争议的标注。缺失的分数不算低：缺失字段上的比较为 false。
		Macro("high_risk", nil, func([]rule.Expr) rule.Cond {
			return &rule.Or{Items: []rule.Cond{
				&rule.Compare{Type: rule.TypeNumber, X: field("score"), Op: rule.OpLt, Y: num(60)},
				&rule.Quant{X: field("labels"), Op: rule.OpAny, Y: &rule.Call{Name: "disputed"}},
			}}
		}).
		// active_since(t): updated at or after t. Arguments are checked
		// against the declared kinds; a JSON date is an RFC 3339 string.
		// active_since(t)：在 t 及之后更新过。参数按声明的类型校验；JSON 中的日期是 RFC 3339 字符串。
		Macro("active_since", []c.Kind{c.KindDate}, func(a []rule.Expr) rule.Cond {
			return &rule.Compare{Type: rule.TypeDate, X: field("updated_at"), Op: rule.OpGte, Y: a[0]}
		}).
		// needs_review(sources): a high-risk record from one of sources that
		// no senior reviewer has handled. Macros call macros.
		// needs_review(sources)：来自指定来源、且未经资深审核员处理的高风险记录。宏可以调用宏。
		Macro("needs_review", []c.Kind{c.KindStrings}, func(a []rule.Expr) rule.Cond {
			return &rule.And{Items: []rule.Cond{
				&rule.Call{Name: "high_risk"},
				&rule.Compare{Type: rule.TypeString, X: field("source"), Op: rule.OpIn, Y: a[0]},
				&rule.Not{X: &rule.And{Items: []rule.Cond{
					&rule.Exists{X: field("reviewer")},
					&rule.Call{Name: "reviewer.senior"},
				}}},
			}}
		})
}

// The rule a user saved: short, and phrased in business terms.
// 用户保存的规则：简短，且使用业务术语。
const savedRule = `{"type":"and","items":[
	{"type":"call","name":"needs_review","args":[["crowd","model"]]},
	{"type":"call","name":"active_since","args":["2024-05-01T00:00:00+08:00"]}
]}`

func main() {
	day := func(d int) time.Time { return time.Date(2024, 5, d, 12, 0, 0, 0, time.UTC) }
	records := []Record{
		{"id": 1, "score": 40.0, "source": "crowd", "updated_at": day(3)},                                                          // low score
		{"id": 2, "score": 90.0, "source": "model", "updated_at": day(3), "labels": []Record{{"value": "cat", "confidence": 0.4}}}, // disputed label
		{"id": 3, "score": 40.0, "source": "crowd", "updated_at": day(3), "reviewer": Record{"level": 4.0}},                        // senior reviewed
		{"id": 4, "score": 40.0, "source": "crowd", "updated_at": day(3), "reviewer": Record{"level": 1.0}},                        // junior reviewed
		{"id": 5, "source": "crowd", "updated_at": day(3)},                                                                         // no score: not high risk
		{"id": 6, "score": 40.0, "source": "expert", "updated_at": day(3)},                                                         // other source
		{"id": 7, "score": 40.0, "source": "crowd"},                                                                                // never updated
		{"id": 8, "score": 40.0, "source": "model", "updated_at": time.Date(2024, 4, 30, 15, 0, 0, 0, time.UTC)},                   // = 2024-04-30 23:00 +08:00
	}

	s := schema()
	checked, err := s.CheckJSON([]byte(savedRule))
	if err != nil {
		panic(err)
	}

	// 1. Backends see the expansion: plain nodes, no calls.
	// 1. 后端看到的是展开结果：只有普通节点，没有调用。
	expanded, _ := json.MarshalIndent(checked.Root(), "", "  ")
	fmt.Println("== expanded rule:")
	fmt.Println(string(expanded))

	// 2. In memory.
	// 2. 内存求值。
	match := s.Build(checked)
	var ids []int
	for _, r := range records {
		if ok, _ := match.Match(r); ok {
			ids = append(ids, r["id"].(int))
		}
	}
	fmt.Println("\n== in-memory matches:", ids)

	// 3. MongoDB: custom data lives under "data", so every path is prefixed.
	//    Element paths (labels.confidence) stay relative to the element. The
	//    date is a time.Time in the filter (a BSON date for the driver); it
	//    prints as a string only because of json.Marshal here.
	// 3. MongoDB：自定义数据位于 "data" 下，所有路径都带上前缀；元素内路径（labels.confidence）仍相对元素。
	//    filter 中的日期是 time.Time（驱动会编码为 BSON date），这里显示为字符串只是因为 json.Marshal。
	filter, err := mongoquery.Filter(checked, mongoquery.Options{Prefix: "data"})
	if err != nil {
		panic(err)
	}
	pretty, _ := json.MarshalIndent(filter, "", "  ")
	fmt.Println("\n== MongoDB filter (Prefix: data):")
	fmt.Println(string(pretty))

	// 4. Mistakes in calls are configuration errors with a path.
	// 4. 调用中的错误是带路径的配置错误。
	fmt.Println("\n== rejected rules:")
	for _, bad := range []string{
		`{"type":"call","name":"low_score"}`,
		`{"type":"call","name":"active_since","args":["2024-05-01"]}`,
		`{"type":"call","name":"needs_review","args":[["crowd"], 1]}`,
		`{"type":"call","name":"disputed"}`,
	} {
		_, err := s.CheckJSON([]byte(bad))
		fmt.Println(" ", err)
	}
}
