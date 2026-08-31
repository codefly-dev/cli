package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/cmd/common"
	output "github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/integrity"
	"github.com/codefly-dev/core/actions/actions"
	actionsmodule "github.com/codefly-dev/core/actions/module"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

const (
	moduleSourceLockRelativePath   = "tools/base-source.json"
	moduleSourceLockSchema         = "codefly/base-source/v1"
	moduleBaseManifestRelativePath = "tools/base-manifest.json"
)

var fullGitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type moduleSourceLock struct {
	Schema       string `json:"schema"`
	Repository   string `json:"repository"`
	Ref          string `json:"ref"`
	Commit       string `json:"commit"`
	Subdirectory string `json:"subdirectory,omitempty"`
}

type moduleSyncOptions struct {
	Source               string
	To                   string
	Subdirectory         string
	AcceptUpstream       []string
	Apply                bool
	RestoreCode          bool
	KeepLocalDivergences bool
}

// ModuleCmd updates an immutable base underneath a product-owned overlay.
// Dry-run is the default; --apply is explicit and re-plans before mutation.
var ModuleCmd = &cobra.Command{
	Use:   "module <name>",
	Short: "Preview or apply a composed module update without overwriting product code",
	Long: `Update a composed module without overwriting product code.

The canonical base owns only paths in tools/base-manifest.json. Consumer-only
files are overlays and remain untouched. Modified base files, new-path
collisions, missing required overlays, and modified upstream deletions fail
closed. The manifest is committed last, so an interrupted update is safely
resumable.

First pin a remote source:
  codefly sync module saas --source https://github.com/codefly-dev/module-saas-starter.git --to v0.0.8 --subdir module
  codefly sync module saas --source https://github.com/codefly-dev/module-saas-starter.git --to v0.0.8 --subdir module --apply

An agent-generated module shell may contain consumer inventory but no base
manifest yet. Its first sync is planned against an empty base and commits the
upstream manifest last, like any later update.

Initialize and populate a brand-new module in one step instead of running
codefly add module first. A new module has no base manifest to plan against, so
the dry-run describes the initialization without registering anything and the
file-level plan appears only on --apply. This does not run a module agent or
generate consumer-owned module and service inventory; use add module --agent
first when the source requires that inventory:
  codefly sync module saas --create --source https://github.com/codefly-dev/module-saas-starter.git --to v0.0.8 --subdir module
  codefly sync module saas --create --source https://github.com/codefly-dev/module-saas-starter.git --to v0.0.8 --subdir module --apply

Future updates use the consumer-owned tools/base-source.json lock:
  codefly sync module saas --to v0.0.9
  codefly sync module saas --to v0.0.9 --apply

Restore missing base-owned service source from the pinned version without
changing existing files or consumer overlays:
  codefly sync module saas --restore-code

For a legacy scaffold with no source lock or recorded agent, provide its
original immutable source once; repair records the lock after validating it:
  codefly sync module saas --restore-code --source https://github.com/codefly-dev/module-saas-starter.git --to v0.0.36 --subdir module

After reviewing a genuine conflict, explicitly select the immutable upstream
version path by path. There is deliberately no accept-all option:
  codefly sync module saas --to v0.0.9 --accept-upstream services/frontend/code/package.json
  codefly sync module saas --to v0.0.9 --accept-upstream services/frontend/code/package.json --apply

For local upstream development, --source may be a repository or module path.
Local paths are preview-only and never persisted in the portable source lock.`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return fmt.Errorf("load workspace: %w", err)
		}
		return runModuleSync(ctx, workspace, args[0], moduleSyncCreate, &moduleSyncFlags)
	},
}

// runModuleSync resolves the target module and syncs it. When --create
// registers a brand-new module as part of an apply, the registration is rolled
// back if the sync itself fails so a failed run never leaves an empty module
// stranded in the workspace.
func runModuleSync(ctx context.Context, workspace *resources.Workspace, name string, create bool, options *moduleSyncOptions) error {
	module, created, err := resolveSyncTarget(ctx, workspace, name, create, options)
	if err != nil {
		return err
	}
	if module == nil {
		// --create dry-run described the intended initialization without
		// registering anything; there is nothing to sync.
		return nil
	}
	if err := syncComposedModule(ctx, module, options); err != nil {
		if created {
			return errors.Join(err, rollbackRegisteredModule(ctx, workspace, name))
		}
		return err
	}
	return nil
}

