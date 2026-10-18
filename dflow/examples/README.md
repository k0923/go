# dflow 示例

在 Go 模块根目录运行，无需外部服务或凭证。

| 场景 | 命令 | 说明 |
| --- | --- | --- |
| [basic](basic/main.go) | `go run ./dflow/examples/basic` | JSON 定义、上游输出引用、JS 计算，输出 `{"total":42}` |
| [custom_node](custom_node/main.go) | `go run ./dflow/examples/custom_node` | 注册自定义节点、闭包注入服务、工作流输入引用，输出 `你好，DAG！` |
| [http](http/main.go) | `go run ./dflow/examples/http` | 本地 HTTP 服务、查询参数、响应 JSON 解析，输出 `你好，HTTP` |
| [suspend](suspend/main.go) | 见下方命令 | 节点返回挂起结果、SQLite 自动保存、跨进程调用 Resume |
| [subflow](subflow/main.go) | 见下方命令 | SQLite 子流程调用协议、foreach Loop、逐轮持久化 |

basic、custom_node 和 http 示例使用 `memory.New()`。挂起示例使用 SQLite，依次执行：

```sh
go run ./dflow/examples/suspend -db /tmp/dflow-lazy-suspend.db -run demo -publish
go run ./dflow/examples/suspend -db /tmp/dflow-lazy-suspend.db -run demo -token '<第一次输出的 token>' -answer completed
```

第一次 -publish 校验并将定义按节点保存，然后执行 task.Run。第二次进程只使用已保存的发布 ID，按需加载 task 与就绪的 end，执行 task.Resume（不再调用 Run）。无需重新读取或编译整图。再次使用原 token 不覆盖结果。重新演示时更换 -run。

框架返回本次驱动触及的节点；示例显式调用 Load 观察持久化节点记录，通过 end 节点判断是否展示输出，没有工作流整体状态。

实际业务实现 `dflow.Infrastructure` 和 outbox 消费即可接入自己的存储与外部服务，契约见 [包说明](../README.md#infrastructure-和恢复)。

子流程与 Loop 示例：

```sh
go run ./dflow/examples/subflow -db /tmp/dflow-child-demo.db -run demo -publish
go run ./dflow/examples/subflow -db /tmp/dflow-child-demo.db -run demo
```

首次发布父图和循环体，依次驱动三个子执行，输出包含 alpha/beta/gamma 的结果数组。重启后按发布 ID 加载，成功节点不重跑。每轮独立 scope 由 ChildExecution.ScopeKey 生成；示例在前台处理持久化 Request/Wake，生产环境可接入自己的调度器。
