package cflow

import (
	"testing"
	"time"
)

// White-box: the backoff formula is private and needs no store.
// 白盒测试：退避公式是私有的，且不需要存储。
func TestRetryPolicyDelay(t *testing.T) {
	policy := RetryPolicy{InitialInterval: time.Second, MaxInterval: 3 * time.Second}
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second} {
		if got := policy.delay(attempt + 1); got != want {
			t.Fatalf("attempt %d: %s", attempt+1, got)
		}
	}
	// A huge multiplier must clamp before the float→Duration conversion.
	// 巨大的倍数必须在 float→Duration 转换前截断。
	huge := RetryPolicy{InitialInterval: time.Second, MaxInterval: time.Minute, Multiplier: 1e19}
	if got := huge.delay(2); got != time.Minute {
		t.Fatalf("overflow: %s", got)
	}
}