// resolveSyncTarget resolves the module to sync. When --create is set and the
// module is not yet registered, the source is validated before any workspace
// mutation, so a bad invocation never registers a module only to roll it back.
// A dry-run describes the intended initialization and returns a nil module
// (nothing to sync); an --apply initializes the module and returns it with
// created=true so the caller can roll the registration back on failure.
func resolveSyncTarget(ctx context.Context, workspace *resources.Workspace, name string, create bool, options *moduleSyncOptions) (*resources.Module, bool, error) {
	module, err := workspace.LoadModuleFromName(ctx, name)
	if err == nil {
		return module, false, nil
	}
	if workspace.ExistsModule(name) {
		return nil, false, fmt.Errorf("load target module %s: %w", name, err)
	}
	if !create {
		var present []string
		for _, ref := range workspace.Modules {
			present = append(present, ref.Name)
		}
		return nil, false, fmt.Errorf("module <%s> is not registered in workspace <%s> (present: %v); run `codefly add module %s` first, or rerun with --create --apply to initialize and sync it in one step", name, workspace.Name, present, name)
	}
	if err = validateNewModuleSource(options); err != nil {
		return nil, false, err
	}
	if !options.Apply {
		output.Info("module <%s> is not registered; --create --apply will initialize it and populate it from %s@%s (the file-level plan is shown on --apply)",
			name, strings.TrimSpace(options.Source), strings.TrimSpace(options.To))
		return nil, false, nil
	}
	module, err = registerModule(ctx, workspace, name)
	if err != nil {
		return nil, false, err
	}
	return module, true, nil
}

// validateNewModuleSource checks the cheap preconditions for initializing a new
// module before any workspace mutation. A new module has no source lock to fall
// back on, so it must come from an immutable remote tag: local paths are
// preview-only and cannot be applied.
func validateNewModuleSource(options *moduleSyncOptions) error {
	source := strings.TrimSpace(options.Source)
	if source == "" {
		return fmt.Errorf("--create requires --source to initialize a new module")
	}
	if info, statErr := os.Stat(source); statErr == nil && info.IsDir() {
		return fmt.Errorf("--create requires an immutable remote source; local path %s is preview-only and cannot initialize a module", source)
	}
	to := strings.TrimSpace(options.To)
	if to == "" {
		return fmt.Errorf("--to is required to initialize a new module from a remote source")
	}
	if _, err := semver.NewVersion(strings.TrimPrefix(to, "v")); err != nil {
		return fmt.Errorf("--to must be an immutable semantic-version tag: %w", err)
	}
	return nil
}

// registerModule initializes a new module and seeds an empty base manifest so
// the very first sync sees the whole upstream base as new files to create.
// Without the seed the base-sync engine fails closed on the missing manifest.
func registerModule(ctx context.Context, workspace *resources.Workspace, name string) (*resources.Module, error) {
	action, err := actionsmodule.NewActionAddModule(ctx, &actionsmodule.AddModule{Name: name})
	if err != nil {
		return nil, fmt.Errorf("create add-module action: %w", err)
	}
	if _, err = actions.Run(ctx, action, &actions.Space{Workspace: workspace}); err != nil {
		return nil, fmt.Errorf("register module %s: %w", name, err)
	}
	module, err := workspace.LoadModuleFromName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("load target module %s after --create: %w", name, err)
	}
	if err := seedEmptyBaseManifest(module.Dir()); err != nil {
		return nil, fmt.Errorf("seed base manifest for %s: %w", name, err)
	}
	output.Info("registered module <%s>", name)
	return module, nil
}

func seedEmptyBaseManifest(moduleDir string) error {
	path := filepath.Join(moduleDir, moduleBaseManifestRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("{\n  \"files\": {}\n}\n"), 0o600)
}

func rollbackRegisteredModule(ctx context.Context, workspace *resources.Workspace, name string) error {
	moduleDir := workspace.ModulePath(ctx, &resources.ModuleReference{Name: name})
	if err := workspace.DeleteModule(ctx, name); err != nil {
		return fmt.Errorf("roll back module reference %s: %w", name, err)
	}
	if err := os.RemoveAll(moduleDir); err != nil {
		return fmt.Errorf("roll back module directory %s: %w", name, err)
	}
	return nil
}

var moduleSyncFlags moduleSyncOptions

var moduleSyncCreate bool

func init() {
	ModuleCmd.Flags().BoolVar(&moduleSyncCreate, "create", false, "initialize a new module base from --source without agent-generated consumer inventory")
	ModuleCmd.Flags().StringVar(&moduleSyncFlags.Source, "source", "", "canonical Git repository URL or local repository/module path")
	ModuleCmd.Flags().StringVar(&moduleSyncFlags.To, "to", "", "immutable semantic-version tag to resolve (for example v0.0.8)")
	ModuleCmd.Flags().StringVar(&moduleSyncFlags.Subdirectory, "subdir", "", "module path inside the source repository (auto-detects module/)")
	ModuleCmd.Flags().StringArrayVar(&moduleSyncFlags.AcceptUpstream, "accept-upstream", nil, "replace one reviewed conflicting path with the immutable upstream version (repeatable)")
	ModuleCmd.Flags().BoolVar(&moduleSyncFlags.KeepLocalDivergences, "keep-local-divergences", false, "re-affirm allow-listed divergences whose upstream changed or was removed, keeping the local version and advancing the recorded base")
	ModuleCmd.Flags().BoolVar(&moduleSyncFlags.Apply, "apply", false, "apply the reviewed update; default is dry-run")
	ModuleCmd.Flags().BoolVar(&moduleSyncFlags.RestoreCode, "restore-code", false, "restore missing base-owned service code from the pinned module version")
}

