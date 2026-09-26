package gitops

// Dev deployment: the deliberate escape hatch for fast iteration on a hosted
// environment. It pushes ONE service's current code — typically uncommitted or
// unreleased — into an environment a full `deploy gitops render` has already
// produced, by building and pushing that service exactly as the render does and
// then re-pinning only that service's image digest in the rendered tree.
//
// It is loud by construction: every dev deployment is recorded in the render
// inventory (.codefly-render.json) under `dev`, `codefly doctor workspace`
// warns while one is active, and the next full render re-derives the tree,
// drops the record and says so.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// DevSourceOrigin names where a dev deployment's source came from.
const (
	DevSourceFlag     = "path"
	DevSourceOverride = "override"
)

// InventoryDevDeployment records one dev deployment on top of a full render.
// Its presence means the environment runs code no release describes.
type InventoryDevDeployment struct {
	Service    string    `json:"service"`
	Origin     string    `json:"origin"`
	Source     string    `json:"source"`
	Commit     string    `json:"commit,omitempty"`
	Dirty      bool      `json:"dirty"`
	Image      string    `json:"image"`
	Digest     string    `json:"digest"`
	DeployedAt time.Time `json:"deployedAt"`
}

// DevSource is the resolved source directory of a dev deployment.
type DevSource struct {
	Dir    string
	Origin string
}

// ResolveDevSource picks the source a dev deployment ships: an explicit path
// wins, then the machine-local service override; with neither there is nothing
// to deploy — a dev deployment exists to ship code the committed configuration
// does not already describe.
func ResolveDevSource(module, service, path string, override *resources.ServiceResolution) (DevSource, error) {
	if strings.TrimSpace(path) != "" {
		dir, err := filepath.Abs(path)
		if err != nil {
			return DevSource{}, fmt.Errorf("resolve --path: %w", err)
		}
		info, err := os.Stat(dir)
		if err != nil {
			return DevSource{}, fmt.Errorf("--path %s: %w", path, err)
		}
		if !info.IsDir() {
			return DevSource{}, fmt.Errorf("--path %s is not a directory", path)
		}
		return DevSource{Dir: dir, Origin: DevSourceFlag}, nil
	}
	if override != nil {
		if override.Kind == resources.ResolutionPinned || override.Dir == "" {
			return DevSource{}, fmt.Errorf(
				"service %s/%s is overridden to module package version %q, which is a release, not local code: give --path <dir>",
				module, service, override.Version)
		}
		return DevSource{Dir: override.Dir, Origin: DevSourceOverride}, nil
	}
	return DevSource{}, fmt.Errorf(
		"nothing to deploy for %s/%s: give --path <dir> or set an override with `codefly override service %s/%s`",
		module, service, module, service)
}

// DevRequest is one dev deployment of a single service.
type DevRequest struct {
	Workspace   *resources.Workspace
	Module      *resources.Module
	Service     string
	Environment *environments.Environment
	// AppProject, when set, must be the AppProject the tree was rendered for.
	AppProject string
	Source     DevSource
	Sink       orchestration.OutputSink
	Now        func() time.Time
}

// DevResult reports what a dev deployment changed.
type DevResult struct {
	// Root is the rendered module tree that was patched.
	Root string
	// Changed are the workspace-relative paths the deployment rewrote,
	// including the render inventory.
	Changed []string
	Entry   InventoryDevDeployment
}

// buildServiceImage is the build/push boundary of a dev deployment. It is a
// variable so tests can stand in for the agent and the registry.
var buildServiceImage = buildRenderedServiceImages

