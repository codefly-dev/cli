package composition

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// WithFileLock runs fn while holding an exclusive, cross-process lock on
// lockPath, waiting up to timeout to acquire it. It serializes read-modify-
// write cycles this package and cmd/run perform on small shared state files
// (codefly.local.yaml, the resolved-version index) that have no locking of
// their own, so two concurrent `codefly run` processes cannot silently
// clobber each other's update.
func WithFileLock(lockPath string, timeout time.Duration, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("create lock directory: %w", err)
	}
	lock := flock.New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	if !locked {
		return fmt.Errorf("timed out acquiring lock %s", lockPath)
	}
	defer func() {
		_ = lock.Unlock()
		_ = lock.Close()
	}()
	return fn()
}