func syncComposedModule(ctx context.Context, target *resources.Module, options *moduleSyncOptions) error {
	if options.RestoreCode {
		return restoreComposedModuleCode(ctx, target, options)
	}
	resolved, cleanup, err := resolveModuleSource(ctx, target.Dir(), options)
	if err != nil {
		return err
	}
	defer cleanup()

	plan, err := integrity.PlanBaseSyncWithResolutions(resolved.Root, target.Dir(), options.AcceptUpstream, options.KeepLocalDivergences)
	if err != nil {
		return err
	}
	if resolved.Lock != nil {
		// The source lock is a consumer-owned survival contract. A first update
		// can bootstrap that required addition because it is written before any
		// base-owned path is changed.
		plan.MissingRequiredAdditions = withoutPath(plan.MissingRequiredAdditions, moduleSourceLockRelativePath)
	}
	printModuleSyncPlan(target.Name, &plan, options.Apply, resolved.Lock)
	if err := plan.Applicable(); err != nil {
		return err
	}
	pendingRefresh, err := integrity.PlanServiceManifestRefresh(resolved.Root, target.Dir())
	if err != nil {
		return fmt.Errorf("plan service manifest refresh: %w", err)
	}
	printServiceManifestRefreshPlan(pendingRefresh, options.Apply)
	pendingLocks, err := staleLockfiles(resolved.Root, target.Dir(), &plan)
	if err != nil {
		return fmt.Errorf("inspect service lockfiles: %w", err)
	}
	printLockfileRefreshPlan(lockLabels(pendingLocks), options.Apply)
	if !options.Apply {
		output.Info("module sync dry-run is applicable; rerun with --apply")
		return nil
	}
	if resolved.Lock == nil {
		return fmt.Errorf("local module sources are preview-only; publish an immutable semantic-version tag before applying")
	}
	// Commit desired provenance first. If a later filesystem operation is
	// interrupted, the default command resolves the intended source and safely
	// resumes instead of silently reverting to the previous base.
	if err := writeModuleSourceLock(filepath.Join(target.Dir(), moduleSourceLockRelativePath), resolved.Lock); err != nil {
		return fmt.Errorf("write module source lock: %w", err)
	}
	applied, err := integrity.ApplyBaseSyncWithResolutions(resolved.Root, target.Dir(), options.AcceptUpstream, options.KeepLocalDivergences)
	if err != nil {
		return err
	}
	output.Info("✓ module <%s> base updated; product overlays preserved", target.Name)
	refreshed, err := integrity.RefreshServiceManifests(resolved.Root, target.Dir())
	if err != nil {
		return fmt.Errorf("refresh generated service manifests: %w", err)
	}
	if len(refreshed) > 0 {
		output.Info("✓ refreshed %d generated service manifest(s) to the pinned agent versions:", len(refreshed))
		for _, relative := range refreshed {
			output.Info("  REFRESHED %s", relative)
		}
		output.Info("restart any active stack so services load the refreshed agent pins")
	}
	// Regenerate lockfiles last. The base update and manifest refresh are
	// deterministic filesystem operations; npm resolves against a registry and
	// can fail. Running it after those two commit means a network failure leaves
	// only the lockfile adrift, and the next sync — which sees package.json as
	// unchanged but the lockfile still out of sync — heals it.
	if err := regenerateNpmLockfiles(ctx, resolved.Root, target.Dir(), &applied); err != nil {
		return err
	}
	return nil
}

func printServiceManifestRefreshPlan(pending []string, applying bool) {
	if len(pending) == 0 {
		return
	}
	label := "WOULD REFRESH GENERATED SERVICE MANIFESTS"
	if applying {
		label = "WILL REFRESH GENERATED SERVICE MANIFESTS"
	}
	output.Info("  %s (%d): %s", label, len(pending), strings.Join(pending, ", "))
}

func printLockfileRefreshPlan(labels []string, applying bool) {
	if len(labels) == 0 {
		return
	}
	label := "WOULD REGENERATE SERVICE LOCKFILES"
	if applying {
		label = "WILL REGENERATE SERVICE LOCKFILES"
	}
	output.Info("  %s (%d): %s", label, len(labels), strings.Join(labels, ", "))
}

// npmLockNames are the lockfiles `npm ci` consumes, in npm's own precedence: an
// npm-shrinkwrap.json shadows a package-lock.json, and `npm install
// --package-lock-only` updates whichever of the two is present.
var npmLockNames = []string{"npm-shrinkwrap.json", "package-lock.json"}

func serviceLockfile(dir string) (string, bool) {
	for _, name := range npmLockNames {
		if fileExists(filepath.Join(dir, name)) {
			return name, true
		}
	}
	return "", false
}

type staleLock struct {
	dir      string // module-relative directory holding the package.json and its lockfile
	lockName string
}

func lockLabels(stale []staleLock) []string {
	labels := make([]string, len(stale))
	for i, entry := range stale {
		labels[i] = path.Join(entry.dir, entry.lockName)
	}
	return labels
}

