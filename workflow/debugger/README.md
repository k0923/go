# workflow/debugger

本地可调试的 `workflow.Infrastructure`：HTTP Server + SQLite 持久化 + Web UI。
一个 Server 可同时调试多个工作流，每个 Run 的记录、租约、通知、WaitAny 组与事件完全隔离。

## 快速开始

```bash
# 1. 启动调试服务（默认 127.0.0.1:7788，数据写入 wfdebug.db）
go run ./workflow/cmd/wfdebug -addr 127.0.0.1:7788 -db wfdebug.db

# 2. 运行示例 worker（可以开多个，-run 不同即为不同 Run）
go run ./workflow/examples/debugger -run order-1
go run ./workflow/examples/debugger -run order-2 -amount 0   # 触发永久失败

# 3. 打开 http://127.0.0.1:7788
```

## 接入自己的工作流

```go
result, err := debugger.Run(ctx, definition, input, debugger.Options{
	RunID: "order-1", // 复用同一 ID 即可在进程重启后恢复
})
```

`Run` 会：

1. 在 Server 上注册 Run，并把 `Client`（远程 Infrastructure）交给 `workflow.Engine`；
2. 订阅引擎事件并批量推送到 Server 的时间线；
3. 循环 `Drive`：`COMPLETED` / `FAILED` 时返回；可重试错误按 `RetryDelay` 重驱；
   `SUSPENDED` 时长轮询等待 WAKE（UI 提交 JSON、Sleep 到期、手动 Re-drive）。

只想要 Infrastructure、自己控制 Drive，用 `debugger.NewClient(server, runID)`。
它同时实现了 `workflow.Infrastructure`、`memory.Outbox`、`workflow.WaitAnyInfrastructure`。

## UI 能做什么

| 区域 | 内容 |
| --- | --- |
| 左侧 Run 列表 | 按 workflow 分组、状态筛选、进度条、worker 在线状态 |
| 步骤 | 每个 record 的类型 / 状态 / 甘特时间线 / 耗时，点击查看 Input、Result、相关事件 |
| 事件时间线 | 引擎事件（Drive、Action、回放、挂起）与存储事件（commit、resolve、cancel、debugger 操作），可隐藏回放 |
| 通知 | REQUEST / WAKE / CANCEL 消息及其投递状态 |
| 等待中的信号 | WaitFor：查看 Request，编辑 JSON 后「提交并推进」（⌘/Ctrl+Enter）；Sleep：「立即唤醒」；WaitAny 成员：「标记失败」 |
| Re-drive / 删除 | 入队手动 WAKE；删除运行数据并保留禁止复用 ID 的标记 |

UI 通过 SSE 实时刷新。样式使用 Tailwind CDN，首次打开需要能访问 `cdn.tailwindcss.com`。

## 语义说明

- 存储语义与 `memory.Infrastructure` 一致：所有变更在单个 SQLite 事务中串行执行，
  租约按 generation 做 fencing，`Commit` 原子校验租约与版本。
- 时间以 **Server 时钟** 为准（租约有效期、Sleep 到期）。本地调试时 worker 与 Server 同机，不存在时钟偏差。
- UI 提交 JSON 等同于 `Engine.ResolveSignal`：只有第一次提交生效，WaitAny 中已取消/失败的成员忽略后续提交。
- 「立即唤醒」会把 Sleep 的 `Until` 改为当前时间再解析，属于调试专用操作。
- 每次 Drive 都会全量回放，时间线里出现大量 `action_replayed` 是正常的。
- 空 RunID 使用 UUID 创建独立运行；显式 ID 用于恢复尚未删除的运行。
- 删除会持久化 tombstone，旧 worker 后续操作返回 ErrNotFound 并退出；
  相同 RunID 不可重新注册。新运行需使用新 ID，避免旧租约跨运行生命周期生效。
- SQLite 在取得实际写锁后采样服务端时间，避免锁等待期间过期的租约仍通过校验。
- WAKE 在后续 Drive 成功、再次挂起或确认 FAILED 后 ACK；暂时错误保留消息，
  ACK 失败会重试。等待期间定期复核持久状态，使冗余 worker 的旧挂起快照能重新驱动。
- 引擎事件发送失败会重试并保留有界内存批次（最多 4096 条），服务端按事件 ID 去重；
  持续故障、编码失败或队列溢出通过 Logf 提示时间线不完整。进程退出仍可能丢失未送达事件。
- UI 草稿按 Run 隔离；详情只接受最新刷新请求，事件按 ID 去重且游标不回退。
- 服务取消会终止 SSE/长轮询，等待 HTTP 关闭后再关闭数据库。

回归验证（不包含 legacy）：

```sh
go test -race ./workflow ./workflow/debugger ./workflow/cmd/... ./workflow/examples/...
node --test workflow/debugger/ui_regression_test.cjs
```

Node 用例直接执行 UI 函数验证草稿、刷新时序与提交归属；不替代浏览器焦点、渲染和完整交互验证。

## HTTP API

| Method | Path | 说明 |
| --- | --- | --- |
| GET | `/api/runs` | Run 列表 |
| POST | `/api/runs` | 注册 / 恢复 Run `{id, workflow, version, input}` |
| GET / DELETE | `/api/runs/{run}` | 详情（records、groups、messages）/ 删除 |
| GET | `/api/runs/{run}/events?after=&limit=` | 事件增量 |
| POST | `/api/runs/{run}/submit` | `{key, response}` 回答 WaitFor |
| POST | `/api/runs/{run}/fire` | `{key}` 立即唤醒 Sleep |
| POST | `/api/runs/{run}/fail` | `{key, message}` WaitAny 成员失败 |
| POST | `/api/runs/{run}/redrive` | 手动 WAKE |
| GET | `/api/runs/{run}/wake?timeout=20s` | worker 长轮询 WAKE |
| POST | `/api/runs/{run}/infra/{op}` | Infrastructure RPC：get/claim/renew/release/commit/put/resolve/pending/ack/wait_any/fail_wait |
| GET | `/api/stream` | SSE，推送发生变化的 run id |

例如用 curl 推进一个 WaitFor：

```bash
curl -X POST localhost:7788/api/runs/order-1/submit \
  -H 'content-type: application/json' \
  -d '{"key":"approval","response":{"approved":true,"by":"alice"}}'
```
