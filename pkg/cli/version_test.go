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

func TestIsNewerVersion(t *testing.T) {
	tests := []struct {
		name    string
		current string
		latest  string
		want    bool
		wantErr bool
	}{
		{name: "new patch", current: "0.1.6", latest: "v0.1.7", want: true},
		{name: "new minor", current: "v0.1.6", latest: "0.2.0", want: true},
		{name: "equal", current: "0.1.6", latest: "v0.1.6"},
		{name: "larger patch in older minor", current: "0.1.6", latest: "v0.0.130"},
		{name: "older", current: "0.1.6", latest: "v0.1.5"},
		{name: "invalid current", current: "development", latest: "v0.1.7", wantErr: true},
		{name: "invalid latest", current: "0.1.6", latest: "latest", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isNewerVersion(tt.current, tt.latest)
			if (err != nil) != tt.wantErr {
				t.Fatalf("isNewerVersion() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("isNewerVersion() = %v, want %v", got, tt.want)
			}
		})
	}
}

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