// staleLockfiles lists the service directories whose lockfile does not yet
// record the dependencies the pinned base declares. Selection is driven by
// on-disk drift, not by whether *this* run rewrote the package.json: a base sync
// commits its manifest last, so a package.json a prior interrupted run already
// wrote reappears as Unchanged. Keying off drift instead lets a rerun heal a
// lockfile that a failed regeneration left behind, and skips a directory whose
// lockfile is already in sync. The incoming package.json is read from the pinned
// source (which, for an Unchanged path, is byte-identical to the target), so the
// dry-run predicts the post-apply drift rather than the pre-apply state. A
// directory without a lockfile is not an `npm ci` workflow and is never given
// one.
func staleLockfiles(sourceRoot, moduleDir string, plan *integrity.BaseSyncPlan) ([]staleLock, error) {
	var stale []staleLock
	for _, relative := range slices.Concat(plan.Unchanged, plan.Create, plan.Update, plan.ResolveUpstream) {
		if path.Base(relative) != "package.json" {
			continue
		}
		relativeDir := path.Dir(relative)
		lockName, ok := serviceLockfile(filepath.Join(moduleDir, filepath.FromSlash(relativeDir)))
		if !ok {
			continue
		}
		drifted, err := lockfileStale(
			filepath.Join(sourceRoot, filepath.FromSlash(relative)),
			filepath.Join(moduleDir, filepath.FromSlash(relativeDir), lockName),
		)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", path.Join(relativeDir, lockName), err)
		}
		if drifted {
			stale = append(stale, staleLock{dir: relativeDir, lockName: lockName})
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].dir < stale[j].dir })
	return stale, nil
}

type nodeDependencies struct {
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

// lockfileStale reports whether packageJSON declares a direct dependency the
// lockfile's root package does not already record with the same specifier. This
// is exactly the drift `npm ci` rejects ("Missing: X from lock file"):
// package.json edits only touch direct dependency specifiers, which npm mirrors
// verbatim into the lockfile's root package. A lockfile npm cannot describe
// (lockfileVersion 1, which has no packages map, or an unparseable/absent root
// entry) is treated as stale so it is regenerated rather than trusted; equally,
// a package.json that does not parse is treated as stale so npm surfaces the
// real error. The comparison is exact, so a specifier npm normalizes on write
// can cause a redundant but harmless regeneration — the safe direction, since a
// missed drift reintroduces the very failure this guards against.
func lockfileStale(packageJSONPath, lockPath string) (bool, error) {
	packageBytes, err := os.ReadFile(packageJSONPath)
	if err != nil {
		return false, err
	}
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		return false, err
	}
	var pkg nodeDependencies
	if json.Unmarshal(packageBytes, &pkg) != nil {
		return true, nil
	}
	var lock struct {
		Packages map[string]nodeDependencies `json:"packages"`
	}
	if json.Unmarshal(lockBytes, &lock) != nil {
		return true, nil
	}
	root, ok := lock.Packages[""]
	if !ok {
		return true, nil
	}
	return !sameDependencies(pkg, root), nil
}

func sameDependencies(a, b nodeDependencies) bool {
	return sameDependencyMap(a.Dependencies, b.Dependencies) &&
		sameDependencyMap(a.DevDependencies, b.DevDependencies) &&
		sameDependencyMap(a.OptionalDependencies, b.OptionalDependencies) &&
		sameDependencyMap(a.PeerDependencies, b.PeerDependencies)
}

func sameDependencyMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, spec := range a {
		if b[name] != spec {
			return false
		}
	}
	return true
}

// regenerateNpmLockfiles brings each drifted service lockfile back in sync with
// the pinned base's dependencies. A base sync rewrites a service's package.json
// but leaves the committed lockfile untouched, so the next `npm ci` (for example
// in a render's frontend Dockerfile) fails closed on the drift.
// `--package-lock-only` rewrites the lockfile without materializing node_modules.
func regenerateNpmLockfiles(ctx context.Context, sourceRoot, moduleDir string, plan *integrity.BaseSyncPlan) error {
	stale, err := staleLockfiles(sourceRoot, moduleDir, plan)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	labels := lockLabels(stale)
	if _, lookErr := exec.LookPath("npm"); lookErr != nil {
		return fmt.Errorf("refreshed dependencies left %d service lockfile(s) out of sync but npm is not installed to regenerate them; install npm, then run `npm install --package-lock-only` in the directory of each: %s", len(labels), strings.Join(labels, ", "))
	}
	for _, entry := range stale {
		command := exec.CommandContext(ctx, "npm", "install", "--package-lock-only", "--no-audit", "--no-fund")
		command.Dir = filepath.Join(moduleDir, filepath.FromSlash(entry.dir))
		if out, runErr := command.CombinedOutput(); runErr != nil {
			return fmt.Errorf("regenerate %s: %w: %s", path.Join(entry.dir, entry.lockName), runErr, strings.TrimSpace(string(out)))
		}
	}
	output.Info("✓ regenerated %d service lockfile(s) to match the refreshed dependencies:", len(stale))
	for _, label := range labels {
		output.Info("  REGENERATED %s", label)
	}
	return nil
}

