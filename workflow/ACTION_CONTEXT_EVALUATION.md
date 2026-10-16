# Action 与编排 Context 架构评估

本文记录从函数式 Action 到标准接口的评估与实施，使用 Go 1.27.1 工具链。已落地 Action/ActionFn/AsAction；FlowContext 只提供编排原语，不暴露数据库、第三方客户端或业务扩展 API；外部能力与依赖统一封装在 Action，不为 Context 引入业务依赖泛型。

公开 Action 接口已替换，Run/Go/Loop/Pure 和仓库内示例、测试已迁移；FlowContext 仍保持非泛型。

## 一、Action：建议接口化，保留函数适配

此前 Action 是 `func(context.Context, I) (O, error)`。引擎在 runAction 中做 key/input 校验、租约、回放、调用、结果持久化与事件发布；Action 的函数表示并不参与事件身份。这些机制保持不变，调用已改为 `action.Run(ctx, input)`。

| 维度 | 先前函数类型 | Action 接口 + ActionFn |
| --- | --- | --- |
| 普通函数、闭包、客户端方法接入 | 可直接传入，最省步骤 | 需要显式适配；可用 AsAction 推断类型 |
| 具名业务步骤 | 必须传方法值，或转换为函数 | 业务结构体直接实现 Run |
| 输入输出类型安全 | 已经完整保留泛型类型 | 同样完整；接口化不是类型安全的跃升 |
| 依赖显式性 | 方法接收器可以显式依赖；匿名闭包容易隐藏依赖 | 具名 Action 的字段更易查看、构造、审查 |
| 可测试性 | 普通函数同样可以独立测试 | 具名 Action 便于构造替身和接口断言，ActionFn 支持简易 fake |
| 可维护性 | 短步骤很好；复杂闭包容易混合依赖、控制流与计算 | Run 约定统一，复杂步骤可以独立演进 |
| 纯度和并发安全 | 无编译期保证 | 仍无保证；结构体也可以持有可变状态 |
| 装饰与观测 | 高阶函数可包装 | 结构体装饰器和 ActionFn 都可包装，不是接口独占能力 |
| 用户门槛 | 最低 | 普通用户多一层适配；复杂业务获得更稳定结构 |
| 调用开销 | 函数间接调用 | 接口间接调用；IO/存储主导，不能据此推断有明显性能优势 |

### 闭包的真实痛点

1. **捕获状态不进入回放校验。** 引擎只比较显式 input 的 JSON。折扣、业务配置或变量藏在闭包里时，即使它们变化，key 和 input 也可能没有变化。
2. **捕获 FlowContext。** Action 可以用闭包绕过标准 context 的能力边界，回到编排对象调用等待或子 Flow。签名正确也不能阻止这种捕获。
3. **共享客户端或对象字段。** 捕获可变指针被多个 Go 分支调用，可能产生竞态。方法值同样会绑定接收器，不能把它视为完全没有捕获。
4. **复用与依赖定位。** 长闭包需要读完整个外围作用域才能确定依赖，单独测试时经常要搭建整段 Flow。
5. **命名与错误。** 持久身份仍是挂载 key，不是闭包或结构体名称。接口化不应额外引入与 key 并行的 Name/ID 概念。

当前 Go 1.27.1 已使用较新的循环变量语义，不应把所有 for-range 闭包都判定为经典的“最终循环变量”缺陷。共享外部变量和指针仍需控制。

把闭包改成 `PriceAction{Discount: discount}` 只是让隐藏依赖可见；Discount 仍不属于 input 校验。影响业务结果的可变配置应进入显式 input，或纳入稳定的工作流版本/配置契约。

### 已实施公开接口

```go
type Action[I, O any] interface {
    Run(context.Context, I) (O, error)
}

type ActionFn[I, O any] func(context.Context, I) (O, error)

func (fn ActionFn[I, O]) Run(ctx context.Context, input I) (output O, err error) {
    if fn == nil {
        return output, ErrInvalid
    }
    return fn(ctx, input)
}

func AsAction[I, O any](fn func(context.Context, I) (O, error)) Action[I, O] {
    return ActionFn[I, O](fn)
}
```

ActionFn 是函数类型适配器，不需要所谓“默认实现类”的继承体系。保留标准 context，不重新恢复 ActionContext 的调度能力。函数化曾解决的“与编排 context 解耦”依然成立。

生产复杂步骤推荐：

```go
type LoadOrderAction struct {
    Repository OrderRepository
}

func (a LoadOrderAction) Run(ctx context.Context, id string) (Order, error) {
    return a.Repository.Find(ctx, id)
}

order, err := flowCtx.Run("load_order", LoadOrderAction{Repository: repo}, orderID)
```

普通函数、轻量步骤与测试可以使用 `AsAction(client.Submit)` 或 `ActionFn[Input, Output](fn)`。ActionFn 不能让闭包自动变安全，应对生产代码约束可变捕获，对复杂业务优先具名类型。

### 实施与兼容性

