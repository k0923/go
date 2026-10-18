# cflow/debugger

本地调试 workflow 的工具：一个带 Web UI 的调试服务。工作流跑在你自己的进程里，存储放在调试服务上，你可以在浏览器里：

- 看每一步执行到哪、输入输出是什么、耗时多少；
- 手动回答 `WaitFor`（填 JSON 提交）、提前唤醒 `Sleep`、把 WaitAny 的某个候选标记为失败；
- 重启 worker 进程后接着跑，观察回放（replay）是怎么发生的。

> 只用于本地开发调试，不是生产存储。

## 5 分钟上手

**1. 启动调试服务**

```bash
go run ./cflow/cmd/wfdebug
# 默认监听 127.0.0.1:7788，数据写入 ./wfdebug.db
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-addr` | `127.0.0.1:7788` | 监听地址 |
| `-db` | `wfdebug.db` | SQLite 文件路径；`:memory:` 表示不落盘，重启即清空 |

**2. 运行示例 worker**（另开一个终端）

```bash
go run ./cflow/examples/debugger -run order-1
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-run` | 随机 `order-<uuid>` | run ID。复用同一个 ID 即可在重启 worker 后继续执行 |
| `-amount` | `128` | 订单金额；`<= 0` 会触发永久失败，用来观察 FAILED |
| `-server` | `http://127.0.0.1:7788` | 调试服务地址 |

**3. 打开 <http://127.0.0.1:7788>，按下面的顺序推进**

示例是一个订单流程：校验 → 等审批 → 扣款 →（物流轮询 ‖ 通知确认）→ 冷却。

| 步骤 | 页面上看到的 | 你的操作 |
| --- | --- | --- |
| 1 | `prepare|approval` 处于 SUSPENDED | 在“等待中的信号”里填 `{"approved": true, "by": "alice"}`，点“提交并推进”（⌘/Ctrl+Enter）。填 `"approved": false` 会走拒绝分支，run 变成 FAILED |
| 2 | `shipping|ship|attempt_N` 每隔 15 秒轮询一次，第 3 次时状态变为 delivered | 等它跑完，或者对 `shipping|ship|sleep_N` 点“立即唤醒”跳过等待 |
| 3 | `notify|channel|email_ack`、`sms_ack`、`give_up` 三个候选同时等待 | 给其中一个提交任意 JSON，另外两个会变成 CANCELLED；也可以对某个候选点“标记失败”，观察其余候选继续等待 |
| 4 | `finish|cooldown` 休眠 5 秒 | 等待或点“立即唤醒”，run 变为 COMPLETED，worker 打印结果后退出 |

**试试恢复**：任意时刻 Ctrl+C 停掉 worker，再用同一个 `-run order-1` 启动。已完成的步骤会显示为回放（`action_replayed`），流程从上次停下的地方继续。

## 接入自己的工作流

把 `engine.Drive` 换成 `debugger.Run`：

```go
import "github.com/k0923/go/cflow/debugger"

result, err := debugger.Run(ctx, definition, input, debugger.Options{
    RunID: "order-1", // 可选；复用同一 ID 可在进程重启后恢复
})
```

`Run` 会：在服务上注册 run → 把引擎事件推送到时间线 → 循环 Drive。返回时机如下：

| 情况 | 行为 |
| --- | --- |
| COMPLETED / FAILED | 返回结果和 error |
| SUSPENDED | 阻塞等待唤醒：UI 提交、Sleep 到期，或手动 Re-drive |
| 可重试错误 | 按 `RetryDelay` 间隔重新 Drive，页面状态显示为 RETRYING |
| ctx 取消 | 返回 `ctx.Err()`，之后可以用同一 RunID 恢复 |

`Options` 常用字段（零值即可使用）：

| 字段 | 说明 |
| --- | --- |
| `Server` | 服务地址，默认 `http://127.0.0.1:7788` |
| `RunID` | 为空时生成 `<workflow名>-<uuid>` |
| `Engine` | 透传给 `cflow.NewEngine` 的选项（如 `LeaseTTL`、`Now`） |
| `RetryDelay` | 可重试错误后的重驱间隔，默认 2s |
| `Logf` | 进度日志，默认 `log.Printf`；传一个空函数即可静默 |
| `OnAttach` | 注册成功后回调，参数是该 run 的页面 URL，可以用来自动打开浏览器 |

