package workflow

import (
	"context"
	"time"

	"github.com/k0923/go/lease"
)

// Infrastructure binds storage to ONE run. RunID/TenantID remain outside the
// kernel; implementations compose resolved keys with their bound scope.
//
// Every returned Record is a deep copy. Every operation checks ctx. Atomic
// methods below must be implemented by a database transaction or equivalent.
// A timeout may mean the commit succeeded; callers must retry idempotently.
type Infrastructure interface {
	lease.Infrastructure
	Get(context.Context, string) (*Record, error)
	// Commit checks CURRENT lease generation and expiry and record version in
	// one atomic write. expected=0 means insert-only. Success increments version.
	Commit(context.Context, Lease, *Record, int64) (*Record, error)
	// Put inserts a checkpoint, definition or waiting record if absent, or
	// returns the existing record. It never overwrites a resolved signal.
	// For Signal/Sleep, registration AND notification insertion are atomic.
	Put(context.Context, *Record) (*Record, error)
	// Resolve checks suspended status, kind and version; commits the response
	// AND a distinct Wake notification atomically. Already resolved is a no-op
	// returning false. A stale version returns ErrConflict.
	Resolve(context.Context, string, int64, []byte) (bool, error)
}

// Delivery is optional: production deployments can expose their own queue API.
// A consumer must ACK only after durable handoff. On ErrConflict or transient
// drive errors it retries the Wake message. Duplicate delivery is expected.
type Delivery interface {
	Pending(context.Context, time.Time) ([]Notification, error)
	Ack(context.Context, string) error
}
