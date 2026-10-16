// Package lease provides a reusable distributed lease runner.
//
// It contains only the lease lifecycle and fencing contract. It does not know
// about workflows, persistence records, suspend state, signals, transactions,
// tenants, or business state. The package path is:
//
//	github.com/k0923/go/lease
//
// # Types
//
// Token is the receipt for one successful acquisition. It contains the lease
// key, a monotonically increasing generation, and the expiry time returned by
// storage at claim time.
//
// Infrastructure is implemented by storage adapters. It exposes atomic Claim,
// Renew, and Release operations.
//
// Runner claims a lease, keeps it renewed while a callback is running, cancels
// the callback context when ownership is lost, and performs bounded cleanup
// after the callback exits.
//
// Options configures the lease TTL and cleanup timeout.
//
// Error preserves the lease operation name and cause, and supports errors.Is
// and errors.As through Unwrap.
//
// # Usage
//
//	runner, err := lease.NewRunner(storage, lease.Options{
//		TTL:            30 * time.Second,
//		CleanupTimeout: 5 * time.Second,
//	})
//	if err != nil {
//		return err
//	}
//	return runner.Run(ctx, "resource_key", func(ctx context.Context, token lease.Token) error {
//		result, err := execute(ctx)
//		if err != nil {
//			return err
//		}
//		// Commit inside the callback, and make the storage layer validate
//		// token ownership atomically.
//		return storage.CommitProtected(ctx, token, result)
//	})
//
// CommitProtected is an example business storage operation. It is not part of
// Infrastructure. Each caller defines its own lease-protected write method; in
// the workflow package, the equivalent operation is Infrastructure.Commit.
//
// # Lifecycle
//
// A run follows this lifecycle:
//
//  1. Runner calls Infrastructure.Claim to acquire ownership.
//  2. The callback receives a standard context and an immutable Token.
//  3. A background goroutine renews the lease every TTL/3. Renewal covers all
//     callback work, including the final protected commit.
//  4. Renewal failure or ownership conflict cancels the callback context. The
//     callback can inspect context.Cause(ctx) to see the lease failure.
//  5. When the callback exits, Runner stops and waits for the renewal
//     goroutine, then performs bounded Release.
//  6. Callback errors, lease errors, and cleanup errors are joined. Panics are
//     re-thrown after cleanup.
//
// Runner is immutable and safe for concurrent Run calls. Each Run owns an
// independent token, context, and renewal goroutine. Callers must make callback
// work responsive to context cancellation. Run will not leave business work
// detached from the callback, and it cannot safely force-stop code that ignores
// context cancellation.
//
// # Storage Contract
//
// Infrastructure implementations must satisfy these rules:
//
//   - Claim atomically acquires ownership.
//   - Each successful Claim returns a generation greater than any previous
//     generation for that key. Releasing a lease must not reset the generation.
//   - Renew validates both the generation and the store's current expiry. It
//     must not renew an expired lease or a lease acquired by another owner.
//   - Release validates the generation. An old owner must not release a newer
//     owner's lease.
//   - Conflicts return ErrConflict. A nil token from Claim or Renew is also
//     treated as a conflict by Runner.
//   - Renew returns a token with the same key and generation as the acquired
//     token.
//   - All storage methods honor context cancellation and deadlines.
//   - Lease-protected writes atomically validate the generation and the store's
//     current expiry. Context cancellation can stop work early, but it cannot
//     replace fencing.
//
// Token.ExpiresAt is only the acquisition receipt. Renewals do not mutate the
// token passed to the callback, so callers must not use that value to infer the
// latest effective expiry. Storage must validate against its own current expiry
// value.
//
// Release uses a context derived from the original parent context values, with
// cancellation removed and CleanupTimeout applied. Timeout or connection
// failure during cleanup can leave the release result unknown; the storage TTL
// is the final expiration mechanism. Renewal is stopped before cleanup starts.
//
// # Relationship to workflow
//
// workflow.Infrastructure embeds lease.Infrastructure, and workflow.Lease is a
// type alias for lease.Token, so existing workflow storage signatures can still
// use the workflow type name. workflow.ErrConflict reuses lease.ErrConflict.
// Lease errors use *lease.Error, while workflow event read/write errors still
// use *workflow.OperationError.
//
// This package does not depend on workflow. The old workflow/legacy
// implementation keeps its independent lease code and historical API.
//
// # Verification
//
//	go test -race ./lease ./workflow
//	go vet ./lease ./workflow
package lease
