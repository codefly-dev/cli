package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// A module agent's release version is the version of every service it ships.
//
// A module repository is released from its agent.codefly.yaml (kind
// codefly:module), but each of its services reports the version in its own
// service.codefly.yaml: Codefly hands a service that value as
// CODEFLY__SERVICE_VERSION, and it is the service's identity in a run plan, a
// producer id and the `/version` a deployed service answers. Nothing kept the
// two together, so every released module's services said 0.0.0.
//
// The agent manifest stays the one place a module's version is decided. The
// release sets each composed service's `version:` to it in the same release
// commit, and adoptMergedMain refuses a merged main where any of them
// disagrees, so a tag of the module names a tree whose services all report it.

// moduleServiceManifests returns, for a module agent's repository, the
// service.codefly.yaml of every service its workspace composes — resolved
// through the workspace and module declarations, never by searching the tree,
// so a fixture or qualification workspace kept elsewhere in the repository is
// not released. It returns nothing for every other agent kind.
func moduleServiceManifests(ctx context.Context, agentManifest, repoRoot string) ([]string, error) {
	kind, err := agentKind(agentManifest)
	if err != nil || kind != string(resources.ModuleAgent) {
		return nil, err
	}
	dir := filepath.Dir(agentManifest)
	if _, statErr := os.Stat(filepath.Join(dir, resources.WorkspaceConfigurationName)); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("module agent %s has no %s beside it: a module release sets the version of every service its workspace composes, so it needs the workspace that composes them", agentManifest, resources.WorkspaceConfigurationName)
		}
		return nil, statErr
	}
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("load the module's workspace to find the services it releases: %w", err)
	}
	modules, err := workspace.LoadModules(ctx)
	if err != nil {
		return nil, fmt.Errorf("load the module's workspace to find the services it releases: %w", err)
	}
	seen := map[string]bool{}
	var paths []string
	for _, module := range modules {
		for _, ref := range module.ServiceReferences {
			manifest := filepath.Join(module.ServicePath(ctx, ref), resources.ServiceConfigurationName)
			relative, relErr := filepath.Rel(repoRoot, manifest)
			if relErr != nil {
				return nil, fmt.Errorf("service %s/%s: %w", module.Name, ref.Name, relErr)
			}
			// The same containment rule every release manifest is held to: a
			// service whose path leaves the repository is not this release's to
			// rewrite.
			path, pathErr := releaseManifestPath(repoRoot, relative)
			if pathErr != nil {
				return nil, fmt.Errorf("service %s/%s: %w", module.Name, ref.Name, pathErr)
			}
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// agentKind reads only the kind an agent manifest declares. It is lenient on
// purpose: the release gate validates the rest of the manifest, and this only
// decides whether there are service versions to carry.
func agentKind(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the detected release manifest
	if err != nil {
		return "", fmt.Errorf("read agent manifest: %w", err)
	}
	var probe struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return "", fmt.Errorf("parse agent manifest: %w", err)
	}
	return probe.Kind, nil
}

// includeModuleServices adds a module agent's service manifests to the
// release, so they are written, committed, restored on an abort and checked on
// merged main exactly like the agent manifest itself.
func (e *Engine) includeModuleServices(ctx context.Context) error {
	if e.Manifest == nil || e.Manifest.Mode != ModeAgent {
		return nil
	}
	root, err := filepath.Abs(e.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve release root: %w", err)
	}
	paths, err := moduleServiceManifests(ctx, e.Manifest.Path, root)
	if err != nil {
		return err
	}
	present := map[string]bool{e.Manifest.Path: true}
	for _, manifest := range e.AdditionalManifests {
		present[manifest.Path] = true
	}
	for _, path := range paths {
		if present[path] {
			continue
		}
		version, versionErr := readVersion(path)
		if versionErr != nil {
			return fmt.Errorf("module service manifest %s: %w; a module release sets it to the release version, so declare one (version: 0.0.0)", path, versionErr)
		}
		e.AdditionalManifests = append(e.AdditionalManifests, &Manifest{Path: path, Version: version})
		present[path] = true
	}
	return nil
}
