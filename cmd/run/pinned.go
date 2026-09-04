package run

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/pkg/cli"
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
// left untouched. The cache is version-keyed, so a bumped committed version pulls
// the new tag and repoints the overlay on the next run.
//
// A module whose artifact cannot be pulled (unreachable repo, missing tag, no
// credentials) is not fatal here: its overlay entry is simply left unwritten and
// a warning is surfaced. If the run actually needs it, core reports the precise
// pinned-load error when the dependency graph loads it; if it does not, the run
// proceeds — a single broken composed module never blocks an otherwise-bootable
// solution.
func materializePinnedModules(ctx context.Context, workspace *resources.Workspace) error {
	// Resolve against the same overlay core will use: LoadLocalOverlay searches
	// upward, so a directive in an ancestor codefly.local.yaml (the shared-monorepo
	// layout) is honored here instead of being silently shadowed by a fresh
	// workspace-local file. Writes go back to that same file so its other entries
	// are preserved.
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
	writeDir := workspace.Dir()
	if dir := nearestOverlayDir(workspace.Dir()); dir != "" {
		writeDir = dir
	}

	cacheRoot := pinnedModuleCacheRoot()
	changed := false
	for _, ref := range workspace.Modules {
		if !pinnedManaged(ref, overlay.Resolve[ref.Name], cacheRoot) {
			continue
		}
		dir, err := ensurePinnedArtifact(ctx, ref, cacheRoot)
		if err != nil {
			cli.Warning("cannot pull pinned module <%s>: %v (it will be resolved when the run loads it, if needed)", ref.Name, err)
			continue
		}
		if existing := overlay.Resolve[ref.Name]; existing == nil || existing.Path != dir || existing.Pinned {
			overlay.Resolve[ref.Name] = &resources.ModuleResolveDirective{Path: dir}
			changed = true
		}
	}
	if pruneStalePinnedEntries(overlay.Resolve, workspace.Modules, cacheRoot) {
		changed = true
	}
	if !changed {
		return nil
	}
	if err := resources.SaveLocalOverlay(ctx, writeDir, overlay); err != nil {
		return fmt.Errorf("cannot save local overlay: %w", err)
	}
	if err := ensurePinnedOverlayIgnored(writeDir); err != nil {
		return fmt.Errorf("cannot gitignore %s: %w", resources.LocalOverlayConfigurationName, err)
	}
	return nil
}

// pruneStalePinnedEntries drops auto-managed cache entries whose module is no
// longer composed, so a removed dependency does not leave a dangling overlay
// pointer at a stale checkout. Only entries the CLI itself wrote (a path under the
// cache) are removed; user directives are never touched. Reports whether it
// changed the map.
func pruneStalePinnedEntries(resolve map[string]*resources.ModuleResolveDirective, modules []*resources.ModuleReference, cacheRoot string) bool {
	present := make(map[string]bool, len(modules))
	for _, ref := range modules {
		present[ref.Name] = true
	}
	changed := false
	for name, directive := range resolve {
		if present[name] {
			continue
		}
		if directive != nil && directive.Path != "" && underDir(cacheRoot, directive.Path) {
			delete(resolve, name)
			changed = true
		}
	}
	return changed
}

// pinnedManaged reports whether the CLI should resolve ref by pulling its pinned
// artifact. A reference is managed when it carries a committed identity (source)
// and the user has not overridden its location: no committed path, and either no
// overlay directive, an explicit `pinned: true`, or a `path` the CLI itself wrote
// into the cache (which it refreshes). A user's own `path`/`worktree` directive —
// the "I am editing this module" case — is left alone.
func pinnedManaged(ref *resources.ModuleReference, directive *resources.ModuleResolveDirective, cacheRoot string) bool {
	if ref.Source == "" || ref.PathOverride != nil {
		return false
	}
	if directive == nil || directive.Pinned {
		return true
	}
	if directive.Worktree != "" {
		return false
	}
	return directive.Path != "" && underDir(cacheRoot, directive.Path)
}

