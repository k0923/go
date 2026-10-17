# Workflow 三层调用规范

系统仅允许以下层级：顶层编排 → Flow → Action。顶层可以顺序或并行调用多个 Flow；Flow 只能组织 Action、信号和确定性计算，不得调用任何其他 Flow，包括自身。

## 类型与权限

| 层级 | 当前类型和入口 | 调用 Action | 调用 Flow | 等待信号 |
| --- | --- | --- | --- | --- |
| 顶层编排 | Engine.Drive 调用 Workflow 私有入口，接收 WorkflowContext | 不提供 | RunFlow/GoFlow | 不提供 |
| Flow | 由顶层 RunFlow/GoFlow 调用 Flow.Run，接收 FlowContext | Run/Go/Loop | 不存在这些方法 | WaitFor/WaitAny/Sleep |
| Action | Action.Run(context.Context, input) | 不可组织其他框架 Action | 严格禁止 | 不可调用工作流等待原语 |

简单工作流也需要定义一个 Flow，并由顶层调用。Action 内可调用 SDK、Repository 和纯函数辅助代码完成一次原子业务操作，但不可重新进入执行引擎或编排其他步骤。“原子”表示框架执行/记录单位，不保证外部系统副作用与 Record 提交跨系统事务。

WorkflowContext 与 FlowContext 是独立类型，没有嵌入关系，不能相互转换。顶层状态通过私有字段引用，业务方不能从 FlowContext 获取或构造有效的 WorkflowContext。Workflow 的执行函数存储在私有字段中；它不实现 Flow，不暴露 Run，不能作为 Flow 传入 RunFlow/GoFlow。

## 编译期约束与运行期校验

1. NewWorkflow 接收 `func(*WorkflowContext, I) (O, error)`；根入口不能使用 FlowContext。
2. FlowContext 不存在 RunFlow/GoFlow，嵌套框架调用无法通过编译，不再需要 topLevel 标记或 ErrFlowCallForbidden。
3. WorkflowContext 不存在 Run/Go/Loop/WaitFor/WaitAny/Sleep/Now/UUID；原子操作与等待必须放进 Flow。
4. Engine.Drive 是 Workflow 的执行入口，校验名称、版本、handler、输入与实例定义；回放入口和错误汇总仍由引擎管理。
5. 顶层可以多次调用同一种 Flow，每个挂载 key 必须唯一。Flow 的本地 key 通过挂载前缀隔离，重复 key、关闭后的执行点仍在运行期拒绝。
6. runChildFlow 只创建 FlowContext；Loop/WaitAny 内部命名空间也不引入顶层能力。

使用框架原语表达的 A → A、A → B → A 都会在第一个 Flow 内跨流程调用处产生编译错误，无需递归深度阈值或遍历调用图。

flowScope 仍然必要：它负责等待 Flow 内未显式 Wait 的异步 Action，并在 RunFlow/Future 返回前汇总其错误。类型隔离解决调用权限，scope 解决完成边界；二者职责不同。

## 编码边界

- 业务代码只能通过顶层 ctx.RunFlow/ctx.GoFlow 进入普通 Flow，禁止直接调用其他 Flow.Run。
- Flow 不持有其他 Flow、Engine、顶层 Context 或可启动工作流的函数依赖；依赖是可执行 Action。
- Action 不持有任何 FlowContext、Engine、Flow、Workflow 或编排回调。标准 context 只用于取消、deadline、trace、稳定 ActionKey。
- ActionFn/FlowFunc 闭包不可捕获顶层 Context 或其他可重入编排对象，避免偷用根授权。
- 不把 FlowContext 存入全局、结构体运行状态或原生 goroutine；不跨驱动复用它。
- 复用流程逻辑时将公共原子操作抽成 Action，或将多个平级 Flow 提升到顶层组合；不要引入递归 Flow。

Go 库不是沙箱：Flow 接口的 Run 方法仍公开，任意用户函数可以直接调用另一个对象的方法，或通过闭包捕获 Engine/WorkflowContext。这样的普通 Go 调用不经过框架入口，类型系统无法普遍拦截。本规范要求在代码审查/静态检查中禁止这些绕过；编译期约束覆盖框架提供的方法和参数类型，静态分析器不属于本次变更。

## 合法并行示例

```go
type SubmitAction struct{ Client Client }

func (a SubmitAction) Run(ctx context.Context, input Input) (Task, error) {
    return a.Client.Submit(ctx, input)
}

type TaskFlow struct {
    Submit workflow.Action[Input, Task]
}

func (f TaskFlow) Run(ctx *workflow.FlowContext, input Input) (Reply, error) {
    task, err := ctx.Run("submit", f.Submit, input)
    if err != nil {
        return Reply{}, err
    }
    return ctx.WaitFor[Reply]("reply", task), nil // 未回调时挂起
}

type Root struct {
    Left, Right workflow.Flow[Input, Reply]
}

func (r Root) Orchestrate(ctx *workflow.WorkflowContext, input Input) ([]Reply, error) {
    // 两个 Flow 先全部启动，再等待本次执行结果。
    left := ctx.GoFlow("left", r.Left, input)
    right := ctx.GoFlow("right", r.Right, input)
    a, err := left.Wait()
    if err != nil {
        return nil, err
    }
    b, err := right.Wait()
    if err != nil {
        return nil, err
    }
    return []Reply{a, b}, nil
}

root := Root{Left: leftFlow, Right: rightFlow}
definition := workflow.NewWorkflow("tasks", "1", root.Orchestrate)
```

上面 Input/Task/Reply/Client 是业务定义。完整可运行验证见 tests/parallel_wait_test.go：12 个独立 Workflow 实例各启动 8 个平级 Flow，混合外部等待与 Sleep，重复回调、分批恢复和回放均使用真实引擎执行。

## 从嵌套方案迁移

这是源码 API 变更：RunFlow/GoFlow 的接收器迁到 WorkflowContext，Workflow.Flow 字段移除。根定义改用 NewWorkflow，根函数参数改为 WorkflowContext；原先位于根的 Action/等待下移到普通 Flow。具名顶层编排可以通过方法值传入 NewWorkflow，它不实现 Flow。

迁移 A → B → Action 为顶层顺序调用 A、B；A 输出作为 B 输入。迁移嵌套并行树为顶层 GoFlow 扇出及汇合。不要仅删除 key 前缀来迁移在途数据：流程结构和 resolved key 可能变化，按版本和实例范围管理部署，新定义不得直接覆盖旧在途实例。

前一版递归并行测试已按本规则迁移为平级并行恢复测试；Flow 内未 Wait 的 Action 仍由 scope 汇合，取消和错误不会造成残留租约。

debugger 示例已拆为 prepare、shipping、notify、finish 四个平级 Flow，使用 v2 定义；审批 key 为 prepare|approval，物流定时器为 shipping|ship|sleep_N，收尾定时器为 finish|cooldown。新示例必须使用新 RunID，旧在途实例需要继续使用原版代码。
