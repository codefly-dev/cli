package composition

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// NearestOverlayDir answers what core's LoadLocalOverlay loads, so it stops
// where core stops: at the repository root. An overlay a parent directory of
// the checkout carries is another checkout's, and neither `run` nor `doctor`
// may write to it or read its resolutions.
func TestTheNearestOverlayIsNeverAboveTheRepository(t *testing.T) {
	outer := t.TempDir()
	repository := filepath.Join(outer, "repository")
	workspace := filepath.Join(repository, "solutions", "notes")
	require.NoError(t, os.MkdirAll(filepath.Join(repository, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outer, resources.LocalOverlayConfigurationName), []byte("resolve: {}\n"), 0o600))

	require.Empty(t, NearestOverlayDir(workspace), "an overlay above the repository was chosen")

	require.NoError(t, os.WriteFile(filepath.Join(repository, resources.LocalOverlayConfigurationName), []byte("resolve: {}\n"), 0o600))
	require.Equal(t, repository, NearestOverlayDir(workspace), "the repository's own overlay is the workspace's")
	overlay, err := resources.LoadLocalOverlay(t.Context(), workspace)
	require.NoError(t, err)
	require.NotNil(t, overlay, "core loads the same one")
}
