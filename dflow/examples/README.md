# dflow 示例

在 Go 模块根目录运行，无需外部服务或凭证。

| 场景 | 命令 | 说明 |
| --- | --- | --- |
| [basic](basic/main.go) | `go run ./dflow/examples/basic` | JSON 定义、上游输出引用、JS 计算，输出 `{"total":42}` |
| [custom_node](custom_node/main.go) | `go run ./dflow/examples/custom_node` | 注册自定义节点、闭包注入服务、工作流输入引用，输出 `你好，DAG！` |
| [suspend](suspend/main.go) | 见下方命令 | 节点返回挂起结果、SQLite 自动保存、跨进程调用 Resume |

前两个示例使用 `memory.New()`。挂起示例使用 SQLite，依次执行：

```sh
go run ./dflow/examples/suspend -db /tmp/dflow-node-suspend.db -run demo
go run ./dflow/examples/suspend -db /tmp/dflow-node-suspend.db -run demo -token '<第一次输出的 token>' -answer completed
```

第一次执行 task.Run，框架保存挂起信息并输出 task 节点状态 `suspended` 和请求 token。第二次进程写入响应，执行 task.Resume（不再调用 Run），继续下游节点并输出结果。再次使用原 token 回调不会覆盖结果或重新执行成功节点。需要重新演示时更换 `-run`。

框架返回节点快照；示例通过 end 节点是否成功判断是否展示输出，没有工作流整体状态。

实际业务实现 `dflow.Infrastructure` 和 outbox 消费即可接入自己的存储与外部服务，契约见 [包说明](../README.md#infrastructure-和恢复)。
