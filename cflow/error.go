package cflow

import "fmt"

// ForEachError exposes a persisted terminal failure of a ForEach child.
// Retryable faults and suspension do not terminate the loop.
// Experimental: Cause is reconstructed; arbitrary Go error identity is lost.
// ForEachError 暴露 ForEach 子项已持久化的终态失败；可重试故障和挂起不终结循环。
// 实验性：Cause 从持久化信息重建，不保留任意 Go 错误身份。
type ForEachError struct {
	Key   string // Full item namespace key, e.g. orders|item[123]. 元素命名空间的完整 key。
	Index int    // Zero-based position in the input slice. 输入切片中从 0 开始的位置。
	Cause error  // Reconstructed failure. 从持久化信息重建的错误。
}

func (e *ForEachError) Error() string {
	return fmt.Sprintf("%s (item %d): %v", e.Key, e.Index, e.Cause)
}

func (e *ForEachError) Unwrap() error { return e.Cause }