// buildRenderedServiceImages builds and pushes one service through the exact
// path a module render takes for it — the same registry preparation, the same
// snapshot flow driving the service agent's Build and Deploy, the same digest
// capture — rendering stand-alone into a scratch tree, and returns the
// digest-pinned image references that render wrote. Nothing in the scratch
// tree is kept but those references.
func buildRenderedServiceImages(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	service *resources.Service,
	env *environments.Environment,
	sink orchestration.OutputSink,
) ([]string, error) {
	if err := environments.ValidateWorkspace(ctx, workspace); err != nil {
		return nil, err
	}
	if err := prepareSnapshotRegistry(ctx, env); err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp("", "codefly-dev-")
	if err != nil {
		return nil, fmt.Errorf("create dev render scratch: %w", err)
	}
	defer os.RemoveAll(scratch)
	destinations := serviceRenderDestinations(scratch)
	if err := renderServiceFlow(ctx, workspace, module, service, env, true, false, sink, destinations, nil, nil, nil, nil); err != nil {
		return nil, fmt.Errorf("build service %s: %w", service.Name, err)
	}
	return digestImages(destinations(module, service))
}

// imageLinePattern matches a YAML `image:` value pinned by digest.
var imageLinePattern = regexp.MustCompile(`(?m)^\s*(?:-\s*)?image:\s*["']?([^\s"']+@sha256:[a-fA-F0-9]{64})["']?\s*$`)

