# fsm 示例

从 Go 模块根目录运行，无需外部服务或配置。

| 场景 | 命令 | 内容 |
| --- | --- | --- |
| [订单状态机](order/main.go) | `go run ./fsm/examples/order` | 无状态 `Definition.Fire` + 模拟 CAS 持久化、金额 Guard、多源取消转换、过期状态写回被拒绝 |
| Mermaid 导出 | `go run ./fsm/examples/order -diagram` | 纵向排版，按订单状态设置颜色，保留完整业务描述 |
| [复杂履约流程](fulfillment/main.go) | `go run ./fsm/examples/fulfillment` | 10 个状态、13 条转换，包含审核分支、取消路径、重试回路，使用精简标签 |
| ELK 布局 | `go run ./fsm/examples/fulfillment -elk` | 同一份复杂规则交给 ELK 排版，需要渲染宿主支持 ELK |

订单示例的预期输出：

```text
初始状态: pending
金额校验拒绝，状态: pending
支付成功，状态: paid，流水号: tx-001
取消成功，状态: cancelled，原因: 用户取消
过期状态写回被拒绝，最终状态: cancelled
```

`-diagram` 仅向标准输出写入 Mermaid 源码，可粘贴到支持 Mermaid 的 Markdown 代码块或渲染器中。需要文件时：

```bash
go run ./fsm/examples/order -diagram > /tmp/order-fsm.mmd
```

默认样式使用 `d.Mermaid()`；自定义样式参考订单示例中的 `MermaidOptions[OrderState]`。复杂图可配合方向、间距、换行宽度和 `Compact` 控制文字密度。
