# Go 动态加载 WASM 最小示例

这个示例使用：

- guest：标准 Go 编译器的 `GOOS=wasip1 GOARCH=wasm`
- host：纯 Go WebAssembly 运行时 [wazero](https://wazero.io/)
- ABI：WASI Preview 1，通过命令行参数传入请求，通过 stdout 返回 JSON

## 运行

```bash
cd wasm
make run
```

预期业务输出：

```json
{"operation":"add","left":20,"right":22,"value":42}
```

也可以分别构建和调用：

```bash
make build
./build/host ./build/guest.wasm multiply 6 7
```

宿主运行过程：

1. 运行时读取 `guest.wasm`，因此模块可以在不重新编译宿主的情况下替换。
2. `CompileModule` 校验并编译 WASM。
3. `InstantiateModule` 创建独立实例并执行 WASI `_start`。
4. 执行结束后关闭实例。
5. 关闭 `CompiledModule` 和 `Runtime`，释放模块及运行时资源。

示例未挂载宿主目录，也未向 guest 提供网络能力。guest 默认只能访问显式传入的参数、stdout 和 stderr。

## 常见方案

| 方案 | Go guest | 调用方式 | 可卸载 | 适用场景 |
| --- | --- | --- | --- | --- |
| 标准 Go + WASI + wazero | 支持 | `_start`，参数/stdin/stdout | 支持 | 任务、规则、插件命令 |
| TinyGo + wazero | 支持 | 导出普通 WASM 函数 | 支持 | 高频细粒度函数调用 |
| wasmtime/Wasmer 子进程 | 支持 WASI | CLI 或进程协议 | 终止进程 | 强进程隔离 |
| Go `plugin` | 原生 `.so`，不是 WASM | Go symbol | 不支持真正卸载 | Linux/macOS 同版本插件 |

标准 Go 生成的 WASM 带有 Go runtime，适合把一次调用建模为一个 WASI 命令。若需要直接调用 `add(i32, i32) -> i32` 这样的导出函数，通常改用 TinyGo、Rust 或 C 编译 guest；宿主仍可沿用 wazero。

这里的“卸载”是关闭实例、编译模块和 runtime，使资源变为可释放状态。Go 堆内存的实际归还时机仍由宿主进程的垃圾回收器决定。对于不可信代码，还应为执行设置超时，并限制内存、文件系统和宿主函数能力。