// digestImages collects the distinct digest-pinned image references under root.
func digestImages(root string) ([]string, error) {
	seen := map[string]bool{}
	err := walkRegularFiles(root, func(path, _ string, _ os.FileInfo) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range imageLinePattern.FindAllSubmatch(data, -1) {
			seen[string(match[1])] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	images := make([]string, 0, len(seen))
	for image := range seen {
		images = append(images, image)
	}
	sort.Strings(images)
	if len(images) == 0 {
		return nil, fmt.Errorf("the service render pinned no image by digest")
	}
	return images, nil
}

// DeployDev builds one service from its dev source and re-pins its image in the
// already-rendered tree of the requested environment.
func DeployDev(ctx context.Context, request *DevRequest) (DevResult, error) {
	root := moduleRenderDestination(request.Workspace, request.Module.Name)
	inventory, unit, err := loadDevTarget(root, request.Module.Name, request.Service, request.Environment.Name, request.AppProject)
	if err != nil {
		return DevResult{}, err
	}
	service, err := loadDevService(ctx, request.Module, request.Service, request.Source)
	if err != nil {
		return DevResult{}, err
	}
	images, err := buildServiceImage(ctx, request.Workspace, request.Module, service, request.Environment, request.Sink)
	if err != nil {
		return DevResult{}, err
	}
	commit, dirty := sourceRevision(ctx, request.Source.Dir)
	now := time.Now
	if request.Now != nil {
		now = request.Now
	}
	entry := InventoryDevDeployment{
		Service: request.Service, Origin: request.Source.Origin, Source: request.Source.Dir,
		Commit: commit, Dirty: dirty, DeployedAt: now().UTC().Truncate(time.Second),
	}
	changed, err := applyDevImages(root, &inventory, &unit, images, &entry)
	if err != nil {
		return DevResult{}, err
	}
	relative := make([]string, 0, len(changed))
	for _, path := range changed {
		rel, relErr := filepath.Rel(request.Workspace.Dir(), filepath.Join(root, path))
		if relErr != nil {
			return DevResult{}, relErr
		}
		relative = append(relative, filepath.ToSlash(rel))
	}
	entry = inventory.Dev[devEntryIndex(inventory.Dev, request.Service)]
	return DevResult{Root: root, Changed: relative, Entry: entry}, nil
}

// moduleRenderDestination is where a full module render writes its owned tree.
func moduleRenderDestination(workspace *resources.Workspace, module string) string {
	return filepath.Join(workspace.Dir(), "deployments", "modules", module)
}

// loadDevTarget refuses a dev deployment onto anything but a complete, intact
// render of this module for this environment that renders the service itself.
func loadDevTarget(root, module, service, environment, appProject string) (Inventory, InventoryUnit, error) {
	renderFirst := fmt.Sprintf("run `codefly deploy gitops render %s --env %s` first", module, environment)
	inventory, err := LoadInventory(root)
	if errors.Is(err, fs.ErrNotExist) {
		return Inventory{}, InventoryUnit{}, fmt.Errorf("module %s has never been rendered (no %s): %s", module, filepath.Join(root, InventoryFilename), renderFirst)
	}
	if err != nil {
		return Inventory{}, InventoryUnit{}, err
	}
	if inventory.Module != module || inventory.Unit != "" {
		return Inventory{}, InventoryUnit{}, fmt.Errorf("%s is not a full render of module %s: %s", root, module, renderFirst)
	}
	if inventory.Environment != environment {
		return Inventory{}, InventoryUnit{}, fmt.Errorf("module %s is rendered for environment %s, not %s: %s", module, inventory.Environment, environment, renderFirst)
	}
	if appProject != "" && inventory.AppProject != appProject {
		return Inventory{}, InventoryUnit{}, fmt.Errorf("module %s is rendered for AppProject %q, not %q: %s", module, inventory.AppProject, appProject, renderFirst)
	}
	actual, err := buildInventory(root, inventoryRenderOptions(&inventory))
	if err != nil {
		return Inventory{}, InventoryUnit{}, err
	}
	if err := validateInventory(&inventory, &actual, "rendered tree"); err != nil {
		return Inventory{}, InventoryUnit{}, fmt.Errorf("%w; the tree was edited since it was rendered: %s", err, renderFirst)
	}
	for _, unit := range inventory.Units {
		if unit.Kind != UnitKindService || unit.Name != service {
			continue
		}
		if unit.Path == "" {
			return Inventory{}, InventoryUnit{}, fmt.Errorf("service %s/%s is managed outside the rendered tree in %s; there is no image to deploy", module, service, environment)
		}
		return inventory, unit, nil
	}
	return Inventory{}, InventoryUnit{}, fmt.Errorf("the render of module %s does not contain service %s: %s", module, service, renderFirst)
}

// inventoryRenderOptions reconstructs the options that re-derive an inventory
// for an already-rendered tree.
func inventoryRenderOptions(inventory *Inventory) *RenderOptions {
	return &RenderOptions{
		Module: inventory.Module, Unit: inventory.Unit, Environment: inventory.Environment,
		Namespace: inventory.Namespace, AppProject: inventory.AppProject, OwnedPath: inventory.OwnedPath,
		ModulePath: inventory.ModulePath, Package: inventory.Package, Units: inventory.Units,
	}
}

// loadDevService loads the service from its dev source. A --path source is
// applied exactly as the machine-local override would be — the same contract
// check, the same PathOverride plumbing — but only in this process.
func loadDevService(ctx context.Context, module *resources.Module, service string, source DevSource) (*resources.Service, error) {
	if source.Origin == DevSourceFlag {
		ref, err := module.GetServiceReferences(service)
		if err != nil {
			return nil, err
		}
		if ref == nil {
			return nil, fmt.Errorf("module %s declares no service %s", module.Name, service)
		}
		if err := resources.CheckServiceOverrideContract(ctx, module, ref, source.Dir); err != nil {
			return nil, err
		}
		dir := source.Dir
		ref.PathOverride = &dir
	}
	loaded, err := module.LoadServiceFromName(ctx, service)
	if err != nil {
		return nil, fmt.Errorf("load service %s from %s: %w", service, source.Dir, err)
	}
	return loaded, nil
}

// sourceRevision reports the git commit holding dir and whether dir has
// uncommitted changes. A source outside git reports no commit and dirty.
func sourceRevision(ctx context.Context, dir string) (string, bool) {
	commit, err := gitCommand(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", true
	}
	status, err := gitCommand(ctx, dir, "status", "--porcelain", "--untracked-files=normal", "--", ".")
	return commit, err != nil || status != ""
}

// applyDevImages re-pins, inside the service's own rendered unit only, every
// image the dev build produced, then re-derives the inventory with the dev
// record. It returns the root-relative paths it rewrote.
func applyDevImages(root string, inventory *Inventory, unit *InventoryUnit, images []string, dev *InventoryDevDeployment) ([]string, error) {
	entry := *dev
	unitRoot := filepath.Join(root, filepath.FromSlash(unit.Path))
	type rewrite struct {
		pattern *regexp.Regexp
		image   string
		found   bool
	}
	rewrites := make([]*rewrite, 0, len(images))
	for _, image := range images {
		rewrites = append(rewrites, &rewrite{pattern: imageRepositoryPattern(image), image: image})
	}
	var changed []string
	err := walkRegularFiles(unitRoot, func(path, relative string, info os.FileInfo) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		updated := data
		for _, candidate := range rewrites {
			if !candidate.pattern.Match(updated) {
				continue
			}
			candidate.found = true
			updated = candidate.pattern.ReplaceAll(updated, []byte("${1}"+candidate.image))
		}
		if string(updated) == string(data) {
			return nil
		}
		// path is a regular file found by walking the rendered unit itself.
		if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil { //nolint:gosec
			return err
		}
		changed = append(changed, filepath.ToSlash(filepath.Join(unit.Path, relative)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	var primary *rewrite
	for _, candidate := range rewrites {
		if candidate.found {
			primary = candidate
			break
		}
	}
	if primary == nil {
		return nil, fmt.Errorf("the rendered unit %s pins none of the images the build produced (%s): the render is out of date, run a full `codefly deploy gitops render` first", unit.Path, strings.Join(images, ", "))
	}
	entry.Image = primary.image
	entry.Digest = primary.image[strings.LastIndex(primary.image, "@")+1:]

	rebuilt, err := buildInventory(root, inventoryRenderOptions(inventory))
	if err != nil {
		return nil, err
	}
	rebuilt.Dev = append([]InventoryDevDeployment(nil), inventory.Dev...)
	if index := devEntryIndex(rebuilt.Dev, entry.Service); index >= 0 {
		rebuilt.Dev[index] = entry
	} else {
		rebuilt.Dev = append(rebuilt.Dev, entry)
	}
	sort.Slice(rebuilt.Dev, func(i, j int) bool { return rebuilt.Dev[i].Service < rebuilt.Dev[j].Service })
	data, err := canonicalInventory(&rebuilt)
	if err != nil {
		return nil, err
	}
	// The inventory is public and inspectable beside the rendered files, as a render writes it.
	if err := os.WriteFile(filepath.Join(root, InventoryFilename), data, 0o644); err != nil { //nolint:gosec
		return nil, fmt.Errorf("write render inventory: %w", err)
	}
	*inventory = rebuilt
	return append(changed, InventoryFilename), nil
}

func devEntryIndex(entries []InventoryDevDeployment, service string) int {
	for i := range entries {
		if entries[i].Service == service {
			return i
		}
	}
	return -1
}

// imageRepositoryPattern matches any digest pin of image's repository, with or
// without a tag, bounded so that it cannot match a longer repository name.
func imageRepositoryPattern(image string) *regexp.Regexp {
	name := image[:strings.LastIndex(image, "@")]
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name = name[:colon]
	}
	return regexp.MustCompile(`(^|[^A-Za-z0-9._/:@-])` + regexp.QuoteMeta(name) + `(?::[A-Za-z0-9_][A-Za-z0-9_.-]*)?@sha256:[a-fA-F0-9]{64}`)
}

// DevCommitTitle is the commit title a dev deployment is recorded under.
func DevCommitTitle(module string, entry *InventoryDevDeployment) string {
	revision := entry.Commit
	if revision == "" {
		revision = "untracked"
	} else if len(revision) > 12 {
		revision = revision[:12]
	}
	if entry.Dirty {
		revision += "-dirty"
	}
	return fmt.Sprintf("dev: %s/%s from %s@%s", module, entry.Service, entry.Source, revision)
}

const gitAddVerb = "add"

// StageDevDeployment stages the files a dev deployment changed and, when asked,
// commits them and pushes the current branch. It never pushes on its own.
func StageDevDeployment(ctx context.Context, workspaceDir, module string, result *DevResult, commit, push bool) error {
	args := append([]string{gitAddVerb, "--"}, result.Changed...)
	if _, err := gitCommand(ctx, workspaceDir, args...); err != nil {
		return err
	}
	if !commit {
		return nil
	}
	args = append([]string{"commit", "-m", DevCommitTitle(module, &result.Entry), "--"}, result.Changed...)
	if _, err := gitCommand(ctx, workspaceDir, args...); err != nil {
		return err
	}
	if !push {
		return nil
	}
	_, err := gitCommand(ctx, workspaceDir, "push")
	return err
}
