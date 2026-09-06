package composition

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWithFileLockSerializesConcurrentCallers is the deterministic regression
// test for the actual defect (no mutual exclusion around the shared-state
// read-modify-write cycles in cmd/run's overlay writes and this package's
// resolved-version index): two concurrent callers racing for the same lock
// path must never execute their critical sections concurrently. The first
// caller holds the lock for a fixed duration and records a start/end
// timestamp pair; if the second caller's critical section starts before the
// first's ends, the lock did not serialize them.
func TestWithFileLockSerializesConcurrentCallers(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	const hold = 200 * time.Millisecond

	type span struct{ start, end time.Time }
	spans := make(chan span, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			if err := WithFileLock(lockPath, 5*time.Second, func() error {
				start := time.Now()
				time.Sleep(hold)
				spans <- span{start: start, end: time.Now()}
				return nil
			}); err != nil {
				t.Errorf("WithFileLock: %v", err)
			}
		}()
	}
	wg.Wait()
	close(spans)

	var recorded []span
	for s := range spans {
		recorded = append(recorded, s)
	}
	if len(recorded) != 2 {
		t.Fatalf("expected 2 critical section runs, got %d", len(recorded))
	}
	first, second := recorded[0], recorded[1]
	if first.start.After(second.start) {
		first, second = second, first
	}
	if second.start.Before(first.end) {
		t.Fatalf("critical sections overlapped: first ran %v-%v, second started at %v", first.start, first.end, second.start)
	}
}
