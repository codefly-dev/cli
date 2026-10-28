package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// A module agent's release version is the version of everything it ships.
//
// A module repository is released from its agent.codefly.yaml (kind
// codefly:module), and the tag is v<that version>. Two other declarations name
// a version of the same release, and nothing kept them with it:
//
//   - each service's service.codefly.yaml: Codefly hands a service that value
//     as CODEFLY__SERVICE_VERSION, and it is the service's identity in a run
//     plan, a producer id and the `/version` a deployed service answers;
//   - the module package manifest (module.package.codefly.yaml): core resolves
//     a verified package from the GitHub release v<package version>, the API
//     catalog carries it, and every derived operation's identity and digest
//     carry it.
//
// So every released module's services said 0.0.0, and its catalog and
// derived operations named a release that does not exist. The agent manifest
// stays the one place the version is decided. The release commit sets every
// composed service's and the package manifest's `version:` to it, and
// regenerates the catalog and the derived runnables at it, through the
// generators that wrote them; adoptMergedMain refuses a merged main where any
// manifest disagrees with the tag.

// moduleRelease is what a module agent's release carries beside its agent
// manifest.
type moduleRelease struct {
	// manifests are the service and package manifests whose `version:` the
	// release sets, sorted.
	manifests []string
	// generated are the paths the release regenerates once the manifests
	// carry the new version: each packageable module's API catalog and
	// derived runnables tree.
	generated []string
	// regenerate writes them.
	regenerate func(context.Context) error
}

// resolveModuleRelease reads, for a module agent's repository, what its release
// carries. It returns nil for every other agent kind.
//
// The services and modules are found through the repository's committed
// declarations only — workspace.codefly.yaml, each module.codefly.yaml — and
// never by searching the tree, so a fixture or qualification workspace kept
// elsewhere in the repository is not released. The machine-local overlay
// (codefly.local.yaml) is deliberately not applied: a release describes the
// repository, not where one developer checked a module out.
func resolveModuleRelease(ctx context.Context, agentManifest, repoRoot string) (*moduleRelease, error) {
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
	workspace, err := resources.LoadFromDir[resources.Workspace](ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("read the module's workspace to find what it releases: %w", err)
	}
	if workspace.Layout != resources.LayoutKindModules {
		return nil, fmt.Errorf("the module's workspace %s declares layout %q; a module release finds its modules and services through a %q layout", resources.WorkspaceConfigurationName, workspace.Layout, resources.LayoutKindModules)
	}
	layout, err := resources.NewLayout(ctx, dir, workspace.Layout, nil)
	if err != nil {
		return nil, err
	}

	release := &moduleRelease{}
	seen := map[string]bool{}
	contain := func(what, path string) (string, error) {
		relative, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return "", fmt.Errorf("%s: %w", what, relErr)
		}
		// The containment rule every release manifest is held to: what leaves
		// the repository is not this release's to rewrite.
		contained, pathErr := releaseManifestPath(repoRoot, relative)
		if pathErr != nil {
			return "", fmt.Errorf("%s: %w", what, pathErr)
		}
		return contained, nil
	}
	var rekey []*resources.Module
	packages := map[string]generators.ClientReleasePackage{}
	packageIDs := map[*resources.Module]string{}
	for _, ref := range workspace.Modules {
		if ref.Source != "" || ref.Module != "" || ref.Version != "" {
			fmt.Printf("==> module %s is a pinned reference, released from its own source; its services keep their versions\n", ref.Name)
			continue
		}
		moduleDir := layout.ModulePath(ref.Name)
		if ref.PathOverride != nil {
			moduleDir = *ref.PathOverride
			if !filepath.IsAbs(moduleDir) {
				moduleDir = filepath.Join(dir, moduleDir)
			}
		}
		if _, err = contain("module "+ref.Name, filepath.Join(moduleDir, resources.ModuleConfigurationName)); err != nil {
			return nil, err
		}
		module, loadErr := resources.LoadModuleFromDir(ctx, moduleDir)
		if loadErr != nil {
			return nil, fmt.Errorf("read module %s to find what it releases: %w", ref.Name, loadErr)
		}
		for _, service := range module.ServiceReferences {
			path, pathErr := contain(fmt.Sprintf("service %s/%s", module.Name, service.Name),
				filepath.Join(module.ServicePath(ctx, service), resources.ServiceConfigurationName))
			if pathErr != nil {
				return nil, pathErr
			}
			if !seen[path] {
				seen[path] = true
				release.manifests = append(release.manifests, path)
			}
		}
		packageManifest := filepath.Join(module.Dir(), composition.PackageManifestFileName)
		if _, statErr := os.Stat(packageManifest); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return nil, statErr
		}
		path, pathErr := contain("module package "+module.Name, packageManifest)
		if pathErr != nil {
			return nil, pathErr
		}
		release.manifests = append(release.manifests, path)
		generated, genErr := generate.ReleasedContractPaths(module.Dir())
		if genErr != nil {
			return nil, fmt.Errorf("module %s: %w", module.Name, genErr)
		}
		for _, generatedPath := range generated {
			contained, containErr := contain("generated contracts of "+module.Name, generatedPath)
			if containErr != nil {
				return nil, containErr
			}
			release.generated = append(release.generated, contained)
		}
		id, producer, loadErr := loadClientReleasePackage(module.Dir())
		if loadErr != nil {
			return nil, loadErr
		}
		packages[id] = producer
		packageIDs[module] = id
		rekey = append(rekey, module)
	}
	clients, err := planReleasedClients(ctx, repoRoot, packages)
	if err != nil {
		return nil, err
	}
	for _, client := range clients {
		release.generated = append(release.generated, client.Paths...)
	}
	sort.Strings(release.manifests)
	release.regenerate = func(ctx context.Context) error {
		return regenerateModuleClients(ctx, workspace.Name, dir, rekey, packageIDs, clients)
	}
	return release, nil
}

