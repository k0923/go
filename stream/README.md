# stream

`stream` 是一个基于 Go 泛型实现的链式集合处理包。它支持过滤、映射、
去重、限制、排序、分组和终端收集操作。

## 环境要求

- Go 1.27 或更高版本
- 模块路径：`github.com/k0923/go/stream`

```go
import "github.com/k0923/go/stream"
```

## API 概览

以下签名使用 Go 1.27 支持的方法级泛型语法：

```go
func From[T any](items []T) *Stream[T]
func Of[T any](items ...T) *Stream[T]

func (stream *Stream[T]) Filter(fn func(T) bool) *Stream[T]
func (stream *Stream[T]) Map[R any](fn func(T) R) *Stream[R]
func (stream *Stream[T]) Distinct[K comparable](fn func(T) K) *Stream[T]
func (stream *Stream[T]) Limit(num int) *Stream[T]
func (stream *Stream[T]) Sort(lessFn func(T, T) bool) *Stream[T]
func (stream *Stream[T]) GroupBy[K comparable](fn func(T) K) *GroupStream[K, T]

func (stream *Stream[T]) First() (T, bool)
func (stream *Stream[T]) Last() (T, bool)
func (stream *Stream[T]) ForEach(fn func(T))
func (stream *Stream[T]) ToSlice() []T

func (stream *GroupStream[K, T]) Filter(fn func(Group[K, T]) bool) *GroupStream[K, T]
func (stream *GroupStream[K, T]) Map[R any](fn func(Group[K, T]) R) *Stream[R]
func (stream *GroupStream[K, T]) Limit(num int) *GroupStream[K, T]
func (stream *GroupStream[K, T]) Sort(lessFn func(Group[K, T], Group[K, T]) bool) *GroupStream[K, T]
func (stream *GroupStream[K, T]) ToDictionary() map[K][]T
```

## 快速开始

```go
numbers := stream.Of(1, 2, 3, 4, 5).
	Filter(func(value int) bool {
		return value%2 == 0
	}).
	Map(func(value int) string {
		return fmt.Sprintf("n=%d", value)
	}).
	ToSlice()

fmt.Println(numbers) // [n=2 n=4]
```

`From` 用已有切片创建 Stream：

```go
values := []int{1, 2, 3}
numbers := stream.From(values)
```

`From` 不复制传入的切片。`Of(values...)` 也可能与 `values` 共享底层数组。
两次终端操作之间对源切片的修改会被后续消费读取；消费 Stream 期间不要并发
修改源切片。

## 中间操作

中间操作不会立即遍历数据，而是返回一个新的 Stream。操作会严格按照调用顺序执行。

### Filter

保留满足条件的元素：

```go
result := stream.Of(1, 2, 3, 4).
	Filter(func(value int) bool {
		return value > 2
	}).
	ToSlice()

// result: [3 4]
```

### Map

将元素转换为其他类型：

```go
lengths := stream.Of("go", "stream").
	Map(func(value string) int {
		return len(value)
	}).
	ToSlice()

// lengths: [2 6]
```

映射函数在每次消费过程中对每个元素执行一次。

### Distinct

根据可比较的键保留首次出现的元素：

```go
type User struct {
	ID   int
	Name string
}

users := stream.Of(
	User{ID: 1, Name: "Alice"},
	User{ID: 1, Name: "Alice Updated"},
	User{ID: 2, Name: "Bob"},
).
	Distinct(func(user User) int {
		return user.ID
	}).
	ToSlice()

// 保留 ID=1 的第一个元素和 ID=2 的元素。
```

`Distinct` 在单次消费期间维护已出现键的集合，空间复杂度为 O(k)，其中 k 是
唯一键数量。

### Limit

最多保留指定数量的元素：

```go
result := stream.Of(1, 2, 3).Limit(2).ToSlice()
// result: [1 2]
```

当数量小于或等于零时，结果为空。

### Sort

按照比较函数排序：

```go
result := stream.Of(3, 1, 2).
	Sort(func(left, right int) bool {
		return left < right
	}).
	ToSlice()

// result: [1 2 3]
```

`Sort` 需要先收集其上游的全部元素，因此不是流式常量内存操作。比较函数应
提供一致的严格弱序关系。当前排序不稳定，相等元素的相对顺序不保证保持。

## GroupBy 和 ToDictionary

