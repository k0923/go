package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	reset  = "\033[0m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	blue   = "\033[34m"
	cyan   = "\033[36m"
	bold   = "\033[1m"
)

// Console serializes output from the host, the LLM Action and concurrently
// running tool Actions (Go futures run on their own goroutines).
// Console 串行化宿主、LLM Action 以及并发工具 Action 的输出。
type Console struct {
	mu    sync.Mutex
	w     io.Writer
	delay time.Duration // per-rune streaming delay
}

func (c *Console) Println(color string, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.w, "%s%s%s\n", color, fmt.Sprintf(format, args...), reset)
}

func (c *Console) Print(color string, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.w, "%s%s%s", color, fmt.Sprintf(format, args...), reset)
}

// Stream writes text rune by rune, like an SSE token stream. It honours ctx so
// an Action timeout or Ctrl+C interrupts the stream mid-way.
// Stream 逐字输出，模拟 SSE 流式 token；响应 ctx，超时或 Ctrl+C 会中断输出。
func (c *Console) Stream(ctx context.Context, color, prefix, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprint(c.w, color+prefix)
	defer fmt.Fprint(c.w, reset+"\n")
	for _, r := range text {
		jitter := time.Duration(rand.Int64N(int64(c.delay) + 1))
		select {
		case <-ctx.Done():
			fmt.Fprint(c.w, " …[中断]")
			return ctx.Err()
		case <-time.After(c.delay/2 + jitter):
		}
		fmt.Fprint(c.w, string(r))
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
