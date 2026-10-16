package fluent

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"
)

type Validator func(ctx context.Context, resp *http.Response) error

// controller 判断是否继续重试；需要等待时必须响应 ctx 取消。
type controller func(ctx context.Context) bool

type controllerBuilder func() controller

func RetryByDuration(duration ...time.Duration) controllerBuilder {
	return func() controller {
		count := 0
		return func(ctx context.Context) bool {
			if count >= len(duration) {
				return false
			}
			d := duration[count]
			count++
			return sleep(ctx, d)
		}
	}
}

func RetryByCount(count int) controllerBuilder {
	return func() controller {
		newCount := count
		return func(context.Context) bool {
			if newCount > 0 {
				newCount--
				return true
			}
			return false
		}
	}
}

// RetryByBackoff 最多重试 count 次，第 n 次等待 base*2^n 并加上 [0, 等待时长/2) 的随机抖动，单次等待不超过 max。
func RetryByBackoff(base, max time.Duration, count int) controllerBuilder {
	return func() controller {
		attempt := 0
		return func(ctx context.Context) bool {
			if attempt >= count {
				return false
			}
			d := base << attempt
			if d <= 0 || d > max {
				d = max
			}
			if half := int64(d / 2); half > 0 {
				d += time.Duration(rand.N(half))
			}
			attempt++
			return sleep(ctx, min(d, max))
		}
	}
}

// sleep 等待 d，ctx 取消时立即返回 false 以终止重试。
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func ValidCode(code int) Validator {
	return func(ctx context.Context, resp *http.Response) error {
		if resp.StatusCode != code {
			return fmt.Errorf("resp code is %d and not equal to expected code %d", resp.StatusCode, code)
		}
		return nil
	}
}

func UseStrategy(builder controllerBuilder, validators ...Validator) RetryStrategy {
	// 在外层确定默认值，避免每次构建 handler 时并发写入捕获的 builder。
	if builder == nil {
		builder = RetryByCount(1)
	}
	return func() RetryHandler {
		controller := builder()
		return func(ctx context.Context, resp *http.Response, err error) (error, bool) {
			if err != nil {
				return err, controller(ctx)
			}

			if resp == nil {
				return fmt.Errorf("resp is nil"), controller(ctx)
			}

			for _, validator := range validators {
				if err := validator(ctx, resp); err != nil {
					return err, controller(ctx)
				}
			}

			return nil, false
		}
	}
}
