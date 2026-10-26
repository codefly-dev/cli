package solutionrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A component's built bundle is services/fe-remote/dist with mf-manifest.json
// at its root: absent is no bundle, present is the archive root, and a dist
// without the manifest is refused as no bundle.
func TestFrontendBundleIsTheBuiltDistWithItsManifest(t *testing.T) {
	module := &resources.Module{Name: "wiki", ServiceEntry: "backend"}
	module.WithDir(t.TempDir())
	root, built, err := FrontendBundle(module)
	require.NoError(t, err)
	require.False(t, built, "nothing built is no bundle")
	require.Empty(t, root)

	dist := filepath.Join(module.ServicesDir(), "fe-remote", "dist")
	require.NoError(t, os.MkdirAll(dist, 0o755))
	_, _, err = FrontendBundle(module)
	require.ErrorContains(t, err, FrontendBundleManifest)

	require.NoError(t, os.WriteFile(filepath.Join(dist, FrontendBundleManifest), []byte(`{}`), 0o600))
	root, built, err = FrontendBundle(module)
	require.NoError(t, err)
	require.True(t, built)
	require.Equal(t, dist, root)

	require.Nil(t, func() error { _, _, err := FrontendBundle(&resources.Module{Name: "bare"}); return err }(), "a module with no directory has no bundle and no error")
}