`GroupBy` 是惰性中间操作，返回 `GroupStream[K, T]`。分组顺序与键首次出现
的顺序一致，同一分组内保持源元素顺序。首次消费时需要遍历并缓存全部上游
元素，空间复杂度为 O(n)。缓存仅属于本次消费；下一次终端操作会重新读取并
分组上游数据。

```go
type User struct {
	Name string
	Team string
}

groups := stream.Of(
	User{Name: "Alice", Team: "A"},
	User{Name: "Bob", Team: "B"},
	User{Name: "Amy", Team: "A"},
).
	GroupBy(func(user User) string {
		return user.Team
	})
```

每个分组使用 `Group[K, T]` 表示：

```go
type Group[K comparable, T any] struct {
	Key    K
	Values []T
}
```

`GroupStream` 支持 `First`、`Last`、`ForEach`、`ToSlice` 和
`ToDictionary`，也可以继续执行 `Filter`、`Sort`、`Limit` 和 `Map`：

```go
largeGroups := groups.
	Filter(func(group stream.Group[string, User]) bool {
		return len(group.Values) >= 2
	}).
	ToSlice()
```

`GroupStream.Map` 返回普通的 `Stream[R]`。映射后分组键不再被保留，因此不能
继续调用无参数的 `ToDictionary`。

使用 `ToDictionary` 将分组收集为 `map[K][]T`：

```go
usersByTeam := groups.ToDictionary()

fmt.Println(usersByTeam["A"])
// [{Alice A} {Amy A}]
```

空分组流会返回非 nil 的空 map。Go 的 map 不保证遍历顺序；需要稳定分组顺序
时应使用 `ToSlice`。

## 终端操作

### ToSlice

将 Stream 收集为切片：

```go
values := stream.Of(1, 2, 3).ToSlice()
```

空的 `Stream` 或 `GroupStream` 返回非 nil 的空切片。`ToSlice` 需要 O(n)
额外空间。返回的切片容器由调用方持有，修改其长度或元素位置不会改变 Stream
的迭代状态。元素本身是浅拷贝；若元素包含指针、切片或 map，其引用数据仍
可能共享。

### First 和 Last

返回第一个或最后一个元素，并通过 `bool` 表示元素是否存在：

```go
first, ok := stream.Of(1, 2, 3).First()
if ok {
	fmt.Println(first)
}

last, ok := stream.Of[int]().Last()
fmt.Println(last, ok) // 0 false
```

`First` 在上游允许时取得第一个元素后停止遍历；如果上游包含 `Sort` 或
`GroupBy`，这两个操作仍需先消费全部输入。`Last` 会遍历整个 Stream。无值时
返回元素类型的零值和 `false`。

### ForEach

依次处理每个元素：

```go
stream.Of(1, 2, 3).ForEach(func(value int) {
	fmt.Println(value)
})
```

### ToDictionary

`ToDictionary` 是 `GroupStream` 的终端操作：

```go
dictionary := stream.Of("alice", "bob", "amy").
	GroupBy(func(name string) int {
		return len(name)
	}).
	ToDictionary()

// map[int][]string{
//     3: {"bob", "amy"},
//     5: {"alice"},
// }
```

`ToDictionary` 需要 O(n) 额外空间，并始终返回非 nil 的 map。

## 执行语义

- `Filter`、`Map`、`Distinct`、`Limit`、`Sort` 和 `GroupBy` 均为惰性操作。
- `ToSlice`、`First`、`Last`、`ForEach` 和 `ToDictionary` 会触发遍历。
- 每次终端操作都会创建独立迭代器，因此同一个 Stream 可以重复消费。
- 从同一个 Stream 派生出的多个 Stream 相互独立。
- 操作顺序会影响结果，例如 `Limit(1).Filter(fn)` 与
  `Filter(fn).Limit(1)` 语义不同。
- 回调函数中的 panic 会直接向调用方传播。

Stream 本身不修改源切片。返回的 `Group.Values` 和 `ToDictionary` 中的值切片
由本次消费创建，但其中的元素仍是浅拷贝。

每次消费使用的排序和分组缓存彼此独立，不会在 Stream 实例之间共享。该包不
为源数据或用户回调提供并发同步；多个 goroutine 同时消费 Stream 时，调用方
必须确保源切片和回调捕获的状态可以安全并发访问。
