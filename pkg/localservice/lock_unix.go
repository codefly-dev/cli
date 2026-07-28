//go:build darwin || linux

package localservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func (m *manager) acquireServiceLock(ctx context.Context, ref ServiceRef) (func(), error) {
	if !labelPattern.MatchString(ref.Label) {
		return nil, fmt.Errorf("service label %q is invalid", ref.Label)
	}
	lockDirectory := filepath.Join(m.home, ".codefly", "services", "locks")
	if err := os.MkdirAll(lockDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create service lock directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDirectory, ref.Label+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open service lock: %w", err)
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				_ = lock.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = lock.Close()
			return nil, fmt.Errorf("lock service %q: %w", ref.Label, err)
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