func restoreComposedModuleCode(ctx context.Context, target *resources.Module, options *moduleSyncOptions) error {
	if len(options.AcceptUpstream) > 0 || options.Apply {
		return fmt.Errorf("--restore-code cannot be combined with update or conflict-resolution flags")
	}
	resolved, writeLock, cleanup, err := resolveRestoreModuleSource(ctx, target, options)
	if err != nil {
		return err
	}
	defer cleanup()
	if validationErr := integrity.ValidateServiceCodeSource(resolved.Root, target.Dir()); validationErr != nil {
		return fmt.Errorf("validate pinned module source: %w", validationErr)
	}
	restored, err := integrity.RestoreMissingServiceCode(resolved.Root, target.Dir())
	if err != nil {
		return err
	}
	if writeLock {
		if err := writeModuleSourceLock(filepath.Join(target.Dir(), moduleSourceLockRelativePath), resolved.Lock); err != nil {
			return fmt.Errorf("write module source lock: %w", err)
		}
	}
	if len(restored) == 0 {
		output.Info("module <%s>: no missing base-owned service code", target.Name)
		return nil
	}
	output.Info("✓ module <%s>: restored %d base-owned service code file(s)", target.Name, len(restored))
	for _, relative := range restored {
		output.Info("  RESTORED %s", relative)
	}
	output.Info("restart any active stack so services load the restored files")
	return nil
}

func resolveRestoreModuleSource(ctx context.Context, target *resources.Module, options *moduleSyncOptions) (resolvedModuleSource, bool, func(), error) {
	lockPath := filepath.Join(target.Dir(), moduleSourceLockRelativePath)
	_, lockErr := os.Stat(lockPath)
	lockPresent := lockErr == nil
	if lockErr != nil && !os.IsNotExist(lockErr) {
		return resolvedModuleSource{}, false, func() {}, fmt.Errorf("inspect %s: %w", moduleSourceLockRelativePath, lockErr)
	}
	explicitSource := strings.TrimSpace(options.Source) != "" || strings.TrimSpace(options.To) != "" || strings.TrimSpace(options.Subdirectory) != ""
	if lockPresent {
		if explicitSource {
			return resolvedModuleSource{}, false, func() {}, fmt.Errorf("--restore-code uses the pinned %s source; source flags are only accepted when that lock is missing", moduleSourceLockRelativePath)
		}
		resolved, cleanup, err := resolveModuleSource(ctx, target.Dir(), &moduleSyncOptions{})
		return resolved, false, cleanup, err
	}
	if strings.TrimSpace(options.Source) == "" {
		if explicitSource {
			return resolvedModuleSource{}, false, func() {}, fmt.Errorf("--source is required when bootstrapping a missing %s", moduleSourceLockRelativePath)
		}
		if target.Agent == nil {
			return resolvedModuleSource{}, false, func() {}, fmt.Errorf("module has no %s or recorded agent; rerun with --source, --to, and optional --subdir", moduleSourceLockRelativePath)
		}
		var err error
		resolvedOptions, err := moduleAgentSource(target.Agent)
		if err != nil {
			return resolvedModuleSource{}, false, func() {}, err
		}
		options = &resolvedOptions
	}
	resolved, cleanup, err := resolveModuleSource(ctx, target.Dir(), options)
	if err != nil {
		return resolvedModuleSource{}, false, cleanup, err
	}
	if resolved.Lock == nil {
		cleanup()
		return resolvedModuleSource{}, false, func() {}, fmt.Errorf("restore source must be an immutable semantic-version tag")
	}
	return resolved, true, cleanup, nil
}

// PreparedModuleSource is an immutable checkout resolved before a scaffold is
// created. Pin validates any base ownership claimed by the scaffold.
type PreparedModuleSource struct {
	resolved resolvedModuleSource
	cleanup  func()
}

func PrepareModuleSource(ctx context.Context, targetRoot string, agent *resources.Agent) (*PreparedModuleSource, error) {
	options, err := moduleAgentSource(agent)
	if err != nil {
		return nil, err
	}
	return prepareModuleSource(ctx, targetRoot, &options)
}

func moduleAgentSource(agent *resources.Agent) (moduleSyncOptions, error) {
	registration, err := resources.AgentKindRegistrationFor(agent.Kind)
	if err != nil {
		return moduleSyncOptions{}, err
	}
	repository := fmt.Sprintf(
		"https://github.com/%s/%s.git",
		strings.ReplaceAll(agent.Publisher, ".", "-"),
		registration.GitHubRepository(agent.Name),
	)
	ref := "v" + strings.TrimPrefix(agent.Version, "v")
	return moduleSyncOptions{Source: repository, To: ref}, nil
}

func prepareModuleSource(ctx context.Context, targetRoot string, options *moduleSyncOptions) (*PreparedModuleSource, error) {
	resolved, cleanup, err := resolveModuleSource(ctx, targetRoot, options)
	if err != nil {
		return nil, err
	}
	if resolved.Lock == nil {
		cleanup()
		return nil, fmt.Errorf("module source did not resolve to an immutable lock")
	}
	return &PreparedModuleSource{resolved: resolved, cleanup: cleanup}, nil
}

