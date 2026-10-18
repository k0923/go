# dflow

`dflow` 是声明式 DAG 工作流执行包。节点接收解析后的 input，返回 JSON output；引擎根据有向边调度节点，保存节点结果，并从未完成节点恢复执行。

支持拓扑校验、祖先节点输出引用、有界并发、分支跳过与汇合、节点状态持久化、挂起与唤醒、执行租约、版本 CAS、自定义节点，以及默认 JavaScript 节点。框架只管理节点状态，工作流的业务状态由外部判断。

## 快速使用

```go
definition := dflow.Definition{
    Version: "1",
    Nodes: []dflow.NodeSpec{
        {ID: "start", Type: "start"},
        {
            ID: "end", Type: "end",
            Inputs: dflow.Parameters{
                "name": dflow.Ref("start", "name"),
            },
        },
    },
    Edges: []dflow.Edge{
        {SourceNodeID: "start", TargetNodeID: "end"},
    },
}
graph, err := dflow.Compile(definition, dflow.DefaultRegistry())
if err != nil {
    return err
}
engine, err := dflow.NewEngine(graph, memory.New(), dflow.Options{
    MaxConcurrency: 4,
})
if err != nil {
    return err
}
state, err := engine.Run(ctx, map[string]any{"name": "DAG"})
if err != nil {
    return err
}
fmt.Println(state.Nodes["end"].Output)
```

导入 `github.com/k0923/go/dflow` 和 `github.com/k0923/go/dflow/memory`。完整示例见 [examples](examples/README.md)。

## 定义和输入引用

`Definition` 包含 `Version`、`Nodes`、`Edges`。每个 `NodeSpec` 统一定义 `ID`、`Type`、`Inputs` 和节点专用的 `Config`。定义支持标准 `encoding/json` 编解码；`Inputs` 的 JSON 字段名为 `inputsValues`。

`Compile` 拒绝空图、空/重复节点 ID、重复边、不存在的边端点、环、未知节点类型和非法参数引用。图支持多个根节点与末端节点，不要求必须存在 start/end。编译完成的 `Graph` 可以复用；后续修改原始定义或注册表不会改变已编译图。

| 参数 | 用途 |
| --- | --- |
| `Constant(value)` | JSON 常量，可为对象、数组或 null |
| `Input("user", "name")` | 工作流输入中的路径 |
| `Ref("nodeID", "items", "0", "name")` | 祖先节点 output 中的对象键或数组下标 |
| `Ref("nodeID")` | 整个祖先节点 output |
| `Template("{{nodeID.name}}")` | 用 JSON 字面量替换引用，字符串带引号 |
| `PlainTemplate("hello {{nodeID.name}}")` | 用纯文本替换引用，适合 URL、消息 |

引用使用 `{"type":"ref","content":["nodeID","path"]}`，常量使用 `{"type":"constant","content":42}`。`$input` 是工作流输入的保留源名称。模板使用点分路径；包含点号的对象键应使用 `Ref` 的路径分段。

节点只能引用沿有向边可达的祖先节点，允许跨多条边引用。输入引用不会自动创建边。模板引用接受相同校验。路径不存在、数组越界、引用节点被跳过都会返回 `ErrMissingValue`；显式 null 是合法值。

数据在执行与持久化边界按 JSON 规范化，数字通常为 `float64`，大整数应使用字符串。每个节点接收独立输入副本，修改 input 不会污染其他节点或已保存的输入。自定义节点可在 `Run` 中使用业务 schema 校验 input/output。

## 节点和分支

```go
type Node interface {
    Run(context.Context, map[string]any) (dflow.Result, error)
}
```

`Result.Output` 是节点输出；`Result.Ports` 选择被激活的出边 `SourcePortID`：

- `nil`：激活全部出边。
- 非 nil 空切片：不激活任何出边。
- 非空切片：仅激活列出的端口；不存在的端口会返回节点错误。

汇合节点等待全部前驱成功或跳过，然后在至少一条入边被激活时执行；全部入边失活时跳过，并继续传播跳过状态。末端节点不会提前终止其他分支。多个末端结果可从 `State.Nodes` 读取；框架不根据它们推断工作流整体状态。

`DefaultRegistry()` 提供：

