package run

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/resources"
)

// materializePinnedModules pulls every composed module that resolves to a pinned
// artifact into the local module cache and points the overlay core loads at that
// cache directory. Core classifies a `source@version` reference as pinned but
// refuses to load it (there is no module-artifact store); this is the CLI-side
// resolve the classification defers to. Once the overlay names a local path, the
// subsequent `run service` reload loads the module as an ordinary local checkout,
// so composing a solution no longer requires a manual checkout + overlay entry
// per dependency.
//
// Only pinned identities are managed: a module the user is actively editing (a
// committed path, or an overlay `path`/`worktree` pointing outside the cache) is
// left untouched. Everything the CLI itself materializes is recorded as a
// receipt in the composition.ResolutionRecordName sidecar, which is what keeps
// machine output distinguishable from a user checkout — including the `git:
// true` escape hatch, whose directive an overlay entry cannot hold beside the
// path it produced (core admits exactly one of path/worktree/pinned/git per
// entry), and which an explicit `pinned: true` later revokes.
//
// A module whose artifact cannot be pulled is handled by what the overlay would
// otherwise keep selecting. When the CLI has never materialized it, nothing
// stale exists: the entry is left unwritten with a warning, and core reports the
// precise pinned-load error if the run actually needs it — a single unpullable
// composed module never blocks an otherwise-bootable solution. When a previous
// run *did* materialize it, that path answers the old request, not this one:
// re-running the old checkout under a request it was never resolved for is how a
// failed upgrade silently keeps shipping the previous version. So the path is
// dropped from the overlay and materialization fails closed, naming the
// requested version and the one that path resolved to.
func materializePinnedModules(ctx context.Context, workspace *resources.Workspace) error {
	writeDir := workspace.Dir()
	if dir := composition.NearestOverlayDir(workspace.Dir()); dir != "" {
		writeDir = dir
	}
	// The whole read-decide-write cycle below (load the overlay, decide what
	// changed, save it back) must run as one critical section: two concurrent
	// `codefly run` processes targeting the same overlay file (e.g. `run
	// service` and `run job` in one workspace) would otherwise each load a
	// stale snapshot, compute their own full desired overlay.Resolve map, and
	// the second writer's full-map write would silently clobber whatever the
	// first writer had just added. A cross-process lock keyed on writeDir
	// serializes that cycle instead.
	lockPath := filepath.Join(resources.CodeflyHomeDir(), "locks", fmt.Sprintf("%x.overlay.lock", sha256.Sum256([]byte(filepath.Clean(writeDir)))))
	return composition.WithFileLock(lockPath, 5*time.Minute, func() error {
		return materializePinnedModulesLocked(ctx, workspace, writeDir)
	})
}

