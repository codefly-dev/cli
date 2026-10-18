package solutionrun

import (
	"os"
	"path/filepath"
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
	require.NoError(t, BuildFrontendBundle(t.Context(), module, destination))
	entries, err := os.ReadDir(destination)
	require.NoError(t, err)
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.ElementsMatch(t, []string{FrontendBundleManifest, "remoteEntry.js"}, names)
	_, err = os.Stat(filepath.Join(module.ServicesDir(), "fe-remote", "dist"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorContains(t, BuildFrontendBundle(t.Context(), module, destination), "must not exist")
}