| 类型 | 行为 |
| --- | --- |
| `start` | 未配置 Inputs 时透传工作流输入，否则透传解析后的输入 |
| `end` / `passthrough` | 将解析后的输入作为输出 |
| `json-parse` | 将输入字段 `json` 的字符串解析为 JSON 值 |
| `condition` | 根据布尔输入 `condition` 选择 `true`/`false` 端口，输出 `{matchedKey: ...}` |
| `code` | 执行 JavaScript，返回 JSON 值 |

`condition` 被选中的端口需要连接出边。复杂条件可由前置 JS 或业务节点计算。

使用 `NewRegistry()` 创建空注册表，或在默认注册表上调用 `Register(type, factory)` 添加/替换节点。工厂接收 `json.RawMessage` 配置并返回 `Node`；服务客户端等依赖可通过闭包注入。工厂只做配置校验和编译，不执行业务副作用。节点实现应可并发复用，遵守 context 取消，不在返回后继续修改输出。

`NodeFunc` 用于适配普通函数。`NodeID(ctx)` 返回当前节点 ID，业务方可结合自己的 RunID 构造幂等键。可运行的服务注入示例见 [custom_node](examples/custom_node/main.go)。

## JavaScript

`code` 配置示例：

```json
{
  "script": {
    "language": "javascript",
    "content": "async function main({ params }) { await sleep(5); return { total: params.price * 2 }; }"
  },
  "timeoutMs": 10000
}
```

支持 `main({ params })`，也支持 `run(params)`。编译阶段只检查语法，入口是否存在在运行时检查。每次执行创建独立 Goja VM，默认限制 10 秒和 1000 层调用栈；引擎超时、调用方取消和脚本超时中最早发生者生效。`sleep(ms)` 可被取消。

返回已完成的 Promise 可正常读取，拒绝的 Promise 返回错误，pending Promise 返回错误。输出按 `JSON.stringify` 语义转换；undefined 顶层输出与循环对象报错。未提供事件循环、网络、文件系统、模块加载和 TypeScript 转译。Goja 没有进程内硬内存限额；需要严格资源隔离时，应将 JS 执行封装为外部进程或服务节点。

## 节点级持久化与恢复

普通节点在当前进程中连续执行，框架自动保存节点 input、output、状态和挂起信息。业务节点无需调用 checkpoint API。恢复的最小单元是一个节点：已保存成功结果的节点跳过，执行中断且尚未保存结果的节点重试。

需要异步等待的节点实现 `ResumableNode`：

```go
type TranscodeNode struct{}

func (TranscodeNode) Run(ctx context.Context, input map[string]any) (dflow.Result, error) {
    return dflow.Result{Suspend: &dflow.Suspension{
        State:   map[string]any{"format": "mp4"},
        Request: map[string]any{"source": input["url"]},
    }}, nil
}

func (TranscodeNode) Resume(
    ctx context.Context, input map[string]any, resumed dflow.ResumeInput,
) (dflow.Result, error) {
    // input 是已保存的原始节点输入。
    // resumed.State 是 Run 返回的挂起信息，Response 是外部回调结果。
    return dflow.Result{Output: resumed.Response}, nil
}
```

`Result` 只有两种有效结果：通过 `Output` / `Ports` 完成节点，或者通过 `Suspend` 挂起节点。两者不能混用。返回挂起结果的节点必须实现 `Resume`。

框架为每次挂起生成 token，将节点的 suspended 状态、`Suspension.State`、`Suspension.Request` 和 Request 通知**原子保存**，再释放节点执行槽。外部服务通过通知收到请求和 token，在回调时调用 `engine.Wakeup(ctx, nodeID, token, response)`；首次结果被持久化并产生 Wake 通知，重复或过期回调返回 `false`，未知 token 返回 `ErrWaitNotFound`。

随后业务调度器调用 `Engine.Resume(ctx)`。框架使用保存的 input、挂起信息和回调结果调用**节点的 Resume**，不会重新调用该节点的 Run。`Resume` 可以返回成功，也可以返回下一次挂起；每次挂起使用新 token，旧回调无法误唤醒新轮次。`Resume` 执行失败或中断时保留原挂起信息和响应，恢复时仍重试 Resume。

