package cflow

// Storage-adapter helpers. Engine code never needs these: they let every
// Infrastructure implementation (memory, debugger, or your own) apply the
// same identity checks, validation and wake IDs as the engine expects.
// Experimental, like FlowInfrastructure and WaitAnyInfrastructure.
// 存储适配辅助函数。编排代码用不到它们：供各 Infrastructure 实现（memory、debugger 或自定义）
// 使用与引擎一致的身份校验、登记校验和唤醒 ID。与 FlowInfrastructure、WaitAnyInfrastructure 一样为实验性。

import (
	"encoding/json"
	"fmt"
)

// ValidateLoopBatch checks a complete initial registration before any write.
// Experimental storage-adapter helper. ValidateLoopBatch 是实验性存储校验辅助函数。
func ValidateLoopBatch(spec LoopSpec, items []FlowExecution) error {
	if spec.Key == "" || spec.Total != len(items) || spec.Depth < 1 || len(spec.Path) == 0 ||
		!localKey.MatchString(spec.FlowName) || spec.Concurrency < 0 ||
		!localKey.MatchString(spec.Workflow) || spec.ItemsHash == "" {
		return ErrInvalid
	}
	seen := map[string]bool{spec.Key: true}
	for i, item := range items {
		if item.Key == "" || seen[item.Key] || item.LoopKey != spec.Key || item.Index != i ||
			item.FlowName != spec.FlowName || item.Workflow != spec.Workflow ||
			item.Depth != spec.Depth || !json.Valid(item.Input) || len(item.Path) != len(spec.Path)+1 {
			return ErrInvalid
		}
		last := item.Path[len(item.Path)-1]
		if last.Name != "item" || last.ID == "" || validID(last.ID) != nil ||
			!item.Path[:len(item.Path)-1].Equal(spec.Path) {
			return ErrInvalid
		}
		seen[item.Key] = true
	}
	return nil
}

// NewLoopState is the initial progress of a registered batch: the first
// Window() items are admitted, an empty batch is already completed.
// NewLoopState 返回登记后的初始进度：前 Window() 个元素已激活，空批次直接完成。
func NewLoopState(spec LoopSpec) *LoopState {
	window := spec.Window()
	loop := &LoopState{LoopSpec: spec, Status: Suspended, Version: 1, AdmittedCount: window, RunningCount: window}
	if spec.Total == 0 {
		loop.Status = Completed
	}
	return loop
}

// ApplyFlowCompletion validates and updates copied snapshots; adapters must
// fence the lease and atomically persist both snapshots plus the wakes. For a
// loop item it returns the index of the next item to admit (-1 for none); the
// loop is terminal once no admitted item is still running. A DiscardOutputs
// loop drops the item's output.
// Experimental. ApplyFlowCompletion 校验并更新副本；适配器必须校验租约并原子提交副本和唤醒。
// 对循环元素返回下一个应激活的下标（无则为 -1）；没有运行中的已激活元素时循环终结。
// DiscardOutputs 循环丢弃元素输出。
func ApplyFlowCompletion(loop *LoopState, current, result *FlowExecution, expected int64) (next int, err error) {
	next = -1
	if current == nil || result == nil || result.Key != current.Key {
		return next, ErrInvalid
	}
	if current.Status != Running || current.Version != expected {
		return next, ErrConflict
	}
	if current.LoopKey != "" && (loop == nil || loop.Status != Suspended || current.Index >= loop.AdmittedCount) {
		return next, ErrConflict
	}
	if (result.Status == Completed && (!json.Valid(result.Output) || result.Failure != nil)) ||
		(result.Status == Failed && result.Failure == nil) ||
		(result.Status != Completed && result.Status != Failed) {
		return next, ErrInvalid
	}
	current.Status, current.Output, current.Failure = result.Status, result.Output, result.Failure
	current.Version++
	if current.LoopKey == "" {
		return next, nil
	}
	if loop.DiscardOutputs {
		current.Output = nil
	}
	loop.Version++
	loop.RunningCount--
	if current.Status == Failed {
		// The first failure wins, except that a framework failure is never
		// hidden behind a business one: it must fail the parent.
		// 首个失败优先，但框架故障不能被业务失败掩盖：它必须令父级失败。
		if loop.Failure == nil || (current.Failure.Fatal && !loop.Failure.Fatal) {
			loop.Failure, loop.FailedKey, loop.FailedIndex = current.Failure, current.Key, current.Index
		}
	} else {
		loop.CompletedCount++
	}
	if loop.Failure == nil && loop.AdmittedCount < loop.Total {
		next = loop.AdmittedCount
		loop.AdmittedCount++
		loop.RunningCount++
	}
	if loop.RunningCount == 0 {
		loop.Status = Completed
		if loop.Failure != nil {
			loop.Status = Failed
		}
	}
	return next, nil
}

