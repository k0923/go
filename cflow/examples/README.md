# cflow 示例

需要 Go 1.27.1+，以下命令均在模块根目录运行。

| 场景 | 内容 | 运行命令 |
| --- | --- | --- |
| `action_context_design` | Action 外部依赖、审批和回放 | `go run ./cflow/examples/action_context_design` |
| `wait_any` | 多个信号竞争、取消落败候选 | `go run ./cflow/examples/wait_any` |
| `debugger` | 调试服务中的订单编排 | `go run ./cflow/examples/debugger -run order-1` |
| `react_agent` | ReAct 对话、并行工具、子 Agent、人工审批与回放 | `go run ./cflow/examples/react_agent` |

`debugger` 场景需要先运行 `go run ./cflow/cmd/wfdebug`。

## ReAct Agent

默认使用模拟模型和进程内存储，无需 API Key。天气、搜索、邮件和文件删除均为模拟工具；计算和时间查询在本地执行。

```sh
go run ./cflow/examples/react_agent
go run ./cflow/examples/react_agent -flaky 0.3
go run ./cflow/examples/react_agent -trace -speed 0
```

可输入「北京和上海天气怎么样」「帮我算 (12+8)*3」「调研 Temporal 和 LangGraph」。发送邮件和删除文件的模拟调用会先等待人工审批。会话命令包括 `/history`、`/records` 和 `/bye`。

接入调试服务时，在两个终端分别运行：

```sh
go run ./cflow/cmd/wfdebug
go run ./cflow/examples/react_agent -debugger
```

通过 `-server` 指定调试服务，`-approval-timeout` 设置审批超时，`-session` 指定会话 ID。
