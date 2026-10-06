package composition

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// ResolveWorkspaceReference materializes a workspace another repository owns and
// versions, so `workspaces:` can name a release instead of a path.
//
// Core delegates this: a reference carrying source + version has no directory
// until a host produces one, and without a resolver registered it fails with
// "requires host resolution". Nothing registered one, so a versioned workspace
// import could not resolve at all and every consumer had to use `path:` --
// which means a checkout per machine, and a composition that renders a
// committed artifact cannot depend on that (obin-ai/platform-obin#104).
//
// A workspace is the same kind of artifact as a module: a repository at an
// immutable tag, with an optional subpath. So it is pulled by exactly the same
// path -- same cache, same tag resolution, same refusal of a moved branch, same
// atomic promotion -- with `workspace:` where a module reference carries
// `module:`. Sharing that machinery is the point: a workspace pulled some other
// way would drift from how every module is pulled.
func ResolveWorkspaceReference(ctx context.Context, ref *resources.WorkspaceReference) (string, error) {
	if ref == nil {
		return "", fmt.Errorf("workspace reference cannot be nil")
	}
	if strings.TrimSpace(ref.Source) == "" || strings.TrimSpace(ref.Version) == "" {
		return "", fmt.Errorf("workspace %q names no source and version to resolve", ref.Name)
	}
	cacheRoot, err := pinnedModuleCacheRoot()
	if err != nil {
		return "", fmt.Errorf("workspace %q: %w", ref.Name, err)
	}
	// The subpath a workspace reference spells `workspace:` is the subpath a
	// module reference spells `module:`; ensurePinnedArtifact checks it is
	// populated and says so by name when it is not.
	dir, _, err := ensurePinnedArtifact(ctx, &resources.ModuleReference{
		Source:  ref.Source,
		Version: ref.Version,
		Module:  ref.Workspace,
	}, cacheRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace %q at %s@%s: %w", ref.Name, ref.Source, ref.Version, err)
	}
	return dir, nil
}

// WithWorkspaceResolver registers ResolveWorkspaceReference on ctx. Every
// command inherits it from the root, so a versioned workspace import resolves
// wherever a workspace is loaded rather than only where someone remembered.
func WithWorkspaceResolver(ctx context.Context) context.Context {
	return resources.WithWorkspaceResolver(ctx, ResolveWorkspaceReference)
}
