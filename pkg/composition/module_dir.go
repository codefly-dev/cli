package composition

import (
	"context"

	"github.com/codefly-dev/core/resources"
)

// ResolveComposedModuleDir resolves a composed module reference to its local
// directory, materializing a pinned reference through the verified module
// package (the same path `codefly run` uses) rather than requiring it
// already be checked out.
func ResolveComposedModuleDir(ctx context.Context, workspace *resources.Workspace, ref *resources.ModuleReference) (string, error) {
	resolution, err := workspace.ResolveModule(ctx, ref)
	if err != nil {
		return "", err
	}
	if resolution.Kind == resources.ResolutionPinned {
		pinned, pinnedErr := ResolvePinnedModule(ctx, workspace.Dir(), ref)
		if pinnedErr != nil {
			return "", pinnedErr
		}
		return pinned.Dir, nil
	}
	return resolution.Dir, nil
}
