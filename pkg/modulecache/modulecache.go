// Package modulecache names where the CLI materializes the modules a
// workspace pins, and answers whether a path is one it wrote. It is a leaf so
// that both the composition that materializes modules and the builder that
// must know whether a service's code is a pinned release can read one answer.
package modulecache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// Env relocates the module cache — both the git-clone cache and the
// verified-package cache — to a directory the developer chooses, so the modules
// a workspace composes can be browsed (and deleted) where they expect them
// rather than inside CODEFLY_HOME and inside the workspace respectively. It
// must name an absolute path; a leading "~" is expanded.
const Env = "CODEFLY_MODULE_CACHE"

// VerifiedDirName is the subdirectory of a configured cache root that holds
// verified module packages. Those are content-addressed by digest (that
// addressing is what makes a cache hit checkable against what was verified),
// so they cannot share the browsable <owner>/<repo>/<version> tree the git
// clones occupy; keeping them under one reserved name is what lets both live
// under the single root the developer configured.
const VerifiedDirName = ".packages"

// Configured returns the root named by Env, or "" when it is unset. A value
// that is not usable as a root is an error rather than an ignored setting:
// silently falling back would put the modules somewhere other than where the
// developer said, and they would look for them where they asked.
func Configured() (string, error) {
	raw := strings.TrimSpace(os.Getenv(Env))
	if raw == "" {
		return "", nil
	}
	expanded := raw
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%s is %q but the home directory cannot be resolved: %w", Env, raw, err)
		}
		expanded = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/"))
	}
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("%s must be an absolute path (or start with ~), got %q", Env, raw)
	}
	return filepath.Clean(expanded), nil
}

// CloneRoot is the root of the git-clone cache: the configured root when there
// is one, otherwise the process-global default. Under either, a module lands
// at <root>/<owner>/<repo>/<tag>/ plus its optional module subpath, which is
// the layout a developer pointing Env at their own vendor directory browses.
func CloneRoot() (string, error) {
	configured, err := Configured()
	if err != nil {
		return "", err
	}
	if configured != "" {
		return configured, nil
	}
	return filepath.Join(resources.CodeflyHomeDir(), "modules"), nil
}

// VerifiedRoot is the content-addressed module cache the core Materializer
// writes into. By default it is workspace-scoped — the digest tree
// NewMaterializer(workspaceDir) picks — while the git-clone cache root is
// process-global (CloneRoot). A configured Env moves it beside the clones,
// under the one reserved subdirectory, so a workspace repository holds no
// module bytes at all.
func VerifiedRoot(workspaceDir string) (string, error) {
	configured, err := Configured()
	if err != nil {
		return "", err
	}
	if configured != "" {
		return filepath.Join(configured, VerifiedDirName), nil
	}
	return filepath.Join(workspaceDir, ".codefly", "cache", "modules"), nil
}

// Owned reports whether path lies under a cache root that holds nothing but
// what the CLI materialized for workspaceDir: a pinned module's code exactly as
// its release carries it. A root the developer configured through Env is never
// proof — it is a directory they chose and may keep their own checkouts in.
func Owned(workspaceDir, path string) (bool, error) {
	configured, err := Configured()
	if err != nil || configured != "" {
		return false, err
	}
	clone, err := CloneRoot()
	if err != nil {
		return false, err
	}
	verified, err := VerifiedRoot(workspaceDir)
	if err != nil {
		return false, err
	}
	for _, root := range []string{clone, verified} {
		if Under(root, path) {
			return true, nil
		}
	}
	return false, nil
}

// Under reports whether path is dir itself or nested inside it.
func Under(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}
