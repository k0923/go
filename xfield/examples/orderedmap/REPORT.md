# wk8 OrderedMap 实测评估

测试日期：2026-10-25（Asia/Shanghai）。版本：`github.com/wk8/go-ordered-map/v2 v2.1.8`。环境：Go 1.27.1、macOS darwin/arm64、Apple M4 Pro。性能测试使用 `-cpu=1 -benchtime=100ms -count=5`，以下为五次测量的中位数；实际应用的 key 分布、字段类型和机器会影响结果。

后续迁移说明（2026-10-26）：xfield 的 Schema 已改为原生 map，前端数组通过显式转换保留在应用层。兼容性测试已迁移至新 API；本报告的容器基准数据保留为原始实测结果。

结论：查找接近原生 map，按 key 查找比线性扫描快；按顺序遍历比连续切片慢；构建需要更多分配。可以用来保存有序 Schema，但现有 xfield 接口需要适配。对任意 JSON 数据值，它不自动提供嵌套顺序和大整数精度。

## 性能

| 操作 | 字段数 | 原生 map | OrderedMap | 有序切片 |
| --- | ---: | ---: | ---: | ---: |
| 命中查找 | 100 | 6.03 ns | 6.10 ns | 88.5 ns |
| 命中查找 | 1,000 | 6.17 ns | 6.54 ns | 759 ns |
| 完整遍历 | 100 | 433 ns | 236 ns | 34.0 ns |
| 完整遍历 | 1,000 | 5.35 μs | 2.51 μs | 0.297 μs |
| 构建 | 100 | 1.26 μs | 4.45 μs | 0.456 μs |
| 构建 | 1,000 | 13.7 μs | 48.1 μs | 4.11 μs |
| JSON 编码 | 100 | 17.5 μs | 12.2 μs | — |
| JSON 编码 | 1,000 | 210 μs | 136 μs | — |
| JSON 解码 | 100 | 16.7 μs | 31.3 μs | — |
| JSON 解码 | 1,000 | 189 μs | 333 μs | — |

构建 100 个键时的分配：

| 容器 | 分配字节/操作 | 分配次数/操作 |
| --- | ---: | ---: |
| 原生 map | 4,952 B | 4 |
| OrderedMap | 11,640 B | 207 |
| 有序切片 | 3,480 B | 2 |

这里的分配字节是构建期间累计分配量，不是驻留内存测量。slice 构建没有建立 key 索引或拒绝重复键。查找及遍历均为零分配。OrderedMap 的实现为 map 加双向链表，每个键分别分配 Pair 和链表节点。

JSON 编码测试中 OrderedMap 较快，因为无需像标准 map 编码一样排序键，但它分配的字节更多。原生 map 的输出顺序是标准库排序结果，OrderedMap 输出插入顺序。此结果只针对扁平字符串键和简单数值，未测整个业务请求的吞吐。

## 兼容性

自己的兼容性测试（8 个）、该库自带测试均通过 Go 1.27.1 的 race 检查；本次没有运行新的 fuzz 轮次。模块 `go vet` 通过。

- 插入顺序稳定；更新已存在 key 保持位置；删除后重新插入追加到末尾；MoveBefore 正常。
- JSON 编解码保留当前映射的顺序；中文、转义 key 和显式 null 正常。
- `OrderedMap[string, xjson.G[xfield.Field[Params]]]` 能恢复具体字段并保留顺序；转换为 xfield 声明后，可编译 Dialog 引用并校验数据、查找嵌套路径。
- 编译后的 xfield 数据处理仍使用普通 map。OrderedMap 是 struct，不属于现有 xfield `ObjectField.Parse` / 路径工具识别的原生 map；直接传入会失败。需显式转换或新增容器适配接口。
- `OrderedMap[string, any]` 内的嵌套 JSON 对象是普通 `map[string]any`。只有显式声明嵌套 OrderedMap 才保留每一层顺序。
- any 中数字是 float64。`9007199254740993` 实测变为 `9007199254740992`；外层 `Decoder.UseNumber()` 不会传入该库内部的 `json.Unmarshal`。使用 `OrderedMap[string, json.Number]` 或 RawMessage 可保留精度。
- JSON 解码到已有 OrderedMap 会合并旧内容，旧 key 保持原来的位置。需要重新接收完整配置时应解码到新实例。
- 重复 JSON key 使用最后一个值，但保留第一次出现的位置；它不会拒绝重复键。需要配置唯一性时应在协议入口另行检查。
- 并发只读通过 race；该库没有锁，读写混用或并发写入需要使用方同步。没有用并发写入作为受支持场景测试。

## 发布与维护

通过 Go 模块解析 `@latest`，截至检查时最新正式版为 v2.1.8，发布日期为 2023-06-27，最低 Go 版本 1.18。

通过浅克隆查看当前 master，HEAD 为 `46d686821e33c3099739cbd52b7f4eca94cc2203`，最近提交为 2025-03-31 18:07:20 -07:00（北京时间 2025-04-01）。当前 master 的 go.mod 要求 Go 1.23，有未发布的迭代器等改动。正式版与主分支 README 的 API 范围并不完全一致。

仓库提供单元测试、JSON/YAML fuzz 测试及 CircleCI 配置。配置中使用 race 和覆盖率，并各运行 JSON/YAML fuzz；当前 CI 配置只列出 Go 1.23。本次检查的是配置和本地实测，未确认远程 CI 的最新运行状态。

正式版有 Go 主版本路径 `/v2`，核心容器 API 已使用数年，可以固定版本使用。但三年多没有正式发布，维护活跃度较低；不能据此保证后续修复响应时间。它还带有 generic-list-go、jsonparser、easyjson、yaml.v3 运行依赖。本次未执行漏洞扫描。

参考：

- [正式版 API](https://pkg.go.dev/github.com/wk8/go-ordered-map/v2@v2.1.8)
- [发布记录](https://github.com/wk8/go-ordered-map/releases)
- [当前提交](https://github.com/wk8/go-ordered-map/commit/46d686821e33c3099739cbd52b7f4eca94cc2203)
- [正式版 JSON 实现](https://github.com/wk8/go-ordered-map/blob/v2.1.8/json.go)

## 对 xfield 的建议

对需要频繁按 key 查找并保持编辑顺序的 Schema，这个容器可以满足需求。应先定义一个 xfield 自己的有序属性接口，再把第三方容器接入，避免所有公开 API 固定依赖具体 OrderedMap 实现。

如果 Schema 主要是编译后反复遍历，现有切片加只读 map 索引也值得采用：保留连续遍历和 O(1) 查找，不需要每个字段的链表节点。当前结果没有测试这种组合，也没有把它或 OrderedMap 应用到 xfield 核心。