**自己控制 Drive**：只需要存储时，用 `debugger.NewClient(server, runID)`。它实现了 `cflow.Infrastructure`、`cflow.WaitAnyInfrastructure` 和 `memory.Outbox`，可以直接传给 `cflow.NewEngine`。另外还提供了几个调试辅助方法：`Submit(ctx, key, resp)`、`FireSleep(ctx, key)`、`Detail(ctx)`、`URL()`。

## 页面说明

| 区域 | 用途 |
| --- | --- |
| 左侧 run 列表 | 按 workflow 分组，可按状态筛选；显示进度和 worker 是否在线 |
| 步骤 | 每条记录的 Kind / 状态 / 甘特时间线 / 耗时；点击打开详情，可以看到 Input、Result、Error、Type（类型路径）和相关事件 |
| 等待中的信号 | WaitFor：查看请求、编辑 JSON 后提交；Sleep：立即唤醒；WaitAny 成员：另有“标记失败” |
| 事件时间线 | 引擎事件（Drive、Action、回放、挂起、`input_changed`）和存储事件（commit、resolve、cancel、调试操作）；可以隐藏回放事件 |
| 通知 | REQUEST / WAKE / CANCEL 消息及其投递状态 |
| Re-drive | 手动投递一个 WAKE，让 worker 再 Drive 一次 |
| 删除 | 删除该 run 的所有数据 |

页面通过 SSE 实时刷新。样式依赖 Tailwind CDN，首次打开需要能访问 `cdn.tailwindcss.com`。

## 不用 UI：curl / 脚本

```bash
# 回答一个 WaitFor（key 是展开后的完整 key）
curl -X POST localhost:7788/api/runs/order-1/submit \
  -H 'content-type: application/json' \
  -d '{"key":"prepare|approval","response":{"approved":true,"by":"alice"}}'

# 立即唤醒一个 Sleep
curl -X POST localhost:7788/api/runs/order-1/fire -d '{"key":"finish|cooldown"}'

# 查看 run 详情（records、等待组、消息）
curl localhost:7788/api/runs/order-1
```

| Method | Path | 说明 |
| --- | --- | --- |
| GET | `/api/runs` | run 列表 |
| GET / DELETE | `/api/runs/{run}` | 详情 / 删除 |
| GET | `/api/runs/{run}/events?after=&limit=` | 增量拉取事件 |
| POST | `/api/runs/{run}/submit` | `{key, response}`：回答 WaitFor |
| POST | `/api/runs/{run}/fire` | `{key}`：立即唤醒 Sleep |
| POST | `/api/runs/{run}/fail` | `{key, message}`：把 WaitAny 成员标记为失败 |
| POST | `/api/runs/{run}/redrive` | 手动 WAKE |
| GET | `/api/stream` | SSE，推送发生变化的 run ID |

`/api/runs/{run}/infra/*` 和 `/wake` 是给 worker 内部使用的存储 RPC 和长轮询接口，不需要手动调用。

## 需要知道的行为

- **每次 Drive 都会从头回放**，时间线里大量 `action_replayed` 是正常的，表示已完成的步骤直接用了持久化结果。
- **只有第一次提交生效**：重复提交，或者给 WaitAny 里已取消、已失败的成员提交，都会被忽略。
- **“立即唤醒”**会把 Sleep 的截止时间改为当前时间再解析，这是调试专用操作，生产中没有对应能力。
- **时间以服务端时钟为准**（租约有效期、Sleep 到期）。worker 和服务在同一台机器上时不存在时钟偏差。
- **修改了代码再恢复旧 run**：如果改动了步骤的类型或 key，或者定义的版本，恢复时 run 会变为 FAILED（`workflow definition changed`）。这时换一个新的 `-run` ID 即可。只改输入构造方式时，会看到 `input_changed` 告警。
- **删除后 ID 不能复用**：删除会留下一个禁止复用该 ID 的标记，旧 worker 的后续请求会返回 not found 并退出。新的 run 请使用新 ID。
- **可以同时运行多个 worker**：不同 `-run` 是相互隔离的 run；对同一个 run 启动多个 worker 也是安全的，租约保证每个步骤只有一个执行者。
- **事件可能不完整**：事件推送失败会重试，并在内存中最多缓存 4096 条。持续失败或进程退出可能导致时间线缺事件，worker 日志会提示。这只影响观测，不影响执行。

## 开发本工具

```sh
go test -race ./cflow/debugger
node --test cflow/debugger/ui_regression_test.cjs
```
