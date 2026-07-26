package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/cmd/common"
	output "github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/integrity"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

const (
	moduleSourceLockRelativePath = "tools/base-source.json"
	moduleSourceLockSchema       = "codefly/base-source/v1"
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

Future updates use the consumer-owned tools/base-source.json lock:
  codefly sync module saas --to v0.0.9
  codefly sync module saas --to v0.0.9 --apply

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
		module, err := workspace.LoadModuleFromName(ctx, args[0])
		if err != nil {
			return fmt.Errorf("load target module %s: %w", args[0], err)
		}
		return syncComposedModule(ctx, module, moduleSyncFlags)
	},
}

var moduleSyncFlags moduleSyncOptions

func init() {
	ModuleCmd.Flags().StringVar(&moduleSyncFlags.Source, "source", "", "canonical Git repository URL or local repository/module path")
	ModuleCmd.Flags().StringVar(&moduleSyncFlags.To, "to", "", "immutable semantic-version tag to resolve (for example v0.0.8)")
	ModuleCmd.Flags().StringVar(&moduleSyncFlags.Subdirectory, "subdir", "", "module path inside the source repository (auto-detects module/)")
	ModuleCmd.Flags().StringArrayVar(&moduleSyncFlags.AcceptUpstream, "accept-upstream", nil, "replace one reviewed conflicting path with the immutable upstream version (repeatable)")
	ModuleCmd.Flags().BoolVar(&moduleSyncFlags.Apply, "apply", false, "apply the reviewed update; default is dry-run")
}

func syncComposedModule(ctx context.Context, target *resources.Module, options moduleSyncOptions) error {
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
	printModuleSyncPlan(target.Name, plan, options.Apply, resolved.Lock)
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
	if err := writeModuleSourceLock(filepath.Join(target.Dir(), moduleSourceLockRelativePath), *resolved.Lock); err != nil {
		return fmt.Errorf("write module source lock: %w", err)
	}
	if _, err := integrity.ApplyBaseSyncWithResolutions(resolved.Root, target.Dir(), options.AcceptUpstream); err != nil {
		return err
	}
	output.Info("✓ module <%s> base updated; product overlays preserved", target.Name)
	return nil
}

type resolvedModuleSource struct {
	Root string
	Lock *moduleSourceLock
}

func resolveModuleSource(ctx context.Context, targetRoot string, options moduleSyncOptions) (resolvedModuleSource, func(), error) {
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

	if info, err := os.Stat(source); err == nil && info.IsDir() {
		root, err := locateModuleRoot(source, subdirectory)
		if err != nil {
			return resolvedModuleSource{}, cleanup, err
		}
		return resolvedModuleSource{Root: root}, cleanup, nil
	}
	if to == "" {
		return resolvedModuleSource{}, cleanup, fmt.Errorf("--to is required for a remote module source")
	}
	if _, err := semver.NewVersion(strings.TrimPrefix(to, "v")); err != nil {
		return resolvedModuleSource{}, cleanup, fmt.Errorf("--to must be an immutable semantic-version tag: %w", err)
	}

	checkout, err := os.MkdirTemp("", "codefly-module-source-*")
	if err != nil {
		return resolvedModuleSource{}, cleanup, fmt.Errorf("create source checkout: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(checkout) }
	clone := exec.CommandContext(ctx, "git", "clone", "--quiet", "--depth", "1", "--branch", to, source, checkout)
	if outputBytes, err := clone.CombinedOutput(); err != nil {
		cleanup()
		return resolvedModuleSource{}, func() {}, fmt.Errorf("resolve module source %s@%s: %w: %s", source, to, err, strings.TrimSpace(string(outputBytes)))
	}
	tagRef := "refs/tags/" + to
	verifyTag := exec.CommandContext(ctx, "git", "-C", checkout, "show-ref", "--verify", "--quiet", tagRef)
	if err := verifyTag.Run(); err != nil {
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

func writeModuleSourceLock(path string, lock moduleSourceLock) error {
	payload, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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

func printModuleSyncPlan(module string, plan integrity.BaseSyncPlan, applying bool, lock *moduleSourceLock) {
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
	groups := []struct {
		label string
		paths []string
	}{
		{"CREATE", plan.Create}, {"UPDATE", plan.Update}, {"REMOVE", plan.Remove},
		{"RELEASED TO OVERLAY OWNERSHIP", plan.Released},
		{"RESOLVE FROM UPSTREAM", plan.ResolveUpstream},
		{"ALREADY RECONCILED FROM UPSTREAM", plan.ReconciledUpstream},
		{"INVALID SOURCE PATHS", plan.SourceInvalid}, {"INVALID TARGET PATHS", plan.TargetInvalid},
		{"MODIFIED BASE", plan.Modified}, {"OVERLAY COLLISIONS", plan.Collisions},
		{"MODIFIED UPSTREAM DELETIONS", plan.StaleModified}, {"MISSING REQUIRED OVERLAYS", plan.MissingRequiredAdditions},
		{"INVALID REQUIRED OVERLAYS", plan.InvalidRequiredAdditions},
	}
	for _, group := range groups {
		if len(group.paths) == 0 {
			continue
		}
		sort.Strings(group.paths)
		output.Info("  %s (%d): %s", group.label, len(group.paths), strings.Join(group.paths, ", "))
	}
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