func (source *PreparedModuleSource) Close() {
	if source != nil && source.cleanup != nil {
		source.cleanup()
		source.cleanup = nil
	}
}

func (source *PreparedModuleSource) Pin(target *resources.Module) error {
	if source == nil || source.resolved.Lock == nil {
		return fmt.Errorf("module source is not prepared")
	}
	manifestPath := filepath.Join(target.Dir(), moduleBaseManifestRelativePath)
	if _, err := os.Stat(manifestPath); err == nil {
		if validateErr := integrity.ValidateServiceCodeSource(source.resolved.Root, target.Dir()); validateErr != nil {
			return validateErr
		}
	} else if os.IsNotExist(err) {
		// Inventory-only scaffold: there is no base manifest to validate service
		// code against, but the scaffold must not carry base-owned code that
		// diverges from the pinned source, or the first sync would fail closed on
		// an inconsistent, manifest-less module.
		if validateErr := integrity.ValidateInventoryOnlyScaffold(source.resolved.Root, target.Dir()); validateErr != nil {
			return validateErr
		}
	} else {
		return fmt.Errorf("inspect module scaffold base manifest: %w", err)
	}
	return writeModuleSourceLock(filepath.Join(target.Dir(), moduleSourceLockRelativePath), source.resolved.Lock)
}

type resolvedModuleSource struct {
	Root string
	Lock *moduleSourceLock
}

func resolveModuleSource(ctx context.Context, targetRoot string, options *moduleSyncOptions) (resolvedModuleSource, func(), error) {
	cleanup := func() {}
	source := strings.TrimSpace(options.Source)
	to := strings.TrimSpace(options.To)
	subdirectory, err := cleanSubdirectory(options.Subdirectory)
	if err != nil {
		return resolvedModuleSource{}, cleanup, err
	}
	lockPath := filepath.Join(targetRoot, moduleSourceLockRelativePath)
	var existing moduleSourceLock
	existingLockPresent := false
	if candidate, lockErr := readModuleSourceLock(lockPath); lockErr == nil {
		existing = candidate
		existingLockPresent = true
	} else if !os.IsNotExist(lockErr) {
		return resolvedModuleSource{}, cleanup, fmt.Errorf("read %s: %w", moduleSourceLockRelativePath, lockErr)
	}
	usingExistingLock := false
	if source == "" {
		if !existingLockPresent {
			return resolvedModuleSource{}, cleanup, fmt.Errorf("--source is required until %s exists", moduleSourceLockRelativePath)
		}
		usingExistingLock = true
		source = existing.Repository
		if to == "" {
			to = existing.Ref
		}
		if subdirectory == "" {
			subdirectory = existing.Subdirectory
		}
	} else if existingLockPresent && source == existing.Repository {
		usingExistingLock = true
		if to == "" {
			to = existing.Ref
		}
		if subdirectory == "" {
			subdirectory = existing.Subdirectory
		}
	}

	if info, statErr := os.Stat(source); statErr == nil && info.IsDir() {
		root, rootErr := locateModuleRoot(source, subdirectory)
		if rootErr != nil {
			return resolvedModuleSource{}, cleanup, rootErr
		}
		return resolvedModuleSource{Root: root}, cleanup, nil
	}
	if to == "" {
		return resolvedModuleSource{}, cleanup, fmt.Errorf("--to is required for a remote module source")
	}
	if _, verErr := semver.NewVersion(strings.TrimPrefix(to, "v")); verErr != nil {
		return resolvedModuleSource{}, cleanup, fmt.Errorf("--to must be an immutable semantic-version tag: %w", verErr)
	}

	checkout, err := os.MkdirTemp("", "codefly-module-source-*")
	if err != nil {
		return resolvedModuleSource{}, cleanup, fmt.Errorf("create source checkout: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(checkout) }
	clone := exec.CommandContext(ctx, "git", "clone", "--quiet", "--depth", "1", "--branch", to, source, checkout)
	if outputBytes, cloneErr := clone.CombinedOutput(); cloneErr != nil {
		cleanup()
		return resolvedModuleSource{}, func() {}, fmt.Errorf("resolve module source %s@%s: %w: %s", source, to, cloneErr, strings.TrimSpace(string(outputBytes)))
	}
	tagRef := "refs/tags/" + to
	verifyTag := exec.CommandContext(ctx, "git", "-C", checkout, "show-ref", "--verify", "--quiet", tagRef)
	if err = verifyTag.Run(); err != nil {
		cleanup()
		return resolvedModuleSource{}, func() {}, fmt.Errorf("module source ref %s is not a tag", to)
	}
	commitCommand := exec.CommandContext(ctx, "git", "-C", checkout, "rev-parse", tagRef+"^{commit}")
	commitBytes, err := commitCommand.Output()
	if err != nil {
		cleanup()
		return resolvedModuleSource{}, func() {}, fmt.Errorf("resolve module source commit: %w", err)
	}
	commit := strings.TrimSpace(string(commitBytes))
	if !fullGitCommitPattern.MatchString(commit) {
		cleanup()
		return resolvedModuleSource{}, func() {}, fmt.Errorf("resolved module source has invalid commit %q", commit)
	}
	if usingExistingLock && to == existing.Ref && commit != existing.Commit {
		cleanup()
		return resolvedModuleSource{}, func() {}, fmt.Errorf("immutable module tag %s moved from %s to %s", to, existing.Commit, commit)
	}
	root, err := locateModuleRoot(checkout, subdirectory)
	if err != nil {
		cleanup()
		return resolvedModuleSource{}, func() {}, err
	}
	if subdirectory == "" && root != checkout {
		subdirectory = filepath.ToSlash(strings.TrimPrefix(root, checkout+string(filepath.Separator)))
	}
	return resolvedModuleSource{
		Root: root,
		Lock: &moduleSourceLock{
			Schema:       moduleSourceLockSchema,
			Repository:   source,
			Ref:          to,
			Commit:       commit,
			Subdirectory: subdirectory,
		},
	}, cleanup, nil
}

func locateModuleRoot(source, subdirectory string) (string, error) {
	root := source
	if subdirectory != "" {
		root = filepath.Join(source, filepath.FromSlash(subdirectory))
	}
	if fileExists(filepath.Join(root, "tools", "base-manifest.json")) {
		return filepath.Abs(root)
	}
	if subdirectory == "" && fileExists(filepath.Join(source, "module", "tools", "base-manifest.json")) {
		return filepath.Abs(filepath.Join(source, "module"))
	}
	return "", fmt.Errorf("source does not contain tools/base-manifest.json at %s", root)
}

func cleanSubdirectory(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", nil
	}
	if filepath.IsAbs(raw) {
		return "", fmt.Errorf("--subdir must be relative to the source repository")
	}
	clean := filepath.Clean(raw)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--subdir escapes the source repository")
	}
	return filepath.ToSlash(clean), nil
}

