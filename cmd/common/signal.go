package common

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ForceQuitInterval is how long after the first signal a repeated SIGINT or
// SIGTERM starts to mean "force the exit". Two seconds: long enough that no
// machine delivers one request twice that far apart, short enough that a
// person who watches a stop that does not end reaches it without waiting.
const ForceQuitInterval = 2 * time.Second

// forcedQuitBudget bounds what a forced quit still does before it exits
// (OnForcedQuit): the person asked to leave now.
const forcedQuitBudget = 10 * time.Second

// SignalContext returns a context cancelled on the first SIGINT (Ctrl-C),
// SIGTERM (kill / container shutdown) or SIGHUP (a closed terminal, a parent
// shell that was stopped), and installs a hard-exit handler for a SIGINT or
// SIGTERM repeated at least ForceQuitInterval after the first, so a user
// mashing Ctrl-C during a wedged teardown can always escape.
//
// A signal repeated sooner is the same request delivered again, not a person
// insisting, and is not a force: whatever stops a process group signals every
// member, and a parent in the group (make, a shell) then forwards the signal
// to its child a second time, milliseconds later. That second delivery used to
// force the exit of a stop that had just begun, and every agent, server and
// store of the run was left behind.
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
	ctx, stop := contextFromSignals(parent, signals, ForceQuitInterval, func(code int) {
		fmt.Fprintln(os.Stderr, "\nForced quit — spawned containers/agents may be left running.")
		runForcedQuitHooks()
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

// forcedQuit is what a forced quit still does before the process exits.
var forcedQuit struct {
	mu    sync.Mutex
	next  int
	hooks map[int]func(context.Context)
}

// OnForcedQuit registers what a forced quit must still do before the process
// exits, and returns the function that withdraws it. A forced quit abandons
// the stop that was running; it must not abandon what nothing else will ever
// take back. Every hook shares one short budget (the context it is handed) and
// the exit does not wait past it.
func OnForcedQuit(hook func(context.Context)) func() {
	forcedQuit.mu.Lock()
	defer forcedQuit.mu.Unlock()
	if forcedQuit.hooks == nil {
		forcedQuit.hooks = map[int]func(context.Context){}
	}
	id := forcedQuit.next
	forcedQuit.next++
	forcedQuit.hooks[id] = hook
	return func() {
		forcedQuit.mu.Lock()
		defer forcedQuit.mu.Unlock()
		delete(forcedQuit.hooks, id)
	}
}

func runForcedQuitHooks() {
	forcedQuit.mu.Lock()
	hooks := make([]func(context.Context), 0, len(forcedQuit.hooks))
	for id := 0; id < forcedQuit.next; id++ {
		if hook, ok := forcedQuit.hooks[id]; ok {
			hooks = append(hooks, hook)
		}
	}
	forcedQuit.mu.Unlock()
	if len(hooks) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), forcedQuitBudget)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, hook := range hooks {
			hook(ctx)
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
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

// contextFromSignals owns the first-signal/repeated-signal state transition.
// One channel is essential: signal.Notify broadcasts a signal to every
// registered channel, so the old two-channel design queued the first Ctrl+C on
// its supposed "second signal" channel and force-exited as soon as cancellation
// was observed.
//
// A repeated signal forces the exit only when it arrives at least forceAfter
// after the first one. One that arrives sooner is dropped, and says how to
// insist.
func contextFromSignals(parent context.Context, signals <-chan os.Signal, forceAfter time.Duration, forceExit func(int)) (context.Context, func()) {
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
		first := time.Now()

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
				// The same request delivered again (a signalled process
				// group whose parent forwards the signal) is no force.
				if time.Since(first) < forceAfter {
					fmt.Fprintf(os.Stderr, "already stopping in order; signal again in %s to force the exit\n", (forceAfter - time.Since(first)).Round(100*time.Millisecond))
					continue
				}
				forceExit(130) // a repeated signal after the interval: the caller insists
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
