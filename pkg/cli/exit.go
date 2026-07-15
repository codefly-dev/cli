package cli

import "sync"

type Cleanup func()

var (
	cleanupMu sync.Mutex
	cleanups  []Cleanup
)

func RegisterCleanup(cleanup Cleanup) {
	if cleanup == nil {
		return
	}
	cleanupMu.Lock()
	cleanups = append(cleanups, cleanup)
	cleanupMu.Unlock()
}

func Done() {
	cleanupMu.Lock()
	pending := cleanups
	cleanups = nil
	cleanupMu.Unlock()

	for _, cleanup := range pending {
		cleanup()
	}
}