// materializePinnedModulesLocked is materializePinnedModules' body, run while
// composition.WithFileLock holds the overlay lock for writeDir.
func materializePinnedModulesLocked(ctx context.Context, workspace *resources.Workspace, writeDir string) error {
	// Resolve against the same overlay core will use: LoadLocalOverlay searches
	// upward, so a directive in an ancestor codefly.local.yaml (the shared-monorepo
	// layout) is honored here instead of being silently shadowed by a fresh
	// workspace-local file. Writes go back to that same file so its other entries
	// are preserved. Reading it fresh here (rather than reusing a snapshot read
	// before the lock was acquired) is what makes the lock actually prevent lost
	// updates instead of merely serializing writes of stale data.
	overlay, err := resources.LoadLocalOverlay(ctx, workspace.Dir())
	if err != nil {
		return fmt.Errorf("cannot load local overlay: %w", err)
	}
	if overlay == nil {
		overlay = &resources.LocalOverlay{}
	}
	if overlay.Resolve == nil {
		overlay.Resolve = map[string]*resources.ModuleResolveDirective{}
	}
	// What the CLI has already materialized, recorded beside the overlay because
	// an overlay entry cannot say so once the resolved path has replaced the
	// directive that selected it. A malformed record is fatal rather than read as
	// empty: treating it as empty would silently resolve a module the user opted
	// out of verification for through the verified package instead, and would
	// reclassify every machine path as a checkout the user manages.
	receipts, err := composition.LoadResolutionReceipts(writeDir)
	if err != nil {
		return fmt.Errorf("cannot load %s: %w", composition.ResolutionRecordName, err)
	}
	cacheRoot := pinnedModuleCacheRoot()
	verifiedCacheRoot := verifiedPinnedModuleCacheRoot(workspace.Dir())
	changed := false
	recorded := false
	var unresolved []error
	for _, ref := range workspace.Modules {
		directive := overlay.Resolve[ref.Name]
		receipt := receipts[ref.Name]
		if !pinnedManaged(ref, directive, receipt.ResolvedPath(), cacheRoot, verifiedCacheRoot) {
			continue
		}
		mode := composition.ResolutionModeFor(directive, receipt)
		gitFallback := mode == composition.ResolutionModeGit
		if receipt != nil && receipt.Mode != mode {
			// The user changed how this module is materialized — an explicit
			// `pinned: true` revoking a git opt-out, or a `git: true` opting out
			// of verification. Drop the receipt now, not after a successful pull:
			// the point of saying so is that the new mode is attempted from here
			// on, even while it still fails.
			delete(receipts, ref.Name)
			receipt = nil
			recorded = true
		}
		resolved, err := resolvePinnedModule(ctx, workspace.Dir(), ref, cacheRoot, gitFallback)
		if err != nil {
			if directive == nil || directive.Path == "" {
				// Nothing was materialized for this module, so nothing stale can be
				// selected: leave it unresolved for core to report if the run needs it.
				cli.Warning("cannot pull pinned module <%s>: %v (it will be resolved when the run loads it, if needed)", ref.Name, err)
				continue
			}
			unresolved = append(unresolved, staleResolutionError(ref, receipt, directive.Path, err))
			// The overlay is left naming a strategy rather than a location, so the
			// module is unresolved instead of resolved-to-the-previous-answer. The
			// git opt-out is restored rather than dropped: without it the module
			// would silently return to verified resolution on the next run.
			if gitFallback {
				overlay.Resolve[ref.Name] = &resources.ModuleResolveDirective{Git: true}
			} else {
				delete(overlay.Resolve, ref.Name)
			}
			delete(receipts, ref.Name)
			changed = true
			recorded = true
			continue
		}
		if updateReceipt(receipts, ref.Name, &composition.ResolutionReceipt{
			Source: ref.Source, Module: ref.Module, Requested: ref.Version,
			Mode: mode, Version: resolved.version, Path: resolved.dir,
			Digest: resolved.digest, Commit: resolved.commit,
		}) {
			recorded = true
		}
		// The resolved location *replaces* whatever selected the module: core
		// requires an overlay entry to select exactly one of
		// path/worktree/pinned/git, so leaving the original `git: true` next to
		// the path it produced would make the entry un-loadable on the next run.
		entry := resources.ModuleResolveDirective{Path: resolved.dir}
		if directive == nil || *directive != entry {
			if directive != nil && directive.Git {
				// The user wrote this entry by hand; say that it is being consumed
				// rather than let them discover the rewrite as a surprise diff.
				cli.Info("module <%s> resolves to its git clone at %s; `git: true` is now recorded in %s", ref.Name, resolved.dir, composition.ResolutionRecordName)
			}
			overlay.Resolve[ref.Name] = &entry
			changed = true
		}
	}
	if pruneStalePinnedEntries(overlay.Resolve, workspace.Modules, receipts, cacheRoot, verifiedCacheRoot) {
		changed = true
	}
	if pruneStaleReceipts(receipts, workspace.Modules) {
		recorded = true
	}
	// The record is written before the overlay, and both under the same lock: a
	// crash between them leaves a receipt naming a materialization the overlay has
	// not adopted yet, which the next run simply redoes. The reverse order would
	// leave an overlay path with no receipt — machine output the CLI would then
	// mistake for a checkout the user manages, and never refresh again.
	if recorded {
		if err := composition.SaveResolutionReceipts(ctx, writeDir, receipts); err != nil {
			return fmt.Errorf("cannot save %s: %w", composition.ResolutionRecordName, err)
		}
		if err := ensureIgnored(writeDir, composition.ResolutionRecordName); err != nil {
			return fmt.Errorf("cannot gitignore %s: %w", composition.ResolutionRecordName, err)
		}
	}
	if changed {
		if err := resources.SaveLocalOverlay(ctx, writeDir, overlay); err != nil {
			return fmt.Errorf("cannot save local overlay: %w", err)
		}
		if err := ensureIgnored(writeDir, resources.LocalOverlayConfigurationName); err != nil {
			return fmt.Errorf("cannot gitignore %s: %w", resources.LocalOverlayConfigurationName, err)
		}
	}
	// Reported only once the invalidated overlay has been persisted: a run that
	// fails closed must leave the stale path gone on disk, not just in memory.
	return errors.Join(unresolved...)
}

