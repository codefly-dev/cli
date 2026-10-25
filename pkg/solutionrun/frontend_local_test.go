package solutionrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A containerized local build reads what the deployed build reads — the files
// the repository tracks — with their working-tree content: local edits are
// built, untracked files never reach the builder, ignored or not, nor do this
// frontend's output, installed dependencies or a symlink elsewhere in the
// repository, which is not this frontend's input.
func TestLocalFrontendSourcesAreTheTrackedWorkingTree(t *testing.T) {
	root := gitRepository(t, map[string]string{
		".gitignore":                                              "*.secret.env\nnode_modules/\n",
		"packages/shared/dist/index.js":                           "shipped dependency",
		"packages/shared/input":                                   "before",
		"solutions/wiki/services/fe-remote/package.json":          "{}",
		"solutions/wiki/services/fe-remote/deleted.ts":            "old",
		"solutions/wiki/services/fe-remote/dist/mf-manifest.json": "tracked stale bundle",
		"CLAUDE.md": "notes",
	})
	require.NoError(t, os.Symlink("CLAUDE.md", filepath.Join(root, "AGENTS.md")))
	gitCommitAll(t, root)
	require.NoError(t, os.WriteFile(filepath.Join(root, "packages/shared/input"), []byte("local edit"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(root, "solutions/wiki/services/fe-remote/deleted.ts")))
	for name, content := range map[string]string{
		"solutions/wiki/services/fe-remote/new.ts": "untracked source",
		".env": "untracked, not ignored",
		"solutions/wiki/services/fe-remote/node_modules/package/index.js": "installed",
		"identity.secret.env": "not a build input",
	} {
		full := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	module := &resources.Module{Name: "wiki"}
	module.WithDir(filepath.Join(root, "solutions/wiki"))
	staged, component, err := localFrontendBuildContext(t.Context(), module, t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "solutions/wiki", component)
	content, err := os.ReadFile(filepath.Join(staged, "packages/shared/input"))
	require.NoError(t, err)
	require.Equal(t, "local edit", string(content))
	require.FileExists(t, filepath.Join(staged, "packages/shared/dist/index.js"), "a dependency artifact is not this frontend build's output")
	require.FileExists(t, filepath.Join(staged, "CLAUDE.md"))
	for _, path := range []string{"AGENTS.md", ".env", "identity.secret.env", ".git", "solutions/wiki/services/fe-remote/new.ts", "solutions/wiki/services/fe-remote/deleted.ts", "solutions/wiki/services/fe-remote/dist", "solutions/wiki/services/fe-remote/node_modules"} {
		_, err = os.Lstat(filepath.Join(staged, path))
		require.ErrorIs(t, err, os.ErrNotExist, path)
	}
}

// A frontend's inputs are its own project's tracked files: an untracked file
// or an edit elsewhere in the repository moves nothing, a tracked edit in the
// project does, and a tracked symlink in the project is refused by name.
func TestLocalFrontendInputsAreItsOwnTrackedProject(t *testing.T) {
	root := gitRepository(t, map[string]string{
		"solutions/wiki/services/fe-remote/package.json":      "{}",
		"solutions/wiki/services/fe-remote/package-lock.json": "{}",
		"solutions/wiki/services/fe-remote/Page.tsx":          "v1",
		"solutions/wiki/services/api/main.go":                 "package main",
	})
	module := &resources.Module{Name: "wiki"}
	module.WithDir(filepath.Join(root, "solutions/wiki"))
	project := filepath.Join(module.ServicesDir(), "fe-remote")
	inputs := func() (string, string) {
		digest, lock, err := LocalFrontend{}.inputs(t.Context(), module, project)
		require.NoError(t, err)
		return digest, lock
	}
	first, lock := inputs()
	require.NotEmpty(t, lock)
	require.NoError(t, os.WriteFile(filepath.Join(project, "untracked.ts"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(module.ServicesDir(), "api", "main.go"), []byte("package main // edited"), 0o600))
	same, _ := inputs()
	require.Equal(t, first, same)
	require.NoError(t, os.WriteFile(filepath.Join(project, "Page.tsx"), []byte("v2"), 0o600))
	edited, sameLock := inputs()
	require.NotEqual(t, first, edited)
	require.Equal(t, lock, sameLock)
	docker, _, err := LocalFrontend{Docker: true}.inputs(t.Context(), module, project)
	require.NoError(t, err)
	require.NotEqual(t, edited, docker, "how it is built is an input")

	require.NoError(t, os.Symlink("Page.tsx", filepath.Join(project, "Alias.tsx")))
	gitCommitAll(t, root)
	_, _, err = LocalFrontend{}.inputs(t.Context(), module, project)
	require.ErrorContains(t, err, "Alias.tsx is not a regular file")
}

// A recorded bundle is reused only while it is retained and its inputs hold.
func TestLocalFrontendReusesOnlyARetainedBundleOfTheSameInputs(t *testing.T) {
	module := frontendModule(t, map[string]string{
		"services/fe-remote/package.json":      "{}",
		"services/fe-remote/package-lock.json": "{}",
		"services/fe-remote/Page.tsx":          "v1",
	})
	retained := map[string]bool{}
	builds := 0
	build := LocalFrontend{
		StateDir: t.TempDir(),
		Retain: func(_ context.Context, root string) (string, error) {
			builds++
			digest := "sha256:" + filepath.Base(root) + string(rune('a'+builds))
			retained[digest] = true
			return digest, nil
		},
		Retained: func(digest string) bool { return retained[digest] },
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\nmkdir -p node_modules dist\necho '{}' > dist/mf-manifest.json\n"), 0o700))
	first, err := build.Bundle(t.Context(), module)
	require.NoError(t, err)
	again, err := build.Bundle(t.Context(), module)
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, 1, builds)
	delete(retained, first)
	_, err = build.Bundle(t.Context(), module)
	require.NoError(t, err)
	require.Equal(t, 2, builds, "a bundle no longer retained is built again")
}

// This qualification exercises BuildKit's real content cache through the
// containerized local build. Random output proves the unchanged build script
// was not rerun; a source edit must invalidate that same layer.
func TestLocalFrontendBuildReusesContentCache(t *testing.T) {
	if os.Getenv("CODEFLY_FRONTEND_BUILD_QUALIFY") != "1" {
		t.Skip("set CODEFLY_FRONTEND_BUILD_QUALIFY=1 to qualify Docker content caching")
	}
	module := frontendModule(t, map[string]string{
		"services/fe-remote/package.json":      `{"name":"remote-cache-test","version":"1.0.0","private":true,"scripts":{"build":"node build.cjs"}}`,
		"services/fe-remote/package-lock.json": `{"name":"remote-cache-test","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"remote-cache-test","version":"1.0.0"}}}`,
		"services/fe-remote/build.cjs":         `const fs=require('fs');fs.mkdirSync('dist');fs.writeFileSync('dist/mf-manifest.json',JSON.stringify({input:fs.readFileSync('input','utf8'),build:require('crypto').randomUUID()}));`,
		"services/fe-remote/input":             t.Name() + t.TempDir(),
	})
	build := func() string {
		destination := filepath.Join(t.TempDir(), "bundle")
		require.NoError(t, buildFrontendBundle(t.Context(), module, destination, localFrontendBuildContext))
		data, err := os.ReadFile(filepath.Join(destination, FrontendBundleManifest))
		require.NoError(t, err)
		return string(data)
	}
	first := build()
	require.Equal(t, first, build(), "unchanged content reuses the built layer, including its random marker")
	require.NoError(t, os.WriteFile(filepath.Join(module.ServicesDir(), "fe-remote/input"), []byte("changed local source"), 0o600))
	changed := build()
	require.NotEqual(t, first, changed)
	require.Contains(t, changed, "changed local source")
	require.NoDirExists(t, filepath.Join(module.ServicesDir(), FrontendBundleDir))
}

func gitCommitAll(t *testing.T, root string) {
	t.Helper()
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", "more")
}

func gitRun(t *testing.T, root string, args ...string) {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	require.NoError(t, err, string(output))
}
