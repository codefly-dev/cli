package composition

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/core/resources"
)

// WithWorkspaceResolution lets Core expand versioned workspace references while
// leaving acquisition in the CLI. Read-only callers use acquire=false: a missing
// release is reported, never fetched as a side effect of diagnosis.
func WithWorkspaceResolution(ctx context.Context, acquire bool) context.Context {
	return resources.WithWorkspaceResolver(ctx, func(ctx context.Context, ref *resources.WorkspaceReference) (string, error) {
		version, err := semver.NewVersion(strings.TrimPrefix(ref.Version, "v"))
		if err != nil || version.String() != strings.TrimPrefix(ref.Version, "v") {
			return "", fmt.Errorf("workspace %q requires an exact semantic version, got %q", ref.Name, ref.Version)
		}
		root, err := pinnedModuleCacheRoot()
		if err != nil {
			return "", err
		}
		// Workspace source releases contain declarations, not signed module
		// packages. Modules still resolve under their declaring owner's policy.
		root = filepath.Join(root, "workspaces")
		tag := "v" + version.String()
		dir := filepath.Join(root, filepath.FromSlash(ref.Source), tag, filepath.FromSlash(ref.Workspace))
		if acquire {
			dir, _, err = ensurePinnedArtifact(ctx, &resources.ModuleReference{
				Name: ref.Name, Source: ref.Source, Version: tag, Module: ref.Workspace,
			}, root)
			if err != nil {
				return "", err
			}
		}
		if _, err := os.Stat(filepath.Join(dir, resources.WorkspaceConfigurationName)); err != nil {
			return "", fmt.Errorf("workspace %s@%s is not materialized with %s; run a materializing command such as codefly deploy gitops render: %w", ref.Source, tag, resources.WorkspaceConfigurationName, err)
		}
		return dir, nil
	})
}

// EffectiveModuleResolutions reads each policy from the workspace that owns the
// module. A product cannot accidentally repin or weaken its core's policy.
func EffectiveModuleResolutions(workspace *resources.Workspace) (map[string]WorkspaceResolution, error) {
	owners := map[string]map[string]WorkspaceResolution{}
	declared, err := LoadModuleResolutions(workspace.Dir())
	if err != nil {
		return nil, err
	}
	owners[workspace.Dir()] = declared
	result := map[string]WorkspaceResolution{}
	for _, ref := range workspace.Modules {
		dir := workspace.ModuleDeclarationDir(ref.Name)
		policy, ok := owners[dir]
		if !ok {
			policy, err = LoadModuleResolutions(dir)
			if err != nil {
				return nil, err
			}
			owners[dir] = policy
		}
		result[ref.Name] = policy[ref.Name]
	}
	return result, nil
}
