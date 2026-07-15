package common

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
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
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	ctx, stop := contextFromSignals(parent, signals, func(code int) {
		fmt.Fprintln(os.Stderr, "\nForced quit — spawned containers/agents may be left running.")
		os.Exit(code)
	})

	return ctx, func() {
		signal.Stop(signals)
		stop()
	}
}

// contextFromSignals owns the first-signal/second-signal state transition.
// One channel is essential: signal.Notify broadcasts a signal to every
// registered channel, so the old two-channel design queued the first Ctrl+C on
// its supposed "second signal" channel and force-exited as soon as cancellation
// was observed.
func contextFromSignals(parent context.Context, signals <-chan os.Signal, forceExit func(int)) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	stopped := make(chan struct{})
	var stopOnce sync.Once

	go func() {
		select {
		case <-parent.Done():
			return
		case <-stopped:
			return
		case <-signals:
			cancel() // first signal: begin graceful teardown
		}

		select {
		case <-parent.Done():
			return
		case <-stopped:
			return
		case <-signals:
			forceExit(130) // second signal: caller explicitly requested force
		}
	}()

	return ctx, func() {
		stopOnce.Do(func() {
			close(stopped)
			cancel()
		})
	}
}