// SameFlow compares the immutable definition, input and parent of an execution.
// SameFlow 校验执行实例不可变的定义、输入、路径和父级。
func SameFlow(a, b FlowExecution) bool {
	return a.Key == b.Key && a.Path.Equal(b.Path) && a.ParentKey == b.ParentKey &&
		a.LoopKey == b.LoopKey && a.Index == b.Index && a.Depth == b.Depth &&
		a.Workflow == b.Workflow && a.FlowName == b.FlowName && sameJSON(a.Input, b.Input)
}

// ValidateFlow checks a standalone child before registration.
func ValidateFlow(item FlowExecution) error {
	if item.Key == "" || len(item.Path) == 0 || item.Depth < 1 || item.LoopKey != "" ||
		item.Key == item.ParentKey || !localKey.MatchString(item.FlowName) ||
		!localKey.MatchString(item.Workflow) || !json.Valid(item.Input) {
		return ErrInvalid
	}
	for _, segment := range item.Path {
		if !localKey.MatchString(segment.Name) || validID(segment.ID) != nil {
			return ErrInvalid
		}
	}
	return nil
}

// FlowWakeID separates child activation and loop completion wakes from step wakes.
// FlowWakeID 为元素激活和循环完成生成稳定、与步骤唤醒隔离的投递 ID。
func FlowWakeID(key string, version int64) string {
	return fmt.Sprintf("%s|FLOW_WAKE|%d", key, version)
}

// FlowCompletionWakeID keeps completion deliveries separate from activations.
func FlowCompletionWakeID(key string, version int64) string {
	return fmt.Sprintf("%s|FLOW_PARENT_WAKE|%d", key, version)
}

// SameLoop checks loop identity, including ordered input fingerprint and
// whether outputs are kept. Concurrency is not identity: a changed window is
// accepted and the registered one keeps applying.
// SameLoop 校验循环身份，包括有序输入的指纹及是否保留输出。Concurrency 不属于身份：
// 修改窗口不会报错，已登记的窗口继续生效。
func SameLoop(a, b LoopSpec) bool {
	return a.Key == b.Key && a.Path.Equal(b.Path) && a.ParentKey == b.ParentKey &&
		a.Workflow == b.Workflow && a.FlowName == b.FlowName &&
		a.ItemsHash == b.ItemsHash && a.Total == b.Total && a.Depth == b.Depth &&
		a.DiscardOutputs == b.DiscardOutputs
}

// SameWaitMember reports whether a re-submitted member matches the stored
// one. Storage adapters use it for every member, in order.
// SameWaitMember 判断重新提交的成员是否与已存储成员匹配。存储适配器按顺序对每个成员使用它。
func SameWaitMember(stored, request WaitRequest) bool {
	return request.Key == stored.Key && request.Kind == stored.Kind && request.ExecutionKey == stored.ExecutionKey &&
		(stored.Path == nil || stored.Path.Equal(request.Path))
}
