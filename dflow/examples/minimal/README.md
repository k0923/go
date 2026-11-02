# 最小接入示例

从模块根目录运行，无需数据库、外部服务或凭证：

```sh
go run ./dflow/examples/minimal
```

输出：

```text
order: succeeded → ready=[notify]
notify: succeeded → ready=[]
订单 order-42 已处理
```

[main.go](main.go) 展示业务定义、从边表生成发布拓扑、Compile、初始化根节点、读取依赖输出和逐节点调度。[store.go](store.go) 演示四个 Infrastructure 方法，以及事务内的租约校验、CAS、结果保存和下游到达合并。

示例存储只在当前进程内保存状态。生产接入需要业务数据库提供相同的原子性和快照隔离，并持久化 Ready，以便恢复提交后丢失的调度消息。