请求通过 `Outbox.Pending` / `Ack` 暴露：

- **Request**：使用通知内的 `Request` 提交外部任务，传递 `NodeID` 与 `Token` 供回调使用，服务接受后 Ack。业务 RunID + NodeID + Token 可用作幂等键。
- **Wake**：调用 Engine.Resume，无错误返回后 Ack；遇到租约冲突或存储错误保留通知并重试。外部业务自行更新整体工作流状态。

Request 和 Wake 分别确认。响应独立于节点快照保存，后续快照写入不能将其覆盖。正在执行的驱动器在后续调度时检查响应；空闲期间由外部消费 Wake 通知恢复。无需逐节点投递 MQ。

跨进程示例见 [suspend](examples/suspend/main.go)。需要拆分同步计算的恢复边界时，拆成多个 DAG 节点即可。

## 调度结果和业务状态

`State` 只包含 `Nodes`、原始 `Input`、`DefinitionHash` 和持久化 `Version`，没有工作流整体 Status。没有可执行节点时引擎返回快照并释放租约。`err == nil` 表示本次驱动正常停止；可能存在 suspended 节点或等待前驱的 pending 节点。

调用方可自行选择业务完成条件，例如检查指定出口：

```go
state, err := engine.Resume(ctx)
if err != nil {
    return err
}
if state.Nodes["end"].Status == dflow.Succeeded {
    // 由业务层处理输出、更新自己的工作流状态。
}
```

框架负责保存节点的 `pending/running/succeeded/skipped/failed/suspended` 状态。首次节点错误停止当前调度并取消已启动节点，退出前等待它们返回；是否重试和如何定义工作流失败由业务决定。再次驱动会重试 failed / 中断的 running 节点，复用 succeeded / skipped 节点，未收到响应的 suspended 节点保持不动。

## Infrastructure

`Infrastructure` 由业务方绑定到单次执行，负责租约、节点快照和回调结果。RunID、TenantID 由实现方管理。`memory.New()` 仅供进程内使用；`sqlite.Open(ctx, path, scope)` 支持进程退出后恢复，同一文件通过 scope 隔离不同执行，使用后关闭存储句柄。

`Save` 必须在同一原子操作中校验当前租约 generation、到期时间和 State.Version，保存节点快照、递增版本，并为新挂起 token 保存唯一 Request 通知。重复 token 不产生新请求。`GetResponse` 读取响应，未到达时返回 nil；JSON null 响应返回非 nil Response。`Resolve` 不获取执行租约，原子保存首个响应和 Wake 通知；Save 不得清除响应。所有返回值均应深拷贝。

租约键为 `dflow/run`；一次执行只有一个驱动器，内部节点有界并发。SQLite 使用事务、WAL 和 synchronous FULL；多机部署应使用业务提供的共享存储。旧版节点内 checkpoint 的 SQLite 文件不兼容此状态模型，Open 会明确拒绝；使用新文件或显式迁移，避免自动误读旧状态。

相同 Infrastructure 只能使用同一份定义和输入，否则返回 `ErrRunMismatch`。修改节点实现或挂起信息结构时更新 `Definition.Version`，为新版本创建新执行。快照全量写入，适合状态规模可控的 DAG；大数据建议保存对象存储引用。

节点默认并发数为 4，单节点每次 Run/Resume 默认超时为 5 分钟，通过 `Options` 调整。业务节点必须响应 context 取消。外部副作用成功但节点结果尚未落盘时仍可能重试，因此 Run/Resume 都需要业务幂等或事务；异步提交优先通过挂起的 Request 通知发出。

`NodeError` 标明节点和失败阶段，`InfrastructureError` 标明存储操作；支持 `errors.Is/As`。持久化失败不会被改写成节点业务失败，也不会允许下游读取尚未确认的结果。

业务方负责初次驱动、故障接管及 outbox 消费。持久化定时器、循环子图和业务专用节点尚未提供；JS 的 sleep 是当前 VM 内的短暂等待。定义使用上述 NodeSpec 格式，不直接解码 flowgram 编辑器的 data、schema、loop blocks 字段。

## 验证

```sh
go test ./dflow/...
go test -race ./dflow/...
go vet ./dflow/...
```