func readModuleSourceLock(path string) (moduleSourceLock, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return moduleSourceLock{}, err
	}
	var lock moduleSourceLock
	if err := json.Unmarshal(payload, &lock); err != nil {
		return moduleSourceLock{}, err
	}
	if lock.Schema != moduleSourceLockSchema || strings.TrimSpace(lock.Repository) == "" ||
		strings.TrimSpace(lock.Ref) == "" || !fullGitCommitPattern.MatchString(lock.Commit) {
		return moduleSourceLock{}, fmt.Errorf("invalid or unsupported source lock")
	}
	if _, err := semver.NewVersion(strings.TrimPrefix(lock.Ref, "v")); err != nil {
		return moduleSourceLock{}, fmt.Errorf("source lock ref must be an immutable semantic-version tag: %w", err)
	}
	if _, err := cleanSubdirectory(lock.Subdirectory); err != nil {
		return moduleSourceLock{}, fmt.Errorf("invalid source lock subdirectory: %w", err)
	}
	return lock, nil
}

func writeModuleSourceLock(path string, lock *moduleSourceLock) error {
	payload, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".codefly-base-source-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o644); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func printModuleSyncPlan(module string, plan *integrity.BaseSyncPlan, applying bool, lock *moduleSourceLock) {
	mode := "dry-run"
	if applying {
		mode = "apply"
	}
	output.Info("module sync: %s <%s>", mode, module)
	if lock != nil {
		output.Info("  source: %s@%s (%s)", lock.Repository, lock.Ref, lock.Commit[:12])
	} else {
		output.Info("  source: %s (local, not persisted)", plan.SourceRoot)
	}
	// upstream-changed is a subset of allowed (both are source-owned paths), so it
	// is shown parenthetically to avoid reading as an additive count; upstream-removed
	// is disjoint (the path left the source) and is its own token.
	output.Info("  unchanged=%d create=%d update=%d remove=%d omitted=%d allowed=%d (upstream-changed=%d) allowed-upstream-removed=%d", len(plan.Unchanged), len(plan.Create), len(plan.Update), len(plan.Remove), len(plan.Omitted), len(plan.Allowed), len(plan.AllowedUpstreamChanged), len(plan.AllowedUpstreamRemoved))
	output.Info("  resolve-upstream=%d already-reconciled=%d", len(plan.ResolveUpstream), len(plan.ReconciledUpstream))
	output.Info("  modified=%d collisions=%d stale-modified=%d released=%d", len(plan.Modified), len(plan.Collisions), len(plan.StaleModified), len(plan.Released))
	printPathGroups([]pathGroup{
		{"CREATE", plan.Create}, {"UPDATE", plan.Update}, {"REMOVE", plan.Remove},
		{"RELEASED TO OVERLAY OWNERSHIP", plan.Released},
		{"RESOLVE FROM UPSTREAM", plan.ResolveUpstream},
		{"ALREADY RECONCILED FROM UPSTREAM", plan.ReconciledUpstream},
	})
	if masked := len(plan.AllowedUpstreamChanged) + len(plan.AllowedUpstreamRemoved); masked > 0 {
		if plan.MaskedUpstreamBlocked() {
			output.Warning("  %d allow-listed file(s) received an upstream change this sync; sync is BLOCKED until you decide (they would otherwise be kept LOCAL, silently dropping the upstream change):", masked)
		} else {
			output.Warning("  %d allow-listed file(s) received an upstream change this sync, kept LOCAL by --keep-local-divergences (the recorded base advances to acknowledge it):", masked)
		}
		printPathGroups([]pathGroup{
			{"ALLOW-LISTED DIVERGENCE MASKING AN UPSTREAM CHANGE", plan.AllowedUpstreamChanged},
			{"ALLOW-LISTED DIVERGENCE MASKING AN UPSTREAM REMOVAL", plan.AllowedUpstreamRemoved},
		})
		if plan.MaskedUpstreamBlocked() {
			output.Warning("  to proceed: keep the divergences with --keep-local-divergences, or take upstream by removing the entry from tools/base-integrity-allow.json and re-running (optionally with --accept-upstream <path>), or reconcile by hand.")
		}
	}
	for _, line := range sourceInvalidReport(plan.SourceInvalid) {
		output.Info("  %s", line)
	}
	printPathGroups([]pathGroup{
		{"INVALID TARGET PATHS", plan.TargetInvalid},
		{"MODIFIED BASE", plan.Modified}, {"OVERLAY COLLISIONS", plan.Collisions},
		{"MODIFIED UPSTREAM DELETIONS", plan.StaleModified}, {"MISSING REQUIRED OVERLAYS", plan.MissingRequiredAdditions},
		{"INVALID REQUIRED OVERLAYS", plan.InvalidRequiredAdditions},
	})
}

