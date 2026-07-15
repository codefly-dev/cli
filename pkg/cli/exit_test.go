package cli

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestDoneDrainsCleanupsExactlyOnce(t *testing.T) {
	Done()
	t.Cleanup(Done)

	var calls atomic.Int32
	RegisterCleanup(func() { calls.Add(1) })
	Done()
	Done()

	if got := calls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want 1", got)
	}
}

func TestCleanupRegistrySupportsConcurrentRegistration(t *testing.T) {
	Done()
	t.Cleanup(Done)

	const count = 100
	var calls atomic.Int32
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RegisterCleanup(func() { calls.Add(1) })
		}()
	}
	wg.Wait()
	Done()

	if got := calls.Load(); got != count {
		t.Fatalf("cleanup calls = %d, want %d", got, count)
	}
}

func TestRegisterCleanupIgnoresNil(t *testing.T) {
	Done()
	RegisterCleanup(nil)
	Done()
}
