# orderedmap

以插入顺序遍历、编码 JSON 的字符串键 Map。解码 JSON 时保留 key 的出现顺序，无第三方依赖。

```go
import (
    "encoding/json"
    "fmt"

    "github.com/k0923/go/orderedmap"
)

var prices orderedmap.Map[int] // 零值可用
prices.Set("banana", 20)
prices.Set("apple", 10)
prices.Set("banana", 25) // 更新值，不改变位置

for key, value := range prices.All() {
    fmt.Println(key, value) // banana 25，然后 apple 10
}

data, err := json.Marshal(prices) // {"banana":25,"apple":10}
if err != nil {
    panic(err)
}
var restored orderedmap.Map[int]
if err := json.Unmarshal(data, &restored); err != nil {
    panic(err)
}
```

## API

| 方法 | 行为 |
| --- | --- |
| `Set(key string, value V)` | 新 key 追加到末尾；更新已有 key 保持位置 |
| `Get(key string) (V, bool)` | 查询；不存在时返回 V 的零值及 false |
| `Delete(key string) bool` | 删除并返回是否存在；重新插入时追加到末尾 |
| `Len() int` | 返回 key 数量 |
| `Keys() []string` | 按顺序返回独立的 key 切片 |
| `All() iter.Seq2[string, V]` | 按顺序遍历，支持 range 和 break |

内部使用 `map[string]V` 保存值、`[]string` 保存 key 顺序。查询和更新平均 O(1)，插入平均摊销 O(1)；删除已有 key 为 O(n)，需要定位并移动顺序切片中的元素。遍历与取出所有 key 为 O(n)。

适合构建后多读、较少删除的配置或字段声明。删除会清空切片末尾不再使用的字符串引用，但容器容量不会自动收缩。

## JSON

- 可直接使用 `json.Marshal`、`json.Unmarshal`、`json.Encoder`、`json.Decoder`，也可作为结构体字段、切片元素或其他 map 的值。
- 解码成功后整体替换旧内容，顺序以输入为准；解码失败时原 Map 不变。
- 空 Map 编码为 `{}`，JSON `null` 解码到已有 Map 时将其清空。标准库将 nil `*Map` 编码为 `null`；解码到 `**Map` 时也遵循标准库的指针规则。
- 重复 key 返回 `*DuplicateKeyError`，支持 `errors.Is(err, orderedmap.ErrDuplicateKey)` 及 `errors.As`。转义后相同的 key 也视为重复。
- 仅保留 key 顺序，不保留空格、缩进、原始转义写法。

### 显式声明嵌套类型

```go
var groups orderedmap.Map[orderedmap.Map[int]]
err := json.Unmarshal([]byte(`{"fruit":{"z":1,"a":2},"other":{"y":3,"b":4}}`), &groups)
```

此时外层及内层均保序。动态值的嵌套对象使用标准库行为：

```go
var document orderedmap.Map[any]
// 解码后，嵌套对象是 map[string]any，数组是 []any，数字是 float64。
// 只有 document 自身的 key 保序。
```

需要动态嵌套 Map 时，也可手动将 `*orderedmap.Map[any]` 放入 value，编码时会保序。需要准确表示大整数时，使用 `Map[json.Number]` 或 `Map[json.RawMessage]` 等显式类型；外层 Decoder 的 `UseNumber` / `DisallowUnknownFields` 设置不会传入自定义的 `UnmarshalJSON`。

## 使用边界

- Map 使用后不要复制；共享、传参及修改嵌套对象时使用指针。JSON 编码可以传值，编码过程只读。
- 并发读写需要调用方加锁；无写入时支持并发读取。
- 遍历期间不支持修改 Map；迭代器不是快照。
- 普通 map 已丢失插入顺序，通过遍历普通 map 调用 `Set` 无法恢复原始顺序。

完整示例及运行命令见 [examples](examples/README.md)。
