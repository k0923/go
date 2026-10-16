# condition 示例

每个子目录都是一个可以直接运行的程序，演示一组特性。`expected_output.txt` 是它的预期输出，`go test ./condition/examples` 会逐一运行这些程序并比对输出。

```bash
go run ./condition/examples/builder       # Go 构建 API
go run ./condition/examples/collections   # 量词、Len、算术、错误处理
go run ./condition/examples/jsonrules     # JSON 规则、Schema、编译期校验
go run ./condition/examples/integration   # 接入业务配置、计算字段、并发共享
go run ./condition/examples/query         # 同一份 JSON：内存判断 + SQL + MongoDB
go test ./condition/examples              # 校验全部示例输出
go test ./condition/examples -update      # 修改示例后重新生成预期输出
```

## 特性索引

| 特性 | API | 示例 |
|---|---|---|
| 字段与常量 | `Field`、`Const`、`ExprFunc` | builder |
| 相等 / 大小比较 | `Eq` `Ne` `Lt` `Le` `Gt` `Ge` | builder |
| 字符串（支持命名类型） | `Contains` `HasPrefix` `HasSuffix` | builder |
| 集合判断 | `In` `NotIn`（字面量）、`InList` `NotInList`（动态列表） | builder |
| 逻辑组合 / 短路 | `And` `Or` `Not`、`True` `False` | builder |
| 自定义谓词 | `CondFunc` | builder |
| 常量折叠 | 构建期自动完成 | builder |
| 列表量词（元素类型独立） | `Any` `All` `None` | collections |
| 长度 | `Len` | collections |
| 算术 | `Add` `Sub` `Mul` `Div`、`ErrDivideByZero` | collections |
| 错误透传与短路保护 | `And(guard, risky)` | collections |
| 字段白名单 | `NewSchema`、`Bool` `String` `Number` `Strings` `Numbers` `List` | jsonrules |
| JSON 编译 | `CompileJSON`、`Compile`、`ParseNode` | jsonrules |
| 代码构建 Node 并序列化 | `Op(OpXxx, …)` `Ref` `LitNum/LitStr/LitBool/LitNums/LitStrs` | jsonrules |
| 编译错误定位 | `*CompileError{Path}`、`ErrUnknownField` 等 | jsonrules |
| 标量列表元素引用 | `{"field":"$"}`（`SelfField`） | jsonrules、integration |
| 嵌入业务配置 | `json.RawMessage` 加 `CompileJSON` | integration |
| 计算字段（扩展点） | Schema 注册任意函数 | integration |
| 并发共享 | 编译结果不可变 | integration |
| 完整复杂规则 JSON | [rule.json](query/rule.json) | query |
| 生成 SQL（SQLite/PostgreSQL/MySQL） | `Check` → `sqlquery.Where` | query |
| 生成 MongoDB filter | `Check` → `mongoquery.Filter` | query |

## 一份结构，三种用途

```text
rule.json ─ParseNode─> Node ─Schema.Check─> TypedNode ─┬─ Schema.Compile    → Condition[T]，内存 Match
                                                        ├─ sqlquery.Where    → WHERE 子句 + 参数
                                                        └─ mongoquery.Filter → MongoDB filter 文档
```

- `Schema` 是字段名和类型的唯一来源。`Check` 只校验一次，生成与后端无关的 `TypedNode`：每个字段都已解析，每个操作数的类型都已确定。
- 内存求值和查询生成读的是同一个 `TypedNode`，规则在各处的含义完全一致。sqlquery 用 SQLite 做差分测试，mongoquery 用参考求值器做差分测试，同一批数据上两边的结果必须和 `Match` 完全相同，并配有 fuzz 测试。
- 以后要接 ES、ClickHouse 等新后端，只需要再写一个读取 `TypedNode` 的翻译器，不用改规则格式和 Schema。

### 类型安全：`any` 只停留在 JSON 边界

`Node.Value` 是 `any`，原因只有一个：它是 JSON 的传输格式。`Check` 之后不会再出现 `any`：

| 层 | 类型 | 出错时 |
|---|---|---|
| JSON / `Node` | `Op string`、`Value any` | `Check` 返回带路径的 `*CompileError`，例如 `$.args[1]: type mismatch` |
| Go 代码构建 `Node` | `Op(condition.OpGe, …)`、`LitNum` / `LitStr` / `LitBool` / `LitNums` / `LitStrs` | 写错运算符名、传入 map 或 struct 字面量，**编译不通过** |
| `TypedNode`（后端读取） | `Op Operator`（枚举）、`Lit *Literal`（按 `Kind` 取值：`Number()`、`Strings()` …） | 运算符拼写错误编译不通过；按错误的 Kind 取值会**立即 panic**，不会悄悄得到零值 |

另外有两道测试防线：

- `TestEveryOperatorCovered`（sqlquery、mongoquery 各一份）遍历 `condition.Operators()`，只要新增了运算符却没有对应的差分用例，测试就会失败。这条测试刚加上就发现 `le` 和 `sub` 此前从未经过差分验证，现已补上。
- 差分测试加 fuzz：同一条规则分别跑内存 `Match` 和 SQLite / Mongo 参考求值器，结果必须一致。

