// version_test.go — coverage for update-notice routing.
//
// The background update check finishes at an arbitrary time, including
// mid-render while a TUI owns the terminal. CaptureUpdateNotice lets a TUI
// command divert that output away from stderr so it no longer corrupts the
// inline status bar (#57). These tests exercise the routing/restore logic
// without touching the network.

package cli

import (
	"sync"
	"testing"
)

func TestCaptureUpdateNotice_RoutesToSink(t *testing.T) {
	var got []string
	var mu sync.Mutex
	restore := CaptureUpdateNotice(func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, msg)
	})
	defer restore()

	emitUpdateNotice("A new version of codefly is available. Please update to %s", "v9.9.9")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "A new version of codefly is available. Please update to v9.9.9" {
		t.Fatalf("sink did not receive formatted notice, got %v", got)
	}
}

func TestCaptureUpdateNotice_RestoreStopsSink(t *testing.T) {
	var count int
	restore := CaptureUpdateNotice(func(string) { count++ })
	restore()

	// After restore the sink must not be called again; delivery reverts to the
	// default stderr path.
	emitUpdateNotice("ignored")

	if count != 0 {
		t.Fatalf("sink called after restore: count=%d", count)
	}
}
