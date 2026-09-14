package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/resources"
)

// ResolvePinnedModulesForRun materializes every composed pinned module of the
// enclosing workspace, so the module and service loads that follow see ordinary
// local checkouts rather than identities core refuses to load. Every run-shaped
// command boundary calls this before resolving what it is about to run.
//
// It returns nothing: the callers here go on to re-find the workspace through
// their own loader (which resolves a module and service too), so what this
// leaves behind for them is the overlay on disk, not an object. Callers that
// need the workspace itself use LoadWorkspaceWithPinnedModules.
func ResolvePinnedModulesForRun(ctx context.Context) error {
	workspace, err := LoadWorkspace(ctx)
	if err != nil {
		return err
	}
	return composition.EnsurePinnedModules(ctx, workspace)
}

// LoadWorkspaceWithPinnedModules is ResolvePinnedModulesForRun for callers that
// use the workspace directly, returning it loaded against the overlay
// materialization produced.
func LoadWorkspaceWithPinnedModules(ctx context.Context) (*resources.Workspace, error) {
	if err := ResolvePinnedModulesForRun(ctx); err != nil {
		return nil, err
	}
	return LoadWorkspace(ctx)
}