// staleResolutionError reports that ref cannot be resolved as requested while a
// previous materialization is still named by the overlay. It names both sides —
// what is requested now and what the path it drops resolved to — because the
// whole failure is that those two disagree and the second was being run as if it
// answered the first.
func staleResolutionError(ref *resources.ModuleReference, receipt *composition.ResolutionReceipt, path string, err error) error {
	previous := "materialization at " + path
	if receipt != nil && receipt.Version != "" {
		previous = fmt.Sprintf("resolved version %s at %s", receipt.Version, path)
	}
	return fmt.Errorf("module <%s>: cannot resolve requested version %s: %w; its previously %s no longer answers that request and has been dropped from %s",
		ref.Name, requestedVersionLabel(ref.Version), err, previous, resources.LocalOverlayConfigurationName)
}

func requestedVersionLabel(version string) string {
	if strings.TrimSpace(version) == "" {
		return "latest"
	}
	return version
}

// updateReceipt stores the receipt for name, reporting whether it differs from
// the one already recorded — an unchanged receipt must not dirty the sidecar, so
// a steady-state run rewrites nothing.
func updateReceipt(receipts map[string]*composition.ResolutionReceipt, name string, receipt *composition.ResolutionReceipt) bool {
	if previous, ok := receipts[name]; ok && *previous == *receipt {
		return false
	}
	receipts[name] = receipt
	return true
}

// pruneStaleReceipts drops receipts for modules that are no longer composed, so
// a removed dependency does not silently re-enter unverified resolution if it is
// composed again later. Reports whether it changed the map.
func pruneStaleReceipts(receipts map[string]*composition.ResolutionReceipt, modules []*resources.ModuleReference) bool {
	present := make(map[string]bool, len(modules))
	for _, ref := range modules {
		present[ref.Name] = true
	}
	changed := false
	for name := range receipts {
		if !present[name] {
			delete(receipts, name)
			changed = true
		}
	}
	return changed
}

// materialization is what one pinned reference resolved to, in the terms a
// receipt records: the module directory plus the exact version (and, when the
// module package was verified, the digest and commit) behind it.
type materialization struct {
	dir     string
	version string
	digest  string
	commit  string
}

// resolvePinnedModule resolves ref to a materialization: through the verified
// module package (composition.ResolvePinnedModule) by default, or through the
// unverified git clone when the workspace has opted this module out via
// `resolve.<name>.git: true`.
func resolvePinnedModule(ctx context.Context, workspaceDir string, ref *resources.ModuleReference, cacheRoot string, gitFallback bool) (*materialization, error) {
	if gitFallback {
		cli.Warning("unverified git clone for %s", ref.Name)
		dir, tag, err := ensurePinnedArtifact(ctx, ref, cacheRoot)
		if err != nil {
			return nil, err
		}
		return &materialization{dir: dir, version: tag}, nil
	}
	resolved, err := composition.ResolvePinnedModule(ctx, workspaceDir, ref)
	if err != nil {
		return nil, err
	}
	return &materialization{dir: resolved.Dir, version: resolved.Version, digest: resolved.Digest, commit: resolved.Commit}, nil
}

