# OrderedMap 性能与兼容性评估

评估的是 `github.com/wk8/go-ordered-map/v2 v2.1.8`。这是独立 Go 模块，通过 replace 使用当前仓库的 xjson/xfield，主项目无需增加 OrderedMap 依赖。根目录的 `go test ./xfield/...` 不包含这个模块，需要单独运行。

```sh
cd xfield/examples/orderedmap
go test -race -v ./...
go test -race github.com/wk8/go-ordered-map/v2
go test -run '^$' -bench . -benchmem -benchtime=100ms -count=5 -cpu=1
```

[兼容性测试](compatibility_test.go)检查顺序、覆盖、删除后重新插入、移动、中文与转义 key、嵌套 JSON、数字精度、并发只读，以及 OrderedMap 中的 xjson.G 如何转换为 xfield 声明。它也记录该版本的实际边界行为。

[基准测试](benchmark_test.go)比较 `map[string]any`、`OrderedMap[string, any]` 和 `[]entry`。查找测试使用预先生成的字段名，按顺序循环命中；遍历测试访问并累加每个值。构建时都预分配容量，键和值在计时前生成。slice 构建只是复制列表，没有去重或哈希索引，所以它只代表当前有序字段列表的成本。

JSON 编解码比较的都是相同扁平字符串键及数字值，实际 xjson 字段可能有不同成本。分配统计是每次操作分配的字节数和次数，不等于长期驻留内存。

实测汇总见 [REPORT.md](REPORT.md)，原始结果见 [benchmark.txt](benchmark.txt)。性能测试不启用 race，兼容性测试启用 race。这里没有把现有 xfield Schema 改为 OrderedMap。
