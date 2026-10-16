package fluent

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// RetryPolicy 描述一次调用的重试行为，零值表示不重试。
type RetryPolicy struct {
	// MaxAttempts 是包含首次请求在内的最大尝试次数，<=1 表示不重试。
	MaxAttempts int
	// Backoff 计算第 attempt 次失败后的等待时长（attempt 从 1 开始），为 nil 时使用 ExponentialBackoff(100ms, 5s)。
	Backoff Backoff
	// RetryIf 判断本次结果是否值得重试，为 nil 时使用 DefaultRetryIf。
	// err 是传输层错误；ctx 已取消或超时时不会调用。
	RetryIf func(resp *http.Response, err error) bool
	// MaxRetryAfter 限制服务端 Retry-After 的最大等待时长，为 0 时取 30s；超过上限则放弃重试。
	MaxRetryAfter time.Duration
	// RetryNonIdempotent 允许对 POST/PATCH 重试。默认只重试幂等方法或带 Idempotency-Key 的请求。
	RetryNonIdempotent bool
}

// Backoff 根据已失败的次数返回下一次等待时长。
type Backoff func(attempt int) time.Duration

// ExponentialBackoff 返回带抖动的指数退避：第 n 次等待在 [d/2, d) 之间，d = min(base*2^(n-1), max)。
func ExponentialBackoff(base, max time.Duration) Backoff {
	return func(attempt int) time.Duration {
		d := base << (attempt - 1)
		if d <= 0 || d > max {
			d = max
		}
		half := d / 2
		if half <= 0 {
			return d
		}
		return half + rand.N(half)
	}
}

// ConstantBackoff 每次等待固定时长。
func ConstantBackoff(d time.Duration) Backoff {
	return func(int) time.Duration { return d }
}

var defaultBackoff = ExponentialBackoff(100*time.Millisecond, 5*time.Second)

// DefaultRetryIf 对传输层错误以及 429/502/503/504 重试。
func DefaultRetryIf(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (p RetryPolicy) shouldRetry(resp *http.Response, err error) bool {
	if p.RetryIf != nil {
		return p.RetryIf(resp, err)
	}
	return DefaultRetryIf(resp, err)
}

// delay 返回下一次等待时长；ok=false 表示服务端要求的等待超过上限，不应再重试。
func (p RetryPolicy) delay(attempt int, resp *http.Response) (d time.Duration, ok bool) {
	if wait, found := retryAfter(resp); found {
		limit := p.MaxRetryAfter
		if limit == 0 {
			limit = 30 * time.Second
		}
		return wait, wait <= limit
	}
	backoff := p.Backoff
	if backoff == nil {
		backoff = defaultBackoff
	}
	return backoff(attempt), true
}

// retryAfter 解析 429/503 响应的 Retry-After，支持秒数与 HTTP 日期两种格式。
func retryAfter(resp *http.Response) (time.Duration, bool) {
	if resp == nil || (resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable) {
		return 0, false
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

func isIdempotent(method string, header http.Header) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}
	return header.Get("Idempotency-Key") != ""
}

// sleep 等待 d，ctx 结束时返回其错误。
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type attemptKey struct{}

// AttemptFromContext 返回当前尝试序号（从 1 开始），供中间件记录日志或指标；不在本包调用链中时返回 0。
func AttemptFromContext(ctx context.Context) int {
	n, _ := ctx.Value(attemptKey{}).(int)
	return n
}
