package solutionrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestLocalFrontendSourcesIncludeEditsAndExcludeBuildOutputs(t *testing.T) {
	root := gitRepository(t, map[string]string{
		".gitignore":                                              "*.secret.env\nnode_modules/\n",
		"packages/shared/dist/index.js":                           "shipped dependency",
		"packages/shared/input":                                   "before",
		"solutions/wiki/services/fe-remote/package.json":          "{}",
		"solutions/wiki/services/fe-remote/deleted.ts":            "old",
		"solutions/wiki/services/fe-remote/dist/mf-manifest.json": "tracked stale bundle",
	})
	require.NoError(t, os.WriteFile(filepath.Join(root, "packages/shared/input"), []byte("local edit"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(root, "solutions/wiki/services/fe-remote/deleted.ts")))
	for name, content := range map[string]string{
		"solutions/wiki/services/fe-remote/new.ts":                        "new source",
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
	require.FileExists(t, filepath.Join(staged, component, "services/fe-remote/new.ts"))
	for _, path := range []string{"identity.secret.env", ".git", "solutions/wiki/services/fe-remote/deleted.ts", "solutions/wiki/services/fe-remote/dist", "solutions/wiki/services/fe-remote/node_modules"} {
		_, err = os.Stat(filepath.Join(staged, path))
		require.ErrorIs(t, err, os.ErrNotExist, path)
	}
}

// This qualification exercises BuildKit's real content cache. Random output
// proves the unchanged build script was not rerun; an uncommitted source edit
// must invalidate that same layer. Unit gates cover the host boundary separately.
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
		require.NoError(t, BuildLocalFrontendBundle(t.Context(), module, destination))
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
