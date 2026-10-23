package solutionrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func frontendModule(t *testing.T, files map[string]string) *resources.Module {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600))
	}
	module := &resources.Module{Name: "shared", ServiceEntry: "api"}
	module.WithDir(dir)
	return module
}

// The npm project a frontend build runs in is the remote's own directory, or
// the component root when only that holds one; neither is refused by name.
func TestFrontendProjectIsTheRemoteThenTheComponentRoot(t *testing.T) {
	both := frontendModule(t, map[string]string{"package.json": "{}", "services/fe-remote/package.json": "{}"})
	project, err := FrontendProject(both)
	require.NoError(t, err)
	require.Equal(t, "services/fe-remote", project)

	root := frontendModule(t, map[string]string{"package.json": "{}", "services/fe-remote/src/index.ts": ""})
	project, err = FrontendProject(root)
	require.NoError(t, err)
	require.Equal(t, ".", project)

	_, err = FrontendProject(frontendModule(t, map[string]string{"services/api/main.go": ""}))
	require.ErrorContains(t, err, "no npm project")
}

// The build runs in the pinned builder on a copy of the component, exports
// exactly the bundle and writes nothing into the component's directory.
// Needs Docker and network access to the npm registry's empty install.
func TestFrontendBundleBuildsInTheBuilderAndLeavesTheSourceUntouched(t *testing.T) {
	if os.Getenv("CODEFLY_FRONTEND_BUILD_QUALIFY") != "1" {
		t.Skip("set CODEFLY_FRONTEND_BUILD_QUALIFY=1 to build in Docker")
	}
	module := frontendModule(t, map[string]string{
		"services/fe-remote/package.json":      `{"name":"remote","version":"1.0.0","private":true,"scripts":{"build":"mkdir -p dist && echo {} > dist/mf-manifest.json && echo x > dist/remoteEntry.js"}}`,
		"services/fe-remote/package-lock.json": `{"name":"remote","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"remote","version":"1.0.0"}}}`,
	})
	destination := filepath.Join(t.TempDir(), "bundle")
	require.NoError(t, BuildFrontendBundle(t.Context(), module, destination, ""))
	entries, err := os.ReadDir(destination)
	require.NoError(t, err)
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.ElementsMatch(t, []string{FrontendBundleManifest, "remoteEntry.js"}, names)
	_, err = os.Stat(filepath.Join(module.ServicesDir(), "fe-remote", "dist"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorContains(t, BuildFrontendBundle(t.Context(), module, destination, ""), "must not exist")
}

// gitRepository commits files into a fresh repository and returns its root.
func gitRepository(t *testing.T, tracked map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, contents := range tracked {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600))
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", "c"}} {
		output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		require.NoError(t, err, string(output))
	}
	return root
}

// A component in a repository builds from the repository's committed tree:
// a package beside it is there, while an ignored secret, an untracked file
// and anything under node_modules never reach the builder. A component the
// repository does not track is refused; one in no repository builds from its
// own directory.
func TestFrontendBuildContextIsTheCommittedRepositoryTree(t *testing.T) {
	root := gitRepository(t, map[string]string{
		".gitignore":                   "*.secret.env\nnode_modules/\n",
		"packages/shared/package.json": "{}",
		"solutions/wiki/services/fe-remote/package.json": "{}",
	})
	for name, contents := range map[string]string{
		"configurations/staging/identity.secret.env":                "TOKEN=canary",
		"solutions/wiki/services/fe-remote/node_modules/x/index.js": "x",
		"solutions/wiki/untracked.txt":                              "x",
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600))
	}
	module := &resources.Module{Name: "wiki"}
	module.WithDir(filepath.Join(root, "solutions", "wiki"))
	context, component, err := frontendBuildContext(t.Context(), module, t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "solutions/wiki", component)
	require.FileExists(t, filepath.Join(context, "packages", "shared", "package.json"))
	require.FileExists(t, filepath.Join(context, "solutions", "wiki", "services", "fe-remote", "package.json"))
	for _, absent := range []string{"configurations/staging/identity.secret.env", "solutions/wiki/services/fe-remote/node_modules", "solutions/wiki/untracked.txt", ".git"} {
		require.NoFileExists(t, filepath.Join(context, absent))
		require.NoDirExists(t, filepath.Join(context, absent))
	}

	untracked := &resources.Module{Name: "cached"}
	untracked.WithDir(filepath.Join(root, ".codefly", "cache", "modules", "digest"))
	require.NoError(t, os.MkdirAll(untracked.Dir(), 0o755))
	_, _, err = frontendBuildContext(t.Context(), untracked, t.TempDir())
	require.ErrorContains(t, err, "not tracked at HEAD")

	packaged := frontendModule(t, map[string]string{"services/fe-remote/package.json": "{}"})
	context, component, err = frontendBuildContext(t.Context(), packaged, t.TempDir())
	require.NoError(t, err)
	require.Equal(t, packaged.Dir(), context)
	require.Equal(t, ".", component)
}

// A path a generated Dockerfile would have to quote is refused before any
// build starts.
func TestFrontendBuildRefusesAPathItCannotNameSafely(t *testing.T) {
	root := gitRepository(t, map[string]string{"solutions/my wiki/services/fe-remote/package.json": "{}"})
	module := &resources.Module{Name: "wiki"}
	module.WithDir(filepath.Join(root, "solutions", "my wiki"))
	err := BuildFrontendBundle(t.Context(), module, filepath.Join(t.TempDir(), "bundle"), "")
	require.ErrorContains(t, err, "a build path may not carry")
}

func TestFrontendBuildUsesTheSelectedBuilder(t *testing.T) {
	module := frontendModule(t, map[string]string{"package.json": "{}"})
	tools := t.TempDir()
	arguments := filepath.Join(tools, "arguments")
	t.Setenv("FRONTEND_TEST_ARGUMENTS", arguments)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, os.WriteFile(filepath.Join(tools, "docker"), []byte(`#!/bin/sh
set -eu
printf '%s\n' "$@" > "$FRONTEND_TEST_ARGUMENTS"
cat > /dev/null
for argument in "$@"; do
  case "$argument" in
    type=local,dest=*) destination="${argument#type=local,dest=}" ;;
  esac
done
mkdir -p "$destination"
printf '{}' > "$destination/mf-manifest.json"
`), 0o700))
	require.NoError(t, BuildFrontendBundle(t.Context(), module, filepath.Join(t.TempDir(), "bundle"), "mirror-bound-builder"))
	got, err := os.ReadFile(arguments)
	require.NoError(t, err)
	args := strings.Split(strings.TrimSpace(string(got)), "\n")
	require.Contains(t, args, "--builder")
	for index, argument := range args {
		if argument == "--builder" {
			require.Equal(t, "mirror-bound-builder", args[index+1])
		}
	}
}
