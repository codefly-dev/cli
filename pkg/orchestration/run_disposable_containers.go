package orchestration

import (
	"context"
	"fmt"
	"slices"

	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/runstate"
)

// removeDisposableRun is core's removal of what a disposable run left on the
// container runtime. A variable so a test can stand in for the Docker engine.
var removeDisposableRun = dockerrun.RemoveDisposableRun

// RemoveContainersOfThisDisposableRun removes what this run left on the
// container runtime when the run is disposable — its containers, running or
// stopped, their anonymous volumes and the data directories core mounted into
// them — and answers what went. Call it once the run's agents have exited.
//
// It is the container half of RemoveNativeStateOfThisRun, on the same fact:
// the scope this invocation generated (IsolateEnvironment), or that the SDK
// session that started it generated (DeclareSessionScope), is never run again,
// so the invocation that declared the state throwaway takes it back. A stopped
// run stops its agents and never destroys them, and an agent keeps its
// container across a stop because the next run reuses it; so a disposable
// run's Redis, object store and Postgres containers, and the Postgres data
// directories, stayed on the machine after every kind of stop. A plain run, or
// a scope the caller named, is a developer's and keeps all of it.
//
// Core decides and removes (dockerrun.RemoveDisposableRun), from the run as
// agents were told it and the scope their containers are labelled with: the
// one this flow resolves from its workspace and the run's naming scope
// (ContainerRecoveryScopeFor). How the run ended is not asked.
func (flow *Flow) RemoveContainersOfThisDisposableRun(ctx context.Context) (dockerrun.DisposableRunRemoval, error) {
	var removal dockerrun.DisposableRunRemoval
	if flow == nil || flow.world == nil || flow.world.Env == nil || flow.world.Workspace == nil {
		return removal, nil
	}
	env := flow.world.Env
	proto, err := env.Proto()
	if err != nil {
		return removal, fmt.Errorf("read this run's environment: %w", err)
	}
	run := runstate.For(flow.world.Workspace.Dir(), proto)
	// An explicit Local or Nix run never touches the Docker control plane.
	if !run.Disposable || !slices.ContainsFunc(flow.SelectableRuntimeContexts(), ReachesDocker) {
		return removal, nil
	}
	scope, err := ContainerRecoveryScopeFor(flow.world.Workspace, env)
	if err != nil {
		return removal, fmt.Errorf("remove this disposable run's containers: %w", err)
	}
	removal, err = removeDisposableRun(ctx, scope, run)
	if err != nil {
		return removal, fmt.Errorf("remove this disposable run's containers: %w", err)
	}
	return removal, nil
}