type pathGroup struct {
	label string
	paths []string
}

func printPathGroups(groups []pathGroup) {
	for _, group := range groups {
		if len(group.paths) == 0 {
			continue
		}
		sort.Strings(group.paths)
		output.Info("  %s (%d): %s", group.label, len(group.paths), strings.Join(group.paths, ", "))
	}
}

// sourceInvalidReport explains why each source-owned path was rejected. The
// reasons fold into one blocker count but have different remedies, so each is
// reported separately with the action the operator can actually take. Groups
// are driven by the reasons actually present, with known reasons ordered first
// and any unrecognized reason still surfaced, so a newly added classification
// can never be silently dropped from the plan output.
func sourceInvalidReport(entries []integrity.InvalidSource) []string {
	byReason := map[integrity.SourceInvalidReason][]integrity.InvalidSource{}
	for _, entry := range entries {
		byReason[entry.Reason] = append(byReason[entry.Reason], entry)
	}
	ordered := make([]integrity.SourceInvalidReason, 0, 3+len(byReason))
	ordered = append(ordered, integrity.SourceDigestMismatch, integrity.SourceUnreadable, integrity.SourceUnsafePath)
	var unknown []integrity.SourceInvalidReason
	for reason := range byReason {
		if !slices.Contains(ordered, reason) {
			unknown = append(unknown, reason)
		}
	}
	slices.Sort(unknown)
	ordered = append(ordered, unknown...)

	var lines []string
	for _, reason := range ordered {
		matched := byReason[reason]
		if len(matched) == 0 {
			continue
		}
		sort.Slice(matched, func(i, j int) bool { return matched[i].Path < matched[j].Path })
		lines = append(lines, fmt.Sprintf("%s (%d):", sourceInvalidHeading(reason), len(matched)))
		for _, entry := range matched {
			lines = append(lines, "  "+entry.Path)
			if reason == integrity.SourceDigestMismatch {
				lines = append(lines, fmt.Sprintf("    manifest %s  actual %s", shortDigest(entry.ManifestDigest), shortDigest(entry.ActualDigest)))
			}
		}
		lines = append(lines, "  -> "+sourceInvalidRemedy(reason))
	}
	return lines
}

func sourceInvalidHeading(reason integrity.SourceInvalidReason) string {
	switch reason {
	case integrity.SourceDigestMismatch:
		return "INVALID SOURCE (digest mismatch, upstream manifest is stale)"
	case integrity.SourceUnreadable:
		return "INVALID SOURCE (missing or unreadable source file)"
	case integrity.SourceUnsafePath:
		return "INVALID SOURCE (unsafe path)"
	default:
		return fmt.Sprintf("INVALID SOURCE (%s)", reason)
	}
}

func sourceInvalidRemedy(reason integrity.SourceInvalidReason) string {
	switch reason {
	case integrity.SourceDigestMismatch:
		return "report to the base repository and pin to a working tag; this is not resolvable in the consumer"
	case integrity.SourceUnreadable:
		return "the upstream checkout is incomplete; re-pin to a working tag or report to the base repository"
	case integrity.SourceUnsafePath:
		return "the upstream manifest lists a path that escapes the module; report to the base repository"
	default:
		return "report to the base repository"
	}
}

func shortDigest(digest string) string {
	if len(digest) <= 8 {
		return digest
	}
	return digest[:8] + "..."
}

func withoutPath(paths []string, excluded string) []string {
	filtered := paths[:0]
	for _, path := range paths {
		if path != excluded {
			filtered = append(filtered, path)
		}
	}
	return filtered
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
