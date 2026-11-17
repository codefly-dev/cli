package common

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
)

// SignalContext returns a context cancelled on the first SIGINT (Ctrl-C),
// SIGTERM (kill / container shutdown) or SIGHUP (a closed terminal, a parent
// shell that was stopped), and installs a hard-exit handler for the second
// SIGINT or SIGTERM so a user mashing Ctrl-C during a wedged teardown can
// always escape.
//
// A hangup stops the command in order like the other two. It was ignored by
// `run service` once, so that a stack survived the shell that started it; what
// that left was a stack nothing owned and nothing would ever stop. A run that
// must outlive its terminal is started detached from it.
//
// This is the single correct signal pattern for every long-running command.
// It replaces the historical `signal.NotifyContext(ctx, os.Interrupt, os.Kill)`
// idiom, which was a noop bug: SIGKILL (os.Kill) cannot be caught, and SIGTERM
// was not listed at all — so `kill <pid>` fell through without cancelling the
// context and orphaned every spawned agent/plugin (the fork-bomb/zombie class).
func SignalContext(parent context.Context) (context.Context, func()) {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	handlers.Add(1)
	ctx, stop := contextFromSignals(parent, signals, func(code int) {
		fmt.Fprintln(os.Stderr, "\nForced quit — spawned containers/agents may be left running.")
		os.Exit(code)
	})

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			signal.Stop(signals)
			handlers.Add(-1)
		})
		stop()
	}
}

// handlers counts the SignalContexts listening now.
var handlers atomic.Int32

// SignalHandled reports whether a command is handling SIGINT, SIGTERM and
// SIGHUP itself (a SignalContext is listening), rather than leaving them to the
// default action, which ends the process. Code that catches a signal for a
// moment of its own uses it to decide whether to hand the signal back.
func SignalHandled() bool {
	return handlers.Load() > 0
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

		for {
			select {
			case <-parent.Done():
				return
			case <-stopped:
				return
			case sig := <-signals:
				// A hangup is never a person insisting: a terminal that
				// closes while the teardown runs must not cut it short.
				if sig == syscall.SIGHUP {
					continue
				}
				forceExit(130) // second signal: caller explicitly requested force
				return
			}
		}
	}()

	return ctx, func() {
		stopOnce.Do(func() {
			close(stopped)
			cancel()
		})
	}
}

// OutliveOutput keeps a command alive when whatever read its output is gone,
// and returns the function that restores the default. A write to a closed
// pipe on stdout or stderr ends a Go program with SIGPIPE at once. For a run
// that is the worst moment: the reader that went away is the process the run
// was started for, or the terminal, and the line being written is the first of
// the teardown, so the run died before it had stopped anything and left its
// whole stack behind. With this the write fails and the teardown goes on.
func OutliveOutput() func() {
	signal.Ignore(syscall.SIGPIPE)
	return func() { signal.Reset(syscall.SIGPIPE) }
}
