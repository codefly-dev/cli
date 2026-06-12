package common

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// SignalContext returns a context cancelled on the first SIGINT (Ctrl-C) or
// SIGTERM (kill / container shutdown), and installs a hard-exit handler for the
// second signal so a user mashing Ctrl-C during a wedged teardown can always
// escape.
//
// This is the single correct signal pattern for every long-running command.
// It replaces the historical `signal.NotifyContext(ctx, os.Interrupt, os.Kill)`
// idiom, which was a noop bug: SIGKILL (os.Kill) cannot be caught, and SIGTERM
// was not listed at all — so `kill <pid>` fell through without cancelling the
// context and orphaned every spawned agent/plugin (the fork-bomb/zombie class).
func SignalContext(parent context.Context) (context.Context, func()) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)

	// Second-signal force-quit. We arm the handler only AFTER the first signal
	// has cancelled ctx, so the first signal stays graceful (NotifyContext
	// consumes it) and only the next one hard-exits while teardown runs.
	forced := make(chan os.Signal, 1)
	// Register for signals up front so a second Ctrl-C that arrives before the
	// goroutine wakes on ctx.Done() is queued (buffered) rather than discarded
	// by the runtime. NotifyContext independently consumes the first signal to
	// cancel ctx, so the first signal still stays graceful.
	signal.Notify(forced, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ctx.Done():
		case <-parent.Done():
			return
		}
		select {
		case <-forced:
			fmt.Fprintln(os.Stderr, "\nForced quit — spawned containers/agents may be left running.")
			os.Exit(130)
		case <-parent.Done():
		}
	}()

	return ctx, func() {
		signal.Stop(forced)
		stop()
	}
}