// verifiedPinnedModuleCacheRoot is the workspace-scoped, content-addressed
// module cache composition.NewMaterializer(workspaceDir) writes into. Cache
// roots are workspace-scoped (a workspace-relative digest tree), while the
// git-clone fallback's cache root is process-global (pinnedModuleCacheRoot);
// both are recognized when deciding whether an overlay path is CLI-managed.
func verifiedPinnedModuleCacheRoot(workspaceDir string) string {
	return filepath.Join(workspaceDir, ".codefly", "cache", "modules")
}

// pruneStalePinnedEntries drops auto-managed cache entries whose module is no
// longer composed, so a removed dependency does not leave a dangling overlay
// pointer at a stale checkout. Only entries the CLI itself wrote are removed —
// a path under a cache root, or one matching the path on that module's receipt;
// user directives are never touched. Reports whether it changed the map.
func pruneStalePinnedEntries(resolve map[string]*resources.ModuleResolveDirective, modules []*resources.ModuleReference, receipts map[string]*composition.ResolutionReceipt, cacheRoots ...string) bool {
	present := make(map[string]bool, len(modules))
	for _, ref := range modules {
		present[ref.Name] = true
	}
	changed := false
	for name, directive := range resolve {
		if present[name] || directive == nil || directive.Path == "" {
			continue
		}
		if directive.Path == receipts[name].ResolvedPath() || underAnyDir(cacheRoots, directive.Path) {
			delete(resolve, name)
			changed = true
		}
	}
	return changed
}

// pinnedManaged reports whether the CLI should resolve ref by pulling its pinned
// artifact. A reference is managed when it carries a committed identity (source)
// and the user has not overridden its location: no committed path, and either no
// overlay directive, an explicit `pinned: true` or `git: true` (both name a
// resolution strategy, not a location), or a `path` the CLI itself wrote — either
// the path on that module's receipt, or one under a cache root. A user's own
// `path`/`worktree` directive — the "I am editing this module" case — is left
// alone. recordedPath is matched exactly rather than by cache-root prefix, so a
// moved CODEFLY_HOME does not turn a materialization the CLI wrote into a
// directory it refuses to touch.
func pinnedManaged(ref *resources.ModuleReference, directive *resources.ModuleResolveDirective, recordedPath string, cacheRoots ...string) bool {
	if ref.Source == "" || ref.PathOverride != nil {
		return false
	}
	if directive == nil || directive.Pinned || directive.Git {
		return true
	}
	if directive.Worktree != "" {
		return false
	}
	if directive.Path == "" {
		return false
	}
	return directive.Path == recordedPath || underAnyDir(cacheRoots, directive.Path)
}

// ensurePinnedArtifact resolves ref's version to an immutable tag, pulls the
// artifact into the version-keyed cache if it is not already there, and returns
// the module directory (the checkout, joined with the optional module subpath)
// along with the tag it resolved to. A concrete version consults only the cache
// — no network — when the checkout is already present, so a cached solution
// boots offline.
func ensurePinnedArtifact(ctx context.Context, ref *resources.ModuleReference, cacheRoot string) (string, string, error) {
	url := composition.PinnedSourceURL(ref.Source)
	sourceCache := filepath.Join(cacheRoot, filepath.FromSlash(ref.Source))
	tag, err := resolvePinnedTag(ctx, url, ref.Version, sourceCache)
	if err != nil {
		return "", "", err
	}
	checkout := filepath.Join(sourceCache, tag)
	if !dirPopulated(checkout) {
		if err := clonePinnedArtifact(ctx, url, tag, cacheRoot, checkout, ref); err != nil {
			return "", "", err
		}
	}
	dir := checkout
	if ref.Module != "" {
		dir = filepath.Join(dir, filepath.FromSlash(ref.Module))
	}
	if !dirPopulated(dir) {
		return "", "", fmt.Errorf("pulled %s@%s but module subpath %q is missing", ref.Source, tag, ref.Module)
	}
	return dir, tag, nil
}

