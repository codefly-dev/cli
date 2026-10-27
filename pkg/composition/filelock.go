package composition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// ErrLockTimeout reports that the lock could not be acquired before the timeout
// elapsed — someone else is still inside their critical section. It is a
// distinct error because waiting out a holder is not the same failure as the
// work itself failing: a caller whose work that holder may have just completed
// can re-check its own precondition instead of failing the operation.
var ErrLockTimeout = errors.New("timed out acquiring lock")

// WithFileLock runs fn while holding an exclusive, cross-process lock on
// lockPath, waiting up to timeout to acquire it. It serializes read-modify-
// write cycles this package and cmd/run perform on small shared state files
// (codefly.local.yaml, the resolved-version index) that have no locking of
// their own, so two concurrent `codefly run` processes cannot silently
// clobber each other's update.
func WithFileLock(lockPath string, timeout time.Duration, fn func() error) error {
	return WithFileLockContext(context.Background(), lockPath, timeout, fn)
}

// WithFileLockContext is WithFileLock with caller cancellation. The timeout
// bounds acquisition only; caller cancellation remains distinguishable from it.
func WithFileLockContext(parent context.Context, lockPath string, timeout time.Duration, fn func() error) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("create lock directory: %w", err)
	}
	lock := flock.New(lockPath)
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	defer func() { _ = lock.Close() }()
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) || (err == nil && !locked) {
		return fmt.Errorf("%w %s", ErrLockTimeout, lockPath)
	}
	if err != nil {
		return fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	return fn()
}
