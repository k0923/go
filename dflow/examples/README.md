# dflow 示例

在 Go 模块根目录运行，无需外部服务或凭证。

| 场景 | 命令 | 说明 |
| --- | --- | --- |
| [minimal](minimal/README.md) | `go run ./dflow/examples/minimal` | 当前核心接口的完整最小接入：业务发布、初始化、事务提交、依赖取值和后继调度 |
| [basic](basic/main.go) | `go run ./dflow/examples/basic` | 接口实例定义、上游输出引用、JS 计算，输出 `map[total:42]` |
| [custom_node](custom_node/main.go) | `go run ./dflow/examples/custom_node` | 直接注入自定义节点和服务、工作流输入引用，输出 `你好，DAG！` |
| [http](http/main.go) | `go run ./dflow/examples/http` | 本地 HTTP 服务、查询参数、响应 JSON 解析，输出 `你好，HTTP` |
| [suspend](suspend/main.go) | 见下方命令 | 节点返回 async 挂起结果，SQLite 保存状态，业务回调后再次 Drive |
| [xjson](xjson/README.md) | `go run ./dflow/examples/xjson` | 业务 JSON DTO、类型绑定、节点依赖重建、SQLite 挂起与继续执行 |

引擎本身每次 `Drive` 只推进一个节点；这些示例通过参考驱动包 [driver](../driver/driver.go) 把整张 DAG 串行驱动到静止（`driver.Run`/`Drain`）。生产系统可改为把每个就绪节点 ID 入队，由多个 worker 各自调用 `engine.Drive`。async 节点未完成时会持久化为 `Suspended`，业务在外部结果到达后显式再次驱动该节点。

HTTP、JS、条件、起始、结束等参考节点集中在 [nodes](nodes/README.md)，这些实现通过示例侧的 `nodes.Spec` 适配到核心的 `dflow.NodeSpec`/`PublishedNode` 契约。核心不包含这些节点，也不对起始节点类型做特殊处理；需要完整工作流输入时显式绑定 `nodes.Input()`。

basic、custom_node 和 http 示例使用 `memory.NewRun()`。suspend 使用 `sqlite.NewRun()` 保存节点状态；[xjson](xjson/README.md) 使用业务 JSON DTO 保存定义，并在进程启动时重新构建可执行 publication。suspend 示例依次执行：

```sh
go run ./dflow/examples/suspend -db /tmp/dflow-lazy-suspend.db -run demo -init
go run ./dflow/examples/suspend -db /tmp/dflow-lazy-suspend.db -run demo -answer completed
```

第一次 `-init` 初始化 run 并执行 task，节点进入 `Suspended`。第二次进程重新构建同一份 publication，传入业务回调结果后再次驱动 task，随后 end 变为成功。重新演示时更换 `-run` 或删除旧数据库。

框架返回本次驱动触及的节点；示例显式调用 Load 观察持久化节点记录，通过 end 节点判断是否展示输出，没有工作流整体状态。

实际业务实现 `dflow.Infrastructure` 和 outbox 消费即可接入自己的存储与外部服务，契约见 [包说明](../README.md#infrastructure-和恢复)。