// ensurePinnedArtifact resolves ref's version to an immutable tag, pulls the
// artifact into the version-keyed cache if it is not already there, and returns
// the module directory (the checkout, joined with the optional module subpath).
// A concrete version consults only the cache — no network — when the checkout is
// already present, so a cached solution boots offline.
func ensurePinnedArtifact(ctx context.Context, ref *resources.ModuleReference, cacheRoot string) (string, error) {
	url := pinnedSourceURL(ref.Source)
	sourceCache := filepath.Join(cacheRoot, filepath.FromSlash(ref.Source))
	tag, err := resolvePinnedTag(ctx, url, ref.Version, sourceCache)
	if err != nil {
		return "", err
	}
	checkout := filepath.Join(sourceCache, tag)
	if !dirPopulated(checkout) {
		if err := clonePinnedArtifact(ctx, url, tag, cacheRoot, checkout, ref); err != nil {
			return "", err
		}
	}
	dir := checkout
	if ref.Module != "" {
		dir = filepath.Join(dir, filepath.FromSlash(ref.Module))
	}
	if !dirPopulated(dir) {
		return "", fmt.Errorf("pulled %s@%s but module subpath %q is missing", ref.Source, tag, ref.Module)
	}
	return dir, nil
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
// tag when constraint is nil, the `latest` case), degrading to the highest
// satisfying tag in the local cache when the remote is unreachable. label names
// the requested constraint for a clear "nothing matches" error.
func highestTag(ctx context.Context, url, sourceCache string, constraint *semver.Constraints, label string) (string, error) {
	tag, err := highestRemoteTag(ctx, url, constraint, label)
	if err != nil {
		if cached := highestSemverTag(cachedTags(sourceCache), constraint); cached != "" {
			return cached, nil
		}
		return "", err
	}
	return tag, nil
}

// highestRemoteTag returns the highest published tag on url satisfying constraint
// (any tag when constraint is nil), preferring a stable release over a
// pre-release.
func highestRemoteTag(ctx context.Context, url string, constraint *semver.Constraints, label string) (string, error) {
	out, err := gitCommand(ctx, "ls-remote", "--tags", "--refs", url).Output()
	if err != nil {
		return "", fmt.Errorf("list tags of %s: %w", url, err)
	}
	var tags []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		tags = append(tags, strings.TrimPrefix(fields[1], "refs/tags/"))
	}
	tag := highestSemverTag(tags, constraint)
	if tag == "" {
		if constraint != nil {
			return "", fmt.Errorf("no published tag on %s satisfies %q", url, label)
		}
		return "", fmt.Errorf("no semver tags published on %s", url)
	}
	return tag, nil
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

// pinnedSourceURL turns a committed source identity into a clonable git URL. A
// bare "owner/repo" slug (how `add module` records identity) becomes a GitHub
// HTTPS URL; a source that is already a URL is used verbatim.
func pinnedSourceURL(source string) string {
	if strings.Contains(source, "://") || strings.Contains(source, "@") {
		return source
	}
	return "https://github.com/" + source + ".git"
}

func pinnedModuleCacheRoot() string {
	return filepath.Join(resources.CodeflyHomeDir(), "modules")
}

// nearestOverlayDir returns the directory of the codefly.local.yaml core would
// load from start (searching upward, nearest first), or "" when none exists. It
// mirrors resources.LoadLocalOverlay's search so a write lands in the file core
// reads rather than a shadowing new one.
func nearestOverlayDir(start string) string {
	cur := start
	for {
		if _, err := os.Stat(filepath.Join(cur, resources.LocalOverlayConfigurationName)); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// underDir reports whether path is dir itself or nested inside it.
func underDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func dirPopulated(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// ensurePinnedOverlayIgnored keeps the machine-local overlay out of git so the
// auto-managed cache pointers never show up in `git status`.
func ensurePinnedOverlayIgnored(dir string) error {
	gitignore := filepath.Join(dir, ".gitignore")
	content, err := os.ReadFile(gitignore)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == resources.LocalOverlayConfigurationName {
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
	_, err = f.WriteString(prefix + resources.LocalOverlayConfigurationName + "\n")
	return err
}
