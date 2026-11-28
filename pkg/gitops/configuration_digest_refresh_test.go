package gitops

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRefreshDeliveredConfigurationDigest(t *testing.T) {
	root := t.TempDir()
	digestUnit(t, root, "before", "shop-api")
	stampedTemplates(t, root)
	tree := testRenderRoot(t, root)
	unit := filepath.Join(root, "services", "api")
	overlay := filepath.Join(unit, "overlays", "prod")
	read := func() []podTemplateAnnotation {
		manifests, err := overlayManifestsInTree(tree, overlay)
		require.NoError(t, err)
		prior, err := deliveredConfigurationDigests(manifests)
		require.NoError(t, err)
		return prior
	}
	prior := read()
	config := filepath.Join(overlay, "configmap.yaml")
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config, bytes.ReplaceAll(data, []byte("before"), []byte("after")), 0600))
	require.NoError(t, refreshConfigurationDigest(tree, unit, "prod", prior))
	updated := read()
	require.NotEqual(t, prior, updated)
	before := renderedOverlay(t, root)
	require.NoError(t, refreshConfigurationDigest(tree, unit, "prod", updated))
	require.Equal(t, before, renderedOverlay(t, root))
	require.Error(t, refreshConfigurationDigest(tree, unit, "prod", prior))
}

func TestRefreshRefusesDuplicateOwnedPatch(t *testing.T) {
	root := t.TempDir()
	digestUnit(t, root, "on", "shop-api")
	stampedTemplates(t, root)
	tree := testRenderRoot(t, root)
	unit := filepath.Join(root, "services", "api")
	overlay := filepath.Join(unit, "overlays", "prod")
	manifests, err := overlayManifestsInTree(tree, overlay)
	require.NoError(t, err)
	prior, err := deliveredConfigurationDigests(manifests)
	require.NoError(t, err)
	require.NotEmpty(t, prior)
	require.NoError(t, addOverlayPatches(tree, unit, "prod", prior[:1]))
	before := renderedOverlay(t, root)
	require.ErrorContains(t, refreshConfigurationDigest(tree, unit, "prod", prior), "duplicate configuration digest patch")
	require.Equal(t, before, renderedOverlay(t, root))
}
