package common

import (
	"context"
	"fmt"
	"time"
)

// WithHeartbeat runs fn while emitting an elapsed-time heartbeat to stdout every
// 5 seconds until fn returns. Headless commands (run/test/build/deploy/sync)
// have no TUI spinner, so a slow phase — agent readiness waits, a cold docker
// pull, a long build — otherwise looks like a frozen process. The first tick
// points the user at `codefly logs -f` for live detail.
//
//	err := common.WithHeartbeat(ctx, "still starting api", func() error { ... })
//
// The heartbeat goroutine stops as soon as fn returns (or ctx is cancelled).
func WithHeartbeat(ctx context.Context, label string, fn func() error) error {
	done := make(chan struct{})
	go func() {
		start := time.Now()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		first := true
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				secs := int(time.Since(start).Seconds())
				if first {
					fmt.Printf("[codefly] %s… %ds — run `codefly logs -f` in another terminal to watch\n", label, secs)
					first = false
				} else {
					fmt.Printf("[codefly] %s… %ds\n", label, secs)
				}
			}
		}
	}()
	err := fn()
	close(done)
	return err
}
