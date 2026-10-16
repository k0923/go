package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu                                                  sync.Mutex
	tokens                                              map[string]Token
	generation                                          int64
	claimErr, renewErr, releaseErr                      error
	nilClaim, nilRenew, changedGeneration, blockRelease bool
	renewed                                             chan Token
	renewCount, releaseCount                            int
	cleanupErr                                          error
	cleanupValue                                        any
}

func newStore() *memoryStore {
	return &memoryStore{tokens: make(map[string]Token), renewed: make(chan Token, 128)}
}

func (s *memoryStore) Claim(ctx context.Context, key string, ttl time.Duration) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	if s.nilClaim {
		return nil, nil
	}
	if _, exists := s.tokens[key]; exists {
		return nil, ErrConflict
	}
	s.generation++
	token := Token{Key: key, Generation: s.generation, ExpiresAt: time.Now().Add(ttl)}
	s.tokens[key] = token
	return &token, nil
}

func (s *memoryStore) Renew(ctx context.Context, token Token, ttl time.Duration) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.renewErr != nil {
		return nil, s.renewErr
	}
	if s.nilRenew {
		return nil, nil
	}
	current, ok := s.tokens[token.Key]
	if !ok || current.Generation != token.Generation {
		return nil, ErrConflict
	}
	current.ExpiresAt = time.Now().Add(ttl)
	s.tokens[token.Key] = current
	s.renewCount++
	if s.changedGeneration {
		current.Generation++
	}
	select {
	case s.renewed <- current:
	default:
	}
	return &current, nil
}

type valueKey struct{}

func (s *memoryStore) Release(ctx context.Context, token Token) error {
	s.mu.Lock()
	s.releaseCount++
	s.cleanupErr = ctx.Err()
	s.cleanupValue = ctx.Value(valueKey{})
	block := s.blockRelease
	releaseErr := s.releaseErr
	current, ok := s.tokens[token.Key]
	if ok && current.Generation == token.Generation {
		delete(s.tokens, token.Key)
	}
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if releaseErr != nil {
		return releaseErr
	}
	if !ok || current.Generation != token.Generation {
		return ErrConflict
	}
	return nil
}

