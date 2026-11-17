package run

import (
	"context"
	"os"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/runners/runstate"
)

// takeOwner reads the process this run was started for from the environment
// (core's runstate.Owner: the SDK declares itself the owner of a dependency
// stack it starts for a test), and whether there is one. The declaration is
// removed from this process's environment: the agents and services the run
// starts are the run's own, and a run they start in turn is not the owner's
// either.
func takeOwner() (runstate.Birth, bool, error) {
	owner, owned, err := runstate.Owner(os.Environ())
	_ = os.Unsetenv(runstate.OwnerEnvironment)
	return owner, owned, err
}

// stopWithOwner returns a context that is cancelled once owner is gone,
// however it went, which stops the run in order exactly as a signal does. The
// owner is told from any later process under its PID by its birth, and is
// never signalled. The returned function ends the watch.
func stopWithOwner(ctx context.Context, owner runstate.Birth) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	gone := owner.Gone(ctx)
	go func() {
		select {
		case <-gone:
			cli.Info("the process this run was started for (%d) is gone: stopping the run", owner.PID)
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
