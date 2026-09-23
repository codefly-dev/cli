package composition

import (
	"context"

	"github.com/codefly-dev/core/resources"
)

// ResolveComposedModuleDir resolves a composed module reference to its local
// directory, materializing a pinned reference exactly as `codefly run` does
// rather than requiring it already be checked out: the workspace's committed
// `module-resolution` declaration, an overlay opt-out, the receipts and the
// module cache all apply, so a command that reads a composed module's tree
// (`test composition`, `generate client`, `sync solution-sdk`, the fixture
// listing) works on a fresh checkout without a prior run.
//
// A reference that already resolves to a directory — a committed `path:`, an
// overlay path or worktree, or a materialization from an earlier command — is
// answered from the workspace as loaded, with nothing pulled or reloaded. Only
// a reference core classifies as pinned is materialized, and the workspace is
// then reloaded from disk because core resolves against the overlay snapshot
// it loaded with, and materialization is what writes the resolved path into
// that overlay. A module EnsurePinnedModules could not pull is left classified
// as pinned (it warns and leaves the entry unwritten), so it is resolved
// through the verified package here, which returns the precise failure —
// missing module-trust, an unpullable release — rather than a bare "not
// loadable as a local checkout".
func ResolveComposedModuleDir(ctx context.Context, workspace *resources.Workspace, ref *resources.ModuleReference) (string, error) {
	resolution, err := workspace.ResolveModule(ctx, ref)
	if err != nil {
		return "", err
	}
	if resolution.Kind != resources.ResolutionPinned {
		return resolution.Dir, nil
	}
	if err = EnsurePinnedModules(ctx, workspace); err != nil {
		return "", err
	}
	reloaded, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	if err != nil {
		return "", err
	}
	if resolution, err = reloaded.ResolveModule(ctx, ref); err != nil {
		return "", err
	}
	if resolution.Kind != resources.ResolutionPinned {
		return resolution.Dir, nil
	}
	pinned, err := ResolvePinnedModule(ctx, workspace.Dir(), ref)
	if err != nil {
		return "", err
	}
	return pinned.Dir, nil
}
