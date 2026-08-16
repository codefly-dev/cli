package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	Source         string
	To             string
	Subdirectory   string
	AcceptUpstream []string
	Apply          bool
	RestoreCode    bool
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

Initialize and populate a brand-new module in one step instead of running
codefly add module first. A new module has no base manifest to plan against, so
the dry-run describes the initialization without registering anything and the
file-level plan appears only on --apply:
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
	ModuleCmd.Flags().BoolVar(&moduleSyncCreate, "create", false, "initialize a new module from --source and populate it in one step (requires --apply)")
	ModuleCmd.Flags().StringVar(&moduleSyncFlags.Source, "source", "", "canonical Git repository URL or local repository/module path")
	ModuleCmd.Flags().StringVar(&moduleSyncFlags.To, "to", "", "immutable semantic-version tag to resolve (for example v0.0.8)")
	ModuleCmd.Flags().StringVar(&moduleSyncFlags.Subdirectory, "subdir", "", "module path inside the source repository (auto-detects module/)")
	ModuleCmd.Flags().StringArrayVar(&moduleSyncFlags.AcceptUpstream, "accept-upstream", nil, "replace one reviewed conflicting path with the immutable upstream version (repeatable)")
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

	plan, err := integrity.PlanBaseSyncWithResolutions(resolved.Root, target.Dir(), options.AcceptUpstream)
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
	if _, err := integrity.ApplyBaseSyncWithResolutions(resolved.Root, target.Dir(), options.AcceptUpstream); err != nil {
		return err
	}
	output.Info("✓ module <%s> base updated; product overlays preserved", target.Name)
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
// created. Pin validates that the checkout actually produced the service code.
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
	if err := integrity.ValidateServiceCodeSource(source.resolved.Root, target.Dir()); err != nil {
		return err
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
	output.Info("  unchanged=%d create=%d update=%d remove=%d omitted=%d allowed=%d", len(plan.Unchanged), len(plan.Create), len(plan.Update), len(plan.Remove), len(plan.Omitted), len(plan.Allowed))
	output.Info("  resolve-upstream=%d already-reconciled=%d", len(plan.ResolveUpstream), len(plan.ReconciledUpstream))
	output.Info("  modified=%d collisions=%d stale-modified=%d released=%d", len(plan.Modified), len(plan.Collisions), len(plan.StaleModified), len(plan.Released))
	printPathGroups([]pathGroup{
		{"CREATE", plan.Create}, {"UPDATE", plan.Update}, {"REMOVE", plan.Remove},
		{"RELEASED TO OVERLAY OWNERSHIP", plan.Released},
		{"RESOLVE FROM UPSTREAM", plan.ResolveUpstream},
		{"ALREADY RECONCILED FROM UPSTREAM", plan.ReconciledUpstream},
	})
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