- Run、Go、Loop 的 action 参数改为接口，统一通过 Run 调用；key、input、lease、Record、事件与错误策略保持原机制。
- Pure 返回 ActionFn 适配后的 Action；不因为接口化而把轻量纯计算全部持久化。
- 必须处理 nil 接口、nil ActionFn 和接口内的 typed-nil 指针。默认函数适配器负责 nil 函数检查；引擎应统一校验接口及 typed-nil，不把空依赖错误误报成业务失败。
- 外部依赖经具名 Action 字段/构造函数注入，不存放流程进度，不把客户端对象纳入序列化 input。
- 普通函数不能直接满足含 Run 方法的接口。直接替换公开 Action 是源码破坏性变更，不能宣传为无缝兼容；在新主版本或尚未发布的重构版本实施。
- 原型桥接已移除，示例直接使用正式 workflow.Action/AsAction，外部接入方按迁移说明适配普通函数。
- Go 不允许把含方法的 Action 接口放进与函数类型的联合约束来实现“两个形态自动接受”。不推荐把参数退化为 any 再做运行期判断，这会损害推断与类型安全。
- 纯表示形式变更不要求修改存储 schema。只有业务行为变化时才需要改变工作流版本；不能为了接口迁移随意改 key。

## 二、Context：只提供编排原语，不承载外部能力

Flow 会反复从入口执行。数据库查询、HTTP、消息以及其他第三方操作统一放在 Action 中，即使是只读查询，它的结果也可能改变流程分支和重放行为。

不再建议通过 OrderContext、ExtendedContext[E] 或 context.Value 给编排 context 挂载 Repository/Client。即使一个扩展方法内部暂时包装了 Run，它仍会扩大编排 Context 的业务能力边界，让执行身份、回放边界和依赖位置变得不直观。

FlowContext 保留 Run/Go、Loop、WaitFor/WaitAny、Sleep、Now/UUID 等原语，类型上不存在 RunFlow/GoFlow。独立 WorkflowContext 仅提供 RunFlow/GoFlow，不嵌入 FlowContext，不直接执行 Action 或等待信号。Workflow 通过 NewWorkflow 定义私有顶层入口，只由 Engine.Drive 执行。Flow 负责选择执行点、传递 input/output、判断业务分支和执行确定性纯计算。

依赖注入示例（已实施 Action 接口）：

```go
type OrderFlow struct {
    LoadOrder Action[string, Order]
}

func (flow OrderFlow) Run(ctx *FlowContext, orderID string) (Order, error) {
    return ctx.Run("load_order", flow.LoadOrder, orderID)
}

flow := OrderFlow{
    LoadOrder: LoadOrderAction{Repository: repo},
}
```

Repository 由组装层注入 LoadOrderAction，Flow 只持有可执行步骤接口。影响执行结果的业务状态通过 input 显式传递，不藏在 Flow 的依赖对象中。

| 业务场景 | 执行边界 |
| --- | --- |
| 查询订单，结果决定流程分支 | 查询 Action；Flow 使用已持久化的返回结果判断 |
| DB 写入、提交视频任务、发消息 | Run/Go 执行对应 Action，外部操作采用幂等键 |
| 需要刷新第三方任务状态 | Loop 每个 attempt 执行查询 Action |
| 等待审批、回调或超时 | Flow 组合 WaitFor/WaitAny/Sleep 信号 |
| 字段映射、请求参数拼装 | Flow 中直接执行确定性纯计算 |
| 当前时间、随机 ID | Flow 使用 Now/UUID 检查点，再传给 Action |
| 日志与监控 | 框架观测接口或日志，不用于影响业务分支 |

在这个边界下，Context 没有引入 E 来承载业务依赖的需求。保持非泛型也避免 E 向 Flow/Workflow/子流程传播。Go 泛型不会自动添加扩展方法，且没有默认类型实参；这些语言限制仍成立，但不再是主要决策依据，主要依据是回放与执行职责的分离。

## 三、E 不向 Action 暴露

Action 只需要执行它这一件事所必需的窄依赖。例如 LoadOrderAction 只依赖 OrderRepository，SubmitVideoAction 只依赖 MediaClient。把整套 E 下传会把编排服务容器和 Action 耦合，增加泛型参数、函数适配和单测成本。

Action 的标准 context 继续用于取消、deadline、trace 和稳定 ActionKey。需要客户 ID、业务配置等业务输入时明确放入 input；需要数据库客户端时由 Action 字段/构造函数注入。若确有一族通用 Action 需要泛型策略，可由该业务 Action 本身定义泛型，不迫使核心 Action 全部增加 E。

## 运行验证与实施结果

`examples/action_context_design/main.go` 使用正式接口演示 Action、自动类型推断、具名查询 Action、组装层依赖注入及引擎回放。Flow 只通过步骤接口执行查询，Context 不持有 Repository。

```sh
go run ./workflow/examples/action_context_design
```

实际输出：

```text
COMPLETED order_123 approved
COMPLETED order_123 approved
repository calls: 1
```

原型运行和 vet 通过；它断言两次 Drive 只调用一次 Repository。还用当前 Go 编译器验证了“原生函数不能直接作为 Action 接口”“带方法接口不能与函数组成联合约束”，并用语法检查确认了类型参数默认值不受支持。这些失败探针是预期语言限制，不是仓库编译失败。

实施结果：Action 已改为接口，Run/Go/Loop/Pure 已迁移；FlowContext 保持只含编排原语的非泛型签名。函数通过 AsAction/ActionFn 接入；nil 接口、typed-nil 和 nil 函数在执行前统一拒绝。回归检查覆盖函数/结构体适配、回放、Go 并发、Loop、事件数量和子 Flow 命名空间。