后端映射：

| 规则 | SQL | MongoDB |
|---|---|---|
| `eq/ne/lt/le/gt/ge`（字段 vs 字面量） | `=` `<>` `<` …，参数绑定 | `$eq` `$ne` `$lt` …（可用索引） |
| `in/not_in` 字面量列表 | `IN (?, ?)` / `NOT (… IN …)` | `$in` / `$nin` |
| `in` 列表字段 | `EXISTS` 子表 / PG `= ANY(col)` | `{tags: "x"}` |
| `starts_with/ends_with/contains` | PG/MySQL：`LIKE`（已转义）；SQLite：`substr`/`instr` | `$regex`（已转义） |
| `any/all/none` 对象列表 | `EXISTS` / `NOT EXISTS` 子表 | `$elemMatch` / `$not: {$elemMatch}` |
| `any/all/none` 标量列表 | 子表 / PG `unnest` | `$elemMatch` 或 `$expr`+`$map` |
| 两字段比较、算术、`len` | 原生表达式、`COUNT(*)` | `$expr`（不走索引） |

需要注意的语义约束：

- **SQL**：默认列为 NOT NULL；字符串比较遵循列的排序规则，要和 Go 一样按字节比较，需要设置 `Options.Collate`；列表字段要么配置 `Relation` 映射到子表，要么在 PostgreSQL 上用数组列；除以 0 在 SQLite/MySQL 中得到 NULL（该行不匹配），而内存求值和 PostgreSQL 会报错。
- **MongoDB**：文档应包含所有注册字段且类型正确；`$ne`/`$nin` 也会匹配缺失该字段的文档；缺失或为 null 的数组按空数组处理。

## JSON 场景的开发规范：不再使用 xjson.G

**结论：condition 的 JSON 场景不要求、也不建议用 `xjson.G` 实现任何接口。** 新实现里，JSON 规则唯一的入口是 `Node` 加 `Schema`。

### 为什么不再需要 xjson.G

旧实现用 `G[Condition[T]]`、`G[Picker[T,E]]` 做多态反序列化：JSON 里的类型名先映射到具体的 Go 结构体，再由结构体在每次 `Match` 时解释字符串 `Opt`。这带来了几个问题：

- 必须在全局用 `Bind` 注册类型映射，而且每种 `T` 都要单独注册一遍；
- 配置错误要到运行时才暴露，比如拼错的 `Opt`、缺失的操作数，后者还会被静默当成零值；
- 字段访问只能通过实现 `Picker` 接口来扩展，每个字段都得写一个类型。

新实现中 `Node` 是一个普通的结构体，标准库 `encoding/json` 就能直接编解码。具体实现在编译时根据 `op` 和 Schema 中登记的字段类型选出来，不需要运行时多态。

### 规范

1. **规则统一用 `Node` 表达**，结构为 `{"op","args"}`、`{"field"}` 或 `{"value"}`，每个节点只能设置其中一项。不要再定义新的 `Condition` 实现类型并通过 `xjson.Bind` 注册。
2. **规则以 `json.RawMessage` 保存，用 `Schema.CompileJSON` 编译。** `CompileJSON` 会拒绝未知字段和多余的尾部数据。直接 `json.Unmarshal` 到 `Node` 会**静默忽略**拼错的键（例如 `"opt"`），只适合可信的数据来源。
3. **业务能力通过 Schema 字段扩展，不通过接口扩展。** 需要自定义判断时，把它注册成计算字段，例如 `Bool("is_weekend", fn)`、`Number("user_bucket", fn)`。JSON 规则只能访问已注册的字段，Schema 也就同时充当了安全白名单。
4. **在加载阶段编译，并把编译结果缓存起来复用。** 编译错误应作为配置错误让加载失败，不要拖到请求时再处理。`Condition` 不可变，可以在多个 goroutine 之间共享。
5. **Schema 在初始化阶段注册完毕。** 注册完成后不要再修改，也不要在 `Compile` 的同时修改。
6. **数值约束**：JSON 模式下数值统一按 `float64` 比较，超过 2^53 的整数（例如雪花 ID）不能用 `Number` 注册，应该用 `String` 注册后按字符串比较。
7. **Go 代码中的规则直接使用构建 API**，如 `And`、`Ge`、`Field` 等，能获得编译期的类型检查，也不需要绕道 JSON。

### 与 xjson.G 共存

`xjson.G` 仍然适合**其他**多态模型，例如工作流节点、消息体。如果某个多态类型里含有规则，把规则字段声明为 `json.RawMessage`，在该类型的初始化钩子或加载逻辑里调用 `CompileJSON`。两套机制各管一层，互不依赖：

```go
type DiscountAction struct {
	Rule    json.RawMessage `json:"rule"` // compiled by condition, not by xjson
	Percent float64         `json:"percent"`
}
```
