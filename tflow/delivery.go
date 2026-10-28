package tflow

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

type DispatchReceipt struct {
	Claimed   int
	Succeeded int
	Failed    int
}

type RetryPolicy interface {
	Delay(Delivery, error) time.Duration
}

type RetryPolicyFunc func(Delivery, error) time.Duration

func (f RetryPolicyFunc) Delay(d Delivery, err error) time.Duration { return f(d, err) }

// ExponentialRetry uses equal jitter in [delay/2, delay] and saturates at Max.
// Generation counts claims, including claims recovered after process failure.
type ExponentialRetry struct {
	Base time.Duration
	Max  time.Duration
}

func (p ExponentialRetry) Delay(d Delivery, _ error) time.Duration {
	delay := p.Base
	if delay <= 0 {
		delay = time.Second
	}
	cap := max(delay, p.Max)
	for n := int64(1); n < d.Generation && delay < cap; n++ {
		if delay > cap/2 {
			delay = cap
		} else {
			delay *= 2
		}
	}
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)+1))
}

func (e *Engine) retryDelay(d Delivery, cause error) (delay time.Duration, err error) {
	defer func() {
		if value := recover(); value != nil {
			delay = e.options.RetryDelay
			err = fmt.Errorf("retry policy panicked: %v", value)
		}
	}()
	delay = e.options.RetryPolicy.Delay(d, cause)
	if delay < 0 {
		return e.options.RetryDelay, fmt.Errorf("%w: negative retry delay", ErrInvalid)
	}
	return delay, nil
}

// Observer receives best-effort diagnostics outside transactions. Implementations
// must be fast and concurrency-safe. Observer failure cannot veto durable work.
type Observer interface {
	Observe(context.Context, Observation)
}

type ObserverFunc func(context.Context, Observation)

func (f ObserverFunc) Observe(ctx context.Context, event Observation) { f(ctx, event) }

type Observation struct {
	Operation string
	MainID    string
	CommandID string
	Kind      CommandKind
	Duration  time.Duration
	Count     int64
	Error     error
}

func (e *Engine) observe(ctx context.Context, event Observation) {
	if e.options.Observer == nil {
		return
	}
	// Diagnostics are non-vetoing, including a broken observer implementation.
	defer func() { recover() }()
	event.MainID = e.MainID()
	e.options.Observer.Observe(ctx, event)
}
