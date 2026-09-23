package common

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/resources"
)

// ResolvePinnedModules materializes every composed pinned module of the
// enclosing workspace, so the module and service loads that follow see ordinary
// local checkouts rather than identities core refuses to load. Every command
// boundary that loads composed modules to act on them calls this before
// resolving what it is about to act on — `run service`/`job`/`command`, `test
// service`, `deploy gitops render` and its siblings, `deploy module`, `deploy
// service`, and every `ci` verb — because the load is precisely what fails
// without it on a fresh checkout, and a CI render always starts from one.
//
// It is cheap once the workspace is materialized: EnsurePinnedModules compares
// receipts against the request and pulls nothing, taking no lock, so calling it
// at every boundary costs a second command nothing.
//
// It returns nothing: the callers here go on to re-find the workspace through
// their own loader (which resolves a module and service too), so what this
// leaves behind for them is the overlay on disk, not an object. Callers that
// need the workspace itself use LoadWorkspaceWithPinnedModules.
func ResolvePinnedModules(ctx context.Context) error {
	ctx = composition.WithWorkspaceResolution(ctx, true)
	workspace, err := resources.FindWorkspaceUp(ctx)
	if err != nil {
		return err
	}
	if workspace == nil {
		return fmt.Errorf("no workspace found")
	}
	if err = composition.EnsurePinnedModules(ctx, workspace); err != nil {
		return err
	}
	// Reported after materialization, so a service overridden to a version is
	// announced at the directory it was pulled to rather than as an unresolved
	// coordinate. The workspace is reloaded because materialization is what
	// writes those directories into the overlay this one was loaded against.
	if reloaded, err := LoadWorkspace(ctx); err == nil {
		composition.ReportServiceOverrides(ctx, reloaded)
	}
	return nil
}

// LoadWorkspaceWithPinnedModules is ResolvePinnedModules for callers that use
// the workspace directly, returning it loaded against the overlay
// materialization produced.
func LoadWorkspaceWithPinnedModules(ctx context.Context) (*resources.Workspace, error) {
	if err := ResolvePinnedModules(ctx); err != nil {
		return nil, err
	}
	return LoadWorkspace(ctx)
}

// LoadRequiredModuleWithPinnedModulesE is LoadRequiredModuleE behind
// ResolvePinnedModules: the module named (or active) may itself be a composed
// pinned module, which core refuses to load until the CLI has materialized it.
func LoadRequiredModuleWithPinnedModulesE(ctx context.Context, args []string) (*resources.Workspace, *resources.Module, error) {
	if err := ResolvePinnedModules(ctx); err != nil {
		return nil, nil, err
	}
	return LoadRequiredModuleE(ctx, args)
}

// LoadRequiredWithPinnedModulesE is LoadRequiredE behind ResolvePinnedModules,
// for the same reason as LoadRequiredModuleWithPinnedModulesE.
func LoadRequiredWithPinnedModulesE(ctx context.Context, args []string) (*resources.Workspace, *resources.Module, *resources.Service, error) {
	if err := ResolvePinnedModules(ctx); err != nil {
		return nil, nil, nil, err
	}
	return LoadRequiredE(ctx, args)
}