// clonePinnedArtifact clones url at tag into a temp directory alongside the cache
// (same filesystem, so the promotion is an atomic rename), verifies the ref is an
// immutable tag rather than a moved branch, then promotes it to checkout. A
// concurrent run that populated checkout first wins; this one discards its clone.
func clonePinnedArtifact(ctx context.Context, url, tag, cacheRoot, checkout string, ref *resources.ModuleReference) error {
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return fmt.Errorf("create module cache: %w", err)
	}
	sweepStalePulls(cacheRoot)
	tmp, err := os.MkdirTemp(cacheRoot, ".pull-*")
	if err != nil {
		return fmt.Errorf("create clone directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	cli.Info("pulling pinned module <%s> from %s@%s", ref.Name, ref.Source, tag)
	if out, err := gitCommand(ctx, "clone", "--quiet", "--depth", "1", "--branch", tag, url, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("clone %s@%s: %w: %s", url, tag, err, strings.TrimSpace(string(out)))
	}
	if err := gitCommand(ctx, "-C", tmp, "show-ref", "--verify", "--quiet", "refs/tags/"+tag).Run(); err != nil {
		return fmt.Errorf("%s@%s is not an immutable tag", ref.Source, tag)
	}
	if err := os.MkdirAll(filepath.Dir(checkout), 0o755); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}
	if err := os.Rename(tmp, checkout); err != nil {
		if dirPopulated(checkout) {
			return nil
		}
		return fmt.Errorf("promote cached module: %w", err)
	}
	return nil
}

// resolvePinnedTag maps a committed version constraint to a concrete tag. An
// explicit version is used as its tag (gaining the conventional "v" prefix when it
// is a bare semver) and never touches the network. A "latest" (or empty)
// constraint asks the remote for the highest published tag; a semver range (e.g.
// ">=0.0.49") asks for the highest published tag that satisfies it. When the
// remote is unreachable either degrades to the highest satisfying tag already in
// the local cache, so a warmed-up solution still boots offline.
//
// A range follows semver constraint semantics: a pre-release tag satisfies a
// range only when the range itself names a pre-release (e.g. ">=0.0.49-0"). This
// differs from `latest`, which selects a pre-release when no stable tag exists.
func resolvePinnedTag(ctx context.Context, url, version, sourceCache string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" || version == "latest" {
		return highestTag(ctx, url, sourceCache, nil, version)
	}
	if _, err := semver.NewVersion(strings.TrimPrefix(version, "v")); err == nil {
		if strings.HasPrefix(version, "v") {
			return version, nil
		}
		return "v" + version, nil
	}
	constraint, err := semver.NewConstraint(version)
	if err != nil {
		return version, nil
	}
	return highestTag(ctx, url, sourceCache, constraint, version)
}

// highestTag returns the highest published tag on url satisfying constraint (any
// tag when constraint is nil, the `latest` case). It degrades to the highest
// satisfying tag in the local cache only when the remote is *unreachable*, so a
// warmed-up solution still boots offline. When the remote answers but publishes
// no satisfying tag, that is a hard error the cache must not mask — otherwise a
// yanked or since-removed tag left in the cache would silently boot in place of an
// unsatisfiable range. label names the requested constraint for that error.
func highestTag(ctx context.Context, url, sourceCache string, constraint *semver.Constraints, label string) (string, error) {
	tags, err := remoteTags(ctx, url)
	if err != nil {
		if cached := highestSemverTag(cachedTags(sourceCache), constraint); cached != "" {
			return cached, nil
		}
		return "", err
	}
	if tag := highestSemverTag(tags, constraint); tag != "" {
		return tag, nil
	}
	if constraint != nil {
		return "", fmt.Errorf("no published tag on %s satisfies %q", url, label)
	}
	return "", fmt.Errorf("no semver tags published on %s", url)
}

