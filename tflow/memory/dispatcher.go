package memory

import (
	"context"
	"sync"

	"github.com/k0923/go/tflow"
)

// FakeDispatcher records in-memory dispatch attempts. Set Handler before use;
// SetError and Commands are safe to call concurrently with Dispatch.
// Business objects carried by commands must be immutable.
type FakeDispatcher struct {
	mu       sync.Mutex
	commands []tflow.Command
	err      error
	Handler  func(context.Context, tflow.Command) error
}

func (f *FakeDispatcher) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *FakeDispatcher) Dispatch(ctx context.Context, c tflow.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.commands = append(f.commands, c)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if f.Handler != nil {
		return f.Handler(ctx, c)
	}
	return nil
}

func (f *FakeDispatcher) Commands() []tflow.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tflow.Command(nil), f.commands...)
}
