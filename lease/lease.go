package lease

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Token identifies an acquisition. Generation must increase on each successful
// claim, including after release. ExpiresAt is a receipt; storage validates its
// current expiry, not this potentially outdated value after renewal.
type Token struct {
	Key        string
	Generation int64
	ExpiresAt  time.Time
}

var (
	ErrConflict = errors.New("lease conflict")
	ErrInvalid  = errors.New("invalid lease configuration")
)

// Infrastructure implementations must atomically check generation and current
// expiry for renewal, and generation for release. All calls must honor ctx.
// An occupied/expired/stale lease returns ErrConflict (nil token is also treated
// as a conflict by Runner). Claim must allocate a monotonically increasing token.
// Protected writes must validate the token atomically at the storage boundary.
type Infrastructure interface {
	Claim(context.Context, string, time.Duration) (*Token, error)
	Renew(context.Context, Token, time.Duration) (*Token, error)
	Release(context.Context, Token) error
}

type Options struct {
	TTL            time.Duration
	CleanupTimeout time.Duration
}

// Error preserves the operation and original cause for errors.Is/As.
type Error struct {
	Key   string
	Op    string
	Cause error
}

func (e *Error) Error() string { return fmt.Sprintf("lease %s %s: %v", e.Op, e.Key, e.Cause) }
func (e *Error) Unwrap() error { return e.Cause }

// Runner is immutable and safe for concurrent Run calls. Each Run owns a
// separate token, context and renewal goroutine.
type Runner struct {
	api     Infrastructure
	options Options
}

func NewRunner(api Infrastructure, options Options) (*Runner, error) {
	if api == nil {
		return nil, fmt.Errorf("%w: infrastructure required", ErrInvalid)
	}
	if options.TTL == 0 {
		options.TTL = 30 * time.Second
	}
	if options.CleanupTimeout == 0 {
		options.CleanupTimeout = 5 * time.Second
	}
	if options.TTL < 3*time.Nanosecond || options.CleanupTimeout <= 0 {
		return nil, ErrInvalid
	}
	return &Runner{api: api, options: options}, nil
}

// Run claims a lease and renews it throughout handler, including protected
// commits. Renewal failure cancels handler's context and is returned even when
// handler ignores cancellation. Context cancellation is cooperative: handler
// must return; Runner does not detach unfinished work.
//
// All exits, including panic, stop renewal and attempt a bounded release using
// the parent context's values without its cancellation. Handler errors and lease
// errors are joined. Handler panics propagate after cleanup.
func (r *Runner) Run(ctx context.Context, key string, handler func(context.Context, Token) error) (err error) {
	if ctx == nil || key == "" || handler == nil {
		return ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	token, err := r.api.Claim(ctx, key, r.options.TTL)
	if err != nil {
		return &Error{key, "Claim", err}
	}
	if token == nil {
		return &Error{key, "Claim", ErrConflict}
	}
	// Keep the acquisition token immutable. Renewals change only the store's
	// expiry; protected writes identify ownership by key + generation.
	acquired := *token
	runCtx, cancel := context.WithCancelCause(ctx)
	stopped := make(chan struct{})
	failure := make(chan error, 1)
	go r.renew(runCtx, cancel, acquired, stopped, failure)
	defer func() {
		contextErr := runCtx.Err()
		cancel(nil)
		<-stopped
		select {
		case renewErr := <-failure:
			err = errors.Join(err, renewErr)
		default:
			if contextErr != nil {
				err = errors.Join(err, contextErr)
			}
		}
		cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), r.options.CleanupTimeout)
		defer cleanupCancel()
		if releaseErr := r.api.Release(cleanup, acquired); releaseErr != nil {
			err = errors.Join(err, &Error{key, "Release", releaseErr})
		}
	}()
	return handler(runCtx, acquired)
}

func (r *Runner) renew(ctx context.Context, cancel context.CancelCauseFunc, token Token, stopped chan<- struct{}, failure chan<- error) {
	defer close(stopped)
	ticker := time.NewTicker(r.options.TTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewed, err := r.api.Renew(ctx, token, r.options.TTL)
			if ctx.Err() != nil {
				return
			} // normal shutdown of an in-flight renew
			if err == nil && (renewed == nil || renewed.Key != token.Key || renewed.Generation != token.Generation) {
				err = ErrConflict
			}
			if err != nil {
				failureErr := &Error{token.Key, "Renew", err}
				failure <- failureErr
				cancel(failureErr)
				return
			}
		}
	}
}