// remoteTags lists the published tag names on url. Its error means the remote
// could not be reached (or refused): an empty-but-reachable remote returns an
// empty slice and no error, so callers can tell "unreachable" from "no such tag".
func remoteTags(ctx context.Context, url string) ([]string, error) {
	out, err := gitCommand(ctx, "ls-remote", "--tags", "--refs", url).Output()
	if err != nil {
		return nil, fmt.Errorf("list tags of %s: %w", url, err)
	}
	var tags []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		tags = append(tags, strings.TrimPrefix(fields[1], "refs/tags/"))
	}
	return tags, nil
}

// cachedTags lists the version-keyed subdirectories already present under a
// source's cache (each is a pulled tag). Transient .pull-* clone directories and
// non-tag entries are excluded by highestSemverTag's parse.
func cachedTags(sourceCache string) []string {
	entries, err := os.ReadDir(sourceCache)
	if err != nil {
		return nil
	}
	var tags []string
	for _, entry := range entries {
		if entry.IsDir() {
			tags = append(tags, entry.Name())
		}
	}
	return tags
}

// highestSemverTag returns the highest semver tag from tags that satisfies
// constraint (any tag when constraint is nil), preferring a stable release; a
// pre-release wins only when no stable tag is present. Non-semver tags are
// ignored. Returns "" when none parse or none satisfy.
func highestSemverTag(tags []string, constraint *semver.Constraints) string {
	type tagged struct {
		tag string
		ver *semver.Version
	}
	var stable, pre []tagged
	for _, tag := range tags {
		ver, err := semver.NewVersion(strings.TrimPrefix(tag, "v"))
		if err != nil {
			continue
		}
		if constraint != nil && !constraint.Check(ver) {
			continue
		}
		if ver.Prerelease() == "" {
			stable = append(stable, tagged{tag: tag, ver: ver})
		} else {
			pre = append(pre, tagged{tag: tag, ver: ver})
		}
	}
	pick := stable
	if len(pick) == 0 {
		pick = pre
	}
	if len(pick) == 0 {
		return ""
	}
	sort.Slice(pick, func(i, j int) bool { return pick[i].ver.LessThan(pick[j].ver) })
	return pick[len(pick)-1].tag
}

// sweepStalePulls removes clone directories orphaned by a hard kill (the deferred
// cleanup in clonePinnedArtifact never ran). The one-hour floor keeps the sweep
// from ever touching a concurrent run's in-progress clone.
func sweepStalePulls(cacheRoot string) {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".pull-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(cacheRoot, entry.Name()))
		}
	}
}

// gitCommand builds a git invocation with interactive credential prompting
// disabled, so a private repo the machine has no credentials for fails fast with
// a clear error instead of blocking the pre-TUI terminal on a username prompt.
// SSH-preferring setups are unaffected: git's own url.*.insteadOf rewrites still
// apply to the HTTPS URL.
func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

func pinnedModuleCacheRoot() string {
	return filepath.Join(resources.CodeflyHomeDir(), "modules")
}

// underDir reports whether path is dir itself or nested inside it.
func underDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// underAnyDir reports whether path is under any of dirs (empty entries are
// skipped). Used to recognize both the git-clone fallback's cache root and the
// verified-package materializer's workspace-scoped cache root as CLI-managed.
func underAnyDir(dirs []string, path string) bool {
	for _, dir := range dirs {
		if dir != "" && underDir(dir, path) {
			return true
		}
	}
	return false
}

func dirPopulated(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// ensureIgnored keeps a machine-local file out of git so the auto-managed cache
// pointers never show up in `git status`.
func ensureIgnored(dir, name string) error {
	gitignore := filepath.Join(dir, ".gitignore")
	content, err := os.ReadFile(gitignore)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == name {
			return nil
		}
	}
	var prefix string
	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		prefix = "\n"
	}
	f, err := os.OpenFile(gitignore, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(prefix + name + "\n")
	return err
}
