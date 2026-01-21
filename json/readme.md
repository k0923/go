# xjson: Go 语言增强型 JSON 库

`xjson` 是一个轻量级的 Go 语言 JSON 扩展库，旨在解决标准库 `encoding/json` 在处理 **多态（Polymorphism）** 和 **可选字段（Optional Fields）** 时的痛点。

## 主要特性

1.  **多态类型绑定 (`G[T]`)**: 支持将接口类型或抽象基类反序列化为具体的实现类型，支持默认布局和扁平化布局。
2.  **可选字段支持 (`Optional[T]`)**: 能够精确区分 JSON 中的 `null`、`undefined`（未设置）和 `value`（有值）。
3.  **高度可配置**: 支持自定义类型键名、值键名、初始化钩子以及底层 JSON 序列化库（如 `sonic` 或 `json-iterator`）。

---

## 1. 多态类型支持 (`G[T]`)

在 Go 中，标准库难以直接将 JSON 反序列化为接口类型。`xjson` 通过泛型容器 `G[T]` 和注册机制解决了这个问题。

### 基础用法

假设我们有一个 `Shape` 接口和两个实现 `Circle` 和 `Rectangle`。

#### 1. 定义接口和结构体

```go
type Shape interface {
    Area() float64
}

type Circle struct {
    Radius float64 `json:"radius"`
}
func (c Circle) Area() float64 { return 3.14 * c.Radius * c.Radius }

type Rectangle struct {
    Width  float64 `json:"width"`
    Height float64 `json:"height"`
}
func (r Rectangle) Area() float64 { return r.Width * r.Height }
```

#### 2. 注册类型绑定

使用 `xjson.Bind` 注册类型映射关系。建议在 `init()` 中进行。

```go
func init() {
    xjson.Bind(map[string]Shape{
        "circle":    Circle{},
        "rectangle": Rectangle{},
    })
}
```

#### 3. 使用 `G[T]` 容器

在包含多态字段的结构体中使用 `G[Shape]` 代替 `Shape` 接口。

```go
type Canvas struct {
    // 使用 G[T] 包装接口
    Item xjson.G[Shape] `json:"item"`
}

func main() {
    // 序列化
    c := Canvas{
        Item: xjson.NG[Shape](Circle{Radius: 10}),
    }
    data, _ := json.Marshal(c)
    // 默认输出: {"item":{"type":"circle","data":{"radius":10}}}

    // 反序列化
    jsonStr := `{"item":{"type":"rectangle","data":{"width":10,"height":5}}}`
    var c2 Canvas
    json.Unmarshal([]byte(jsonStr), &c2)
    
    // 获取真实值
    shape := c2.Item.Value() // 返回 Shape 接口
    fmt.Println(shape.Area()) // 50
}
```

### 高级配置 (`BindOption`)

`Bind` 函数支持多种选项来定制序列化行为。

#### 扁平化布局 (`WithFlatLayout`)

默认情况下，`xjson` 使用嵌套结构（`type` + `data`）。使用 `WithFlatLayout` 可以支持扁平化 JSON，将类型字段合并到数据中。

```go
xjson.Bind(map[string]Shape{
    "circle": Circle{},
}, xjson.WithFlatLayout())

// 序列化结果: {"type":"circle", "radius":10}
```

#### 自定义键名 (`WithTypeKey`, `WithValueKey`)

修改默认的 `type` 和 `data` 键名。

```go
xjson.Bind(map[string]Shape{...}, 
    xjson.WithTypeKey("kind"), 
    xjson.WithValueKey("payload"),
)

// 序列化结果: {"kind":"circle", "payload":{...}}
```

#### 初始化钩子 (`WithInitializer`)

在反序列化后自动执行初始化逻辑（例如设置默认值）。

```go
xjson.Bind(map[string]User{...}, xjson.WithInitializer(func(u User) User {
    if u.Age == 0 {
        u.Age = 18 // 默认值
    }
    return u
}))
```

#### 自定义底层序列化库 (`WithJSONHandler`)

你可以替换默认的 `encoding/json`，使用性能更好的库（如 `bytedance/sonic`）。

```go
import "github.com/bytedance/sonic"

xjson.Bind(map[string]Shape{...}, xjson.WithJSONHandler(
    sonic.Marshal, 
    sonic.Unmarshal,
))
```

#### 手动解析 (`ParseFromJSON`)

如果不使用 `G[T]` 容器，也可以手动解析：

```go
data := []byte(`{"radius": 10}`)
// 显式指定类型别名 "circle"
g, err := xjson.ParseFromJSON[Shape]("circle", data)
if err == nil {
    shape := g.Value()
}
```

---

## 2. 可选字段支持 (`Optional[T]`)

`Optional[T]` 用于处理 JSON 中的 "零值" 问题，能够区分字段是**不存在**、**为 null** 还是**零值**。

### 三种状态

1.  **Undefined (未定义)**: JSON 中没有该字段。`IsUndefined() == true`
2.  **Null (空值)**: JSON 中该字段为 `null`。`IsNull() == true`
3.  **Value (有值)**: JSON 中该字段有具体值。`HasValue() == true`

### 使用示例

```go
type People struct {
    // 必须配合 omitempty 使用，以便在 Undefined 时不输出字段
    Name xjson.Optional[string] `json:"name,omitempty"`
    Age  xjson.Optional[int]    `json:"age,omitempty"`
}

func main() {
    // 1. 创建有值的 Optional
    p1 := People{
        Name: xjson.NO("Alice"),
        Age:  xjson.NO(25),
    }
    // {"name":"Alice","age":25}

    // 2. 创建 Null 值
    p2 := People{
        Name: xjson.Null[string](),
    }
    // {"name":null}

    // 3. 创建 Undefined (不赋值即可)
    p3 := People{}
    // {}
}
```

### 注意事项

1.  **不要使用指针**: 字段定义请直接使用 `Optional[T]`，不要使用 `*Optional[T]`。`Optional` 内部已经是引用类型（切片），直接使用即可。
2.  **omitempty**: 务必在 JSON tag 中添加 `omitempty`，否则 `Undefined` 状态可能会被序列化为 `null`。

```go
// ✅ 正确
Name xjson.Optional[string] `json:"name,omitempty"`

// ❌ 错误 (无法区分 Undefined 和 Null)
Name *xjson.Optional[string] `json:"name,omitempty"`

// ❌ 错误 (Undefined 会变成 null)
Name xjson.Optional[string] `json:"name"`
```