func runner(t *testing.T, s Infrastructure, options Options) *Runner {
	t.Helper()
	r, err := NewRunner(s, options)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewRunner(t *testing.T) {
	if _, err := NewRunner(nil, Options{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	for _, options := range []Options{{TTL: -1}, {TTL: time.Nanosecond}, {CleanupTimeout: -1}} {
		if _, err := NewRunner(newStore(), options); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	r := runner(t, newStore(), Options{})
	if r.options.TTL != 30*time.Second || r.options.CleanupTimeout != 5*time.Second {
		t.Fatal(r.options)
	}
}

func TestRunnerRun(t *testing.T) {
	t.Run("renewal covers handler and token is stable", func(t *testing.T) {
		s := newStore()
		r := runner(t, s, Options{TTL: 150 * time.Millisecond})
		err := r.Run(context.Background(), "task", func(ctx context.Context, token Token) error {
			for i := 0; i < 3; i++ {
				select {
				case renewed := <-s.renewed:
					if renewed.Generation != token.Generation || !renewed.ExpiresAt.After(token.ExpiresAt) {
						t.Fatalf("%+v %+v", token, renewed)
					}
				case <-time.After(time.Second):
					t.Fatal("renewal did not run")
				}
			}
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.renewCount < 3 || s.releaseCount != 1 || len(s.tokens) != 0 {
			t.Fatalf("renew=%d release=%d tokens=%v", s.renewCount, s.releaseCount, s.tokens)
		}
	})
	t.Run("claim failure and nil claim", func(t *testing.T) {
		cause := errors.New("storage unavailable")
		for _, nilClaim := range []bool{false, true} {
			s := newStore()
			s.nilClaim = nilClaim
			want := cause
			if nilClaim {
				want = ErrConflict
			} else {
				s.claimErr = cause
			}
			err := runner(t, s, Options{}).Run(context.Background(), "task", func(context.Context, Token) error { t.Fatal("handler ran"); return nil })
			var leaseErr *Error
			if !errors.Is(err, want) || !errors.As(err, &leaseErr) || leaseErr.Op != "Claim" {
				t.Fatal(err)
			}
			if s.releaseCount != 0 {
				t.Fatal("released a lease that was not acquired")
			}
		}
	})
	t.Run("conflict preserves winning handler", func(t *testing.T) {
		s := newStore()
		r := runner(t, s, Options{})
		started := make(chan struct{})
		finish := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- r.Run(context.Background(), "same", func(context.Context, Token) error { close(started); <-finish; return nil })
		}()
		<-started
		err := r.Run(context.Background(), "same", func(context.Context, Token) error { t.Error("loser ran"); return nil })
		if !errors.Is(err, ErrConflict) {
			t.Error(err)
		}
		close(finish)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("renewal failure cancels and propagates original cause", func(t *testing.T) {
		cause := errors.New("renew failed")
		for _, mode := range []string{"error", "nil", "generation"} {
			s := newStore()
			want := ErrConflict
			switch mode {
			case "error":
				s.renewErr = cause
				want = cause
			case "nil":
				s.nilRenew = true
			case "generation":
				s.changedGeneration = true
			}
			err := runner(t, s, Options{TTL: 30 * time.Millisecond}).Run(context.Background(), "task", func(ctx context.Context, _ Token) error {
				<-ctx.Done()
				if !errors.Is(context.Cause(ctx), want) {
					t.Errorf("cause lost: %v", context.Cause(ctx))
				}
				return nil // even an ignoring handler cannot hide lease failure
			})
			var leaseErr *Error
			if !errors.Is(err, want) || !errors.As(err, &leaseErr) || leaseErr.Op != "Renew" {
				t.Fatal(err)
			}
			if s.releaseCount != 1 {
				t.Fatal(s.releaseCount)
			}
		}
	})
	t.Run("handler and release errors are joined", func(t *testing.T) {
		s := newStore()
		handlerErr := errors.New("handler failed")
		releaseErr := errors.New("release failed")
		s.releaseErr = releaseErr
		err := runner(t, s, Options{}).Run(context.Background(), "task", func(context.Context, Token) error { return handlerErr })
		if !errors.Is(err, handlerErr) || !errors.Is(err, releaseErr) {
			t.Fatal(err)
		}
	})
	t.Run("parent cancellation keeps cleanup values and releases", func(t *testing.T) {
		s := newStore()
		parent, cancel := context.WithCancel(context.WithValue(context.Background(), valueKey{}, "trace"))
		err := runner(t, s, Options{}).Run(parent, "task", func(ctx context.Context, _ Token) error {
			cancel()
			<-ctx.Done()
			return nil
		})
		if !errors.Is(err, context.Canceled) || s.cleanupErr != nil || s.cleanupValue != "trace" || s.releaseCount != 1 {
			t.Fatalf("%v %+v", err, s)
		}
	})
	t.Run("release has a deadline", func(t *testing.T) {
		s := newStore()
		s.blockRelease = true
		err := runner(t, s, Options{CleanupTimeout: time.Millisecond}).Run(context.Background(), "task", func(context.Context, Token) error { return nil })
		var leaseErr *Error
		if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &leaseErr) || leaseErr.Op != "Release" {
			t.Fatal(err)
		}
	})
	t.Run("panic propagates after cleanup", func(t *testing.T) {
		s := newStore()
		marker := &struct{ msg string }{"panic"}
		func() {
			defer func() {
				if recover() != marker {
					t.Error("panic changed")
				}
			}()
			_ = runner(t, s, Options{}).Run(context.Background(), "task", func(context.Context, Token) error { panic(marker) })
			t.Error("panic swallowed")
		}()
		if s.releaseCount != 1 || len(s.tokens) != 0 {
			t.Fatal("lease not cleaned up")
		}
	})
	t.Run("invalid or already cancelled execution does not claim", func(t *testing.T) {
		s := newStore()
		r := runner(t, s, Options{})
		fn := func(context.Context, Token) error { return nil }
		for _, err := range []error{r.Run(nil, "x", fn), r.Run(context.Background(), "", fn), r.Run(context.Background(), "x", nil)} {
			if !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := r.Run(ctx, "x", fn); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if s.generation != 0 {
			t.Fatal("lease claimed")
		}
	})
	t.Run("runner reused concurrently", func(t *testing.T) {
		s := newStore()
		r := runner(t, s, Options{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, key := range []string{"a", "b"} {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				errs <- r.Run(context.Background(), key, func(_ context.Context, token Token) error {
					if token.Key != key {
						return errors.New("wrong token")
					}
					return nil
				})
			}(key)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if s.releaseCount != 2 {
			t.Fatal(s.releaseCount)
		}
	})
}
