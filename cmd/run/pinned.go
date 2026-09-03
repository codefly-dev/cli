package run

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
)

// materializePinnedModules pulls every composed module that resolves to a pinned
// artifact into the local module cache and points the workspace's codefly.local.yaml
// overlay at that cache directory. Core classifies a `source@version` reference as
// pinned but refuses to load it (there is no module-artifact store); this is the
// CLI-side resolve the classification defers to. Once the overlay names a local
// path, the subsequent `run service` reload loads the module as an ordinary local
// checkout, so composing a solution no longer requires a manual checkout + overlay
// entry per dependency.
//
// Only pinned identities are managed: a module the user is actively editing (a
// committed path, or an overlay `path`/`worktree` pointing outside the cache) is
// left untouched. The cache is version-keyed, so a bumped committed version pulls
// the new tag and repoints the overlay on the next run.
func materializePinnedModules(ctx context.Context, workspace *resources.Workspace) error {
	overlayPath := filepath.Join(workspace.Dir(), resources.LocalOverlayConfigurationName)
	overlay := &resources.LocalOverlay{Resolve: map[string]*resources.ModuleResolveDirective{}}
	if _, err := os.Stat(overlayPath); err == nil {
		loaded, err := resources.LoadLocalOverlay(ctx, workspace.Dir())
		if err != nil {
			return fmt.Errorf("cannot load local overlay: %w", err)
		}
		if loaded != nil && loaded.Resolve != nil {
			overlay = loaded
		}
	}
	if overlay.Resolve == nil {
		overlay.Resolve = map[string]*resources.ModuleResolveDirective{}
	}

	cacheRoot := pinnedModuleCacheRoot()
	changed := false
	for _, ref := range workspace.Modules {
		if !pinnedManaged(ref, overlay.Resolve[ref.Name], cacheRoot) {
			continue
		}
		dir, err := ensurePinnedArtifact(ctx, ref, cacheRoot)
		if err != nil {
			return fmt.Errorf("cannot pull pinned module <%s>: %w", ref.Name, err)
		}
		if existing := overlay.Resolve[ref.Name]; existing == nil || existing.Path != dir || existing.Worktree != "" || existing.Pinned {
			overlay.Resolve[ref.Name] = &resources.ModuleResolveDirective{Path: dir}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := resources.SaveLocalOverlay(ctx, workspace.Dir(), overlay); err != nil {
		return fmt.Errorf("cannot save local overlay: %w", err)
	}
	if err := ensurePinnedOverlayIgnored(workspace.Dir()); err != nil {
		return fmt.Errorf("cannot gitignore %s: %w", resources.LocalOverlayConfigurationName, err)
	}
	return nil
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
func ensurePinnedArtifact(ctx context.Context, ref *resources.ModuleReference, cacheRoot string) (string, error) {
	url := pinnedSourceURL(ref.Source)
	tag, err := resolvePinnedTag(ctx, url, ref.Version)
	if err != nil {
		return "", err
	}
	checkout := filepath.Join(cacheRoot, filepath.FromSlash(ref.Source), tag)
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
	tmp, err := os.MkdirTemp(cacheRoot, ".pull-*")
	if err != nil {
		return fmt.Errorf("create clone directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	cli.Info("pulling pinned module <%s> from %s@%s", ref.Name, ref.Source, tag)
	clone := exec.CommandContext(ctx, "git", "clone", "--quiet", "--depth", "1", "--branch", tag, url, tmp)
	if out, err := clone.CombinedOutput(); err != nil {
		return fmt.Errorf("clone %s@%s: %w: %s", url, tag, err, strings.TrimSpace(string(out)))
	}
	verify := exec.CommandContext(ctx, "git", "-C", tmp, "show-ref", "--verify", "--quiet", "refs/tags/"+tag)
	if err := verify.Run(); err != nil {
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

// resolvePinnedTag maps a committed version constraint to a concrete tag. A
// "latest" (or empty) constraint resolves to the highest published semver tag;
// an explicit version is used as its tag, gaining the conventional "v" prefix
// when it is a bare semver.
func resolvePinnedTag(ctx context.Context, url, version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" || version == "latest" {
		return highestRemoteTag(ctx, url)
	}
	if !strings.HasPrefix(version, "v") {
		if _, err := semver.NewVersion(version); err == nil {
			return "v" + version, nil
		}
	}
	return version, nil
}

// highestRemoteTag returns the highest semver tag published on url.
func highestRemoteTag(ctx context.Context, url string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "ls-remote", "--tags", "--refs", url).Output()
	if err != nil {
		return "", fmt.Errorf("list tags of %s: %w", url, err)
	}
	type tagged struct {
		tag string
		ver *semver.Version
	}
	var tags []tagged
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		tag := strings.TrimPrefix(fields[1], "refs/tags/")
		ver, err := semver.NewVersion(strings.TrimPrefix(tag, "v"))
		if err != nil {
			continue
		}
		tags = append(tags, tagged{tag: tag, ver: ver})
	}
	if len(tags) == 0 {
		return "", fmt.Errorf("no semver tags published on %s", url)
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].ver.LessThan(tags[j].ver) })
	return tags[len(tags)-1].tag, nil
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
