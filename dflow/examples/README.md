# dflow 示例

在 Go 模块根目录运行，无需外部服务或凭证。

| 场景 | 命令 | 说明 |
| --- | --- | --- |
| [basic](basic/main.go) | `go run ./dflow/examples/basic` | 接口实例定义、上游输出引用、JS 计算，输出 `map[total:42]` |
| [custom_node](custom_node/main.go) | `go run ./dflow/examples/custom_node` | 直接注入自定义节点和服务、工作流输入引用，输出 `你好，DAG！` |
| [http](http/main.go) | `go run ./dflow/examples/http` | 本地 HTTP 服务、查询参数、响应 JSON 解析，输出 `你好，HTTP` |
| [suspend](suspend/main.go) | 见下方命令 | 节点返回挂起结果、SQLite 自动保存、跨进程调用 Resume |
| [xjson](xjson/README.md) | `go run ./dflow/examples/xjson` | 业务 JSON DTO、类型绑定、节点依赖重建、SQLite 挂起与回调恢复 |

引擎本身每次 `Drive` 只推进一个节点；这些示例通过参考驱动包 [driver](../driver/driver.go) 把整张 DAG 串行驱动到静止（`driver.Run`/`Drain`/`Resume`）。生产系统可改为把每个就绪节点 ID 入队，由多个 worker 各自调用 `engine.Drive`。子流程与循环由业务使用相同的 Suspension/Wakeup/Resume 协议组合。

HTTP、JS、条件、起始、结束等参考节点集中在 [nodes](nodes/README.md)，这些实现使用公开的 `dflow.Node` 契约，业务可以复制或自行替换。核心不包含这些节点，也不对起始节点类型做特殊处理；需要完整工作流输入时显式绑定 `dflow.Input()`。

basic、custom_node 和 http 示例使用 `memory.New()`。suspend 的编码器位于 [internal/appcodec](internal/appcodec/codec.go)，演示业务侧使用 gob 保存接口类型；[xjson](xjson/README.md) 则使用业务 JSON DTO 保存定义、挂起数据和具体回调类型，在解码时重新编译 JS 并注入服务依赖。suspend 示例依次执行：

```sh
go run ./dflow/examples/suspend -db /tmp/dflow-lazy-suspend.db -run demo -publish
go run ./dflow/examples/suspend -db /tmp/dflow-lazy-suspend.db -run demo -token '<第一次输出的 token>' -answer completed
```

第一次 -publish 校验并将定义按节点保存，然后执行 task.Run。第二次进程只使用已保存的发布 ID，按需加载 task 与就绪的 end，执行 task.Resume（不再调用 Run）。无需重新读取或编译整图。再次使用原 token 不覆盖结果。重新演示时更换 -run。

框架返回本次驱动触及的节点；示例显式调用 Load 观察持久化节点记录，通过 end 节点判断是否展示输出，没有工作流整体状态。

实际业务实现 `dflow.Infrastructure` 和 outbox 消费即可接入自己的存储与外部服务，契约见 [包说明](../README.md#infrastructure-和恢复)。