func regenerateModuleClients(ctx context.Context, name, dir string, modules []*resources.Module, packageIDs map[*resources.Module]string, clients []*generators.ClientRelease) error {
	versions := map[string]string{}
	for _, module := range modules {
		if err := generate.RekeyReleasedContracts(ctx, name, dir, module); err != nil {
			return err
		}
		version, err := readVersion(filepath.Join(module.Dir(), composition.PackageManifestFileName))
		if err != nil {
			return err
		}
		versions[packageIDs[module]] = version.String()
	}
	for _, client := range clients {
		if err := client.Write(ctx, versions); err != nil {
			return err
		}
	}
	return nil
}

// A module may not export contracts yet. Read its schema identity without
// requiring an exported artifact tree, and fully verify catalogs that exist.
func loadClientReleasePackage(dir string) (string, generators.ClientReleasePackage, error) {
	producer := generators.ClientReleasePackage{}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", producer, err
	}
	producer.Directory = resolved
	data, err := os.ReadFile(filepath.Join(dir, composition.PackageManifestFileName)) //nolint:gosec // contained module package
	if err != nil {
		return "", producer, err
	}
	var manifest composition.PackageManifest
	if err = yaml.Unmarshal(data, &manifest); err != nil {
		return "", producer, err
	}
	if err = manifest.Validate(); err != nil {
		return "", producer, err
	}
	producer.Catalog, err = composition.LoadAPIContractCatalog(dir)
	if errors.Is(err, os.ErrNotExist) {
		return manifest.ID, producer, nil
	}
	if err != nil {
		return "", producer, err
	}
	if producer.Catalog.Package != manifest.ID || producer.Catalog.Version != manifest.Version {
		return "", producer, fmt.Errorf("module %s catalog disagrees with its package; regenerate contracts before release", dir)
	}
	return manifest.ID, producer, composition.ValidatePackageAPIContracts(dir, &manifest, producer.Catalog)
}

// planReleasedClients discovers only committed library manifests, wherever their
// owners placed them. Untracked caches, foreign sources and hand-written
// libraries have no release-owned source to stamp.
func planReleasedClients(ctx context.Context, repoRoot string, packages map[string]generators.ClientReleasePackage) ([]*generators.ClientRelease, error) {
	if len(packages) == 0 {
		return nil, nil
	}
	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return nil, err
	}
	files, err := (&Engine{WorkDir: root}).git(ctx, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, path := range strings.Split(files, "\x00") {
		tracked[filepath.Join(root, path)] = true
	}
	check := func(path string) error {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		contained, err := releaseManifestPath(root, relative)
		if err != nil {
			return err
		}
		if contained != path {
			return fmt.Errorf("client release path %s follows a symlink", path)
		}
		if !tracked[path] {
			return fmt.Errorf("client release path %s is not git-tracked", path)
		}
		return nil
	}
	var plans []*generators.ClientRelease
	for _, relative := range strings.Split(files, "\x00") {
		if filepath.Base(relative) != resources.LibraryConfigurationName {
			continue
		}
		plan, err := generators.PlanClientRelease(filepath.Dir(filepath.Join(root, relative)), packages, check)
		if err != nil {
			return nil, fmt.Errorf("release client %s: %w", relative, err)
		}
		if plan != nil {
			plans = append(plans, plan)
		}
	}
	return plans, nil
}

// agentKind reads only the kind an agent manifest declares. It is lenient on
// purpose: the release gate validates the rest of the manifest, and this only
// decides whether there is a module release to carry.
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

// includeModuleRelease adds a module agent's service and package manifests to
// the release, so they are written, committed, restored on an abort and
// checked on merged main exactly like the agent manifest itself, and arms the
// regeneration of its generated contracts.
func (e *Engine) includeModuleRelease(ctx context.Context) error {
	if e.Manifest == nil || e.Manifest.Mode != ModeAgent {
		return nil
	}
	root, err := filepath.Abs(e.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve release root: %w", err)
	}
	release, err := resolveModuleRelease(ctx, e.Manifest.Path, root)
	if err != nil || release == nil {
		return err
	}
	present := map[string]bool{e.Manifest.Path: true}
	for _, manifest := range e.AdditionalManifests {
		present[manifest.Path] = true
	}
	for _, path := range release.manifests {
		if present[path] {
			continue
		}
		version, versionErr := readVersion(path)
		if errors.Is(versionErr, errNoVersion) {
			return fmt.Errorf("module release manifest %s: %w; a module release sets it to the release version, so declare one (version: 0.0.0)", path, versionErr)
		}
		if versionErr != nil {
			return fmt.Errorf("module release manifest %s: %w", path, versionErr)
		}
		e.AdditionalManifests = append(e.AdditionalManifests, &Manifest{Path: path, Version: version})
		present[path] = true
	}
	e.generated = release.generated
	e.regenerate = release.regenerate
	return nil
}
