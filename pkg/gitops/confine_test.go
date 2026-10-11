package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func testRenderRoot(t *testing.T, directory string) *renderRoot {
	t.Helper()
	tree, err := newRenderRoot(directory)
	require.NoError(t, err)
	return tree
}

func TestRenderRootRefusesSymlinkedServiceAncestors(t *testing.T) {
	for _, relative := range []string{"services", "services/api", "services/api/base", "services/api/overlays", "services/api/overlays/prod", "services/api/overlays/prod/kustomization.yaml"} {
		for _, operation := range []string{"patch", "configuration", "full render"} {
			t.Run(relative+"/"+operation, func(t *testing.T) {
				workspace, env := workloadIdentityComposition(t)
				scope := moduleScope(env, workspace, "app")
				var err error
				scope.WorkloadPeers, err = resolveWorkloadPeers(t.Context(), workspace, env)
				require.NoError(t, err)
				root := t.TempDir()
				tree := testRenderRoot(t, root)
				body := strings.Replace(sidecarDeployment, "metadata:\n", "metadata:\n  namespace: "+scope.Namespace+"\n", 1)
				require.NoError(t, kustomizedWorkload(body)(t.Context(), root))
				outside := t.TempDir()
				target := filepath.Join(outside, "target")
				require.NoError(t, os.Rename(filepath.Join(root, relative), target))
				require.NoError(t, os.Symlink(target, filepath.Join(root, relative)))
				before := snapshotTree(t, outside)
				service := &resources.Service{Name: "api", WorkspaceConfigurationDependencies: []string{"workload-identity"}}
				projection := podProjection{kind: kindDeployment, name: "api", container: "api", volume: projectedVolume("test"), mount: readOnlyMount("test", "/test")}
				switch operation {
				case "patch":
					// A base symlink is not read by the patch writer. Its whole subtree
					// preflight must still refuse before modifying the overlay index.
					err = addOverlayPatches(tree, filepath.Join(root, "services", "api"), env.Name, []podProjection{projection})
				case "configuration":
					err = projectServiceConfiguration(t.Context(), tree, filepath.Join(root, "services", "api"), service, env, scope, serviceInjection{})
				case "full render":
					opts := &RenderOptions{Destination: filepath.Join(t.TempDir(), "promoted"), Environment: env.Name, Namespace: scope.Namespace, Promotable: true}
					_, err = RenderOwnedTree(t.Context(), opts, func(ctx context.Context, stage string) error {
						// Agent output containing the same hostile ancestor, before the
						// real configuration projection (which runs inside generation).
						require.NoError(t, kustomizedWorkload(body)(ctx, stage))
						require.NoError(t, os.RemoveAll(filepath.Join(stage, relative)))
						require.NoError(t, os.Symlink(target, filepath.Join(stage, relative)))
						return projectServiceConfiguration(ctx, opts.renderRoot, filepath.Join(stage, "services", "api"), service, env, scope, serviceInjection{})
					})
				}
				require.ErrorContains(t, err, "symbolic link")
				require.Equal(t, before, snapshotTree(t, outside))
			})
		}
	}
}

func TestRenderRootAllowsOnlyItsOwnSymlink(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.Symlink(root, alias))
	tree := testRenderRoot(t, alias)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "nested"), 0o755))
	require.NoError(t, tree.write(alias, "nested/file", []byte("safe")))
	require.NoError(t, os.Symlink("file", filepath.Join(root, "nested", "link")))
	_, err := tree.read(alias, "nested/link")
	require.ErrorContains(t, err, "symbolic link")
	require.ErrorContains(t, tree.write(alias, "nested/link", []byte("changed")), "symbolic link")
	data, err := os.ReadFile(filepath.Join(root, "nested", "file"))
	require.NoError(t, err)
	require.Equal(t, "safe", string(data))
}

func TestOverlayWritesDoNotTruncateExternalHardlinks(t *testing.T) {
	for _, plain := range []bool{false, true} {
		t.Run(fmt.Sprint(plain), func(t *testing.T) {
			root := t.TempDir()
			var file string
			if plain {
				require.NoError(t, renderWorkload(sidecarDeployment)(t.Context(), root))
				file = filepath.Join(root, "services", "api", "overlays", "prod", "deployment.yaml")
			} else {
				require.NoError(t, kustomizedWorkload(sidecarDeployment)(t.Context(), root))
				file = filepath.Join(root, "services", "api", "overlays", "prod", kustomizationFile)
			}
			outside := t.TempDir()
			require.NoError(t, os.Link(file, filepath.Join(outside, "original")))
			before := snapshotTree(t, outside)
			projection := podProjection{kind: kindDeployment, name: "api", container: "api", volume: projectedVolume("test"), mount: readOnlyMount("test", "/test")}
			require.NoError(t, addOverlayPatches(testRenderRoot(t, root), filepath.Join(root, "services", "api"), "prod", []podProjection{projection}))
			require.Equal(t, before, snapshotTree(t, outside))
		})
	}
}

func TestResourceQuotaRefusesSymlinkedBootstrap(t *testing.T) {
	for _, relative := range []string{"bootstrap", "bootstrap/overlays", "bootstrap/overlays/prod"} {
		t.Run(relative, func(t *testing.T) {
			root := t.TempDir()
			overlay := filepath.Join(root, "bootstrap", "overlays", "prod")
			require.NoError(t, os.MkdirAll(overlay, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(overlay, kustomizationFile), []byte("resources: []\n"), 0o600))
			outside := t.TempDir()
			target := filepath.Join(outside, "target")
			require.NoError(t, os.Rename(filepath.Join(root, relative), target))
			require.NoError(t, os.Symlink(target, filepath.Join(root, relative)))
			before := snapshotTree(t, outside)
			_, err := projectResourceQuota(testRenderRoot(t, root), filepath.Join(root, "bootstrap"), "prod", "target", fullResourceQuota())
			require.ErrorContains(t, err, "symbolic link")
			require.Equal(t, before, snapshotTree(t, outside))
		})
	}
}

func TestKustomizeFilesystemRechecksLinksOnEveryRead(t *testing.T) {
	root := t.TempDir()
	overlay := filepath.Join(root, "overlays", "prod")
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	file := filepath.Join(overlay, "data with spaces.txt")
	require.NoError(t, os.WriteFile(file, []byte("owned"), 0o600))
	tree := testRenderRoot(t, root)
	filesystem, err := tree.fileSystem()
	require.NoError(t, err)
	content, err := filesystem.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, "owned", string(content))
	outside := filepath.Join(t.TempDir(), "external")
	require.NoError(t, os.WriteFile(outside, []byte("external"), 0o600))
	require.NoError(t, os.Remove(file))
	require.NoError(t, os.Symlink(outside, file))
	_, err = filesystem.ReadFile(file)
	require.ErrorContains(t, err, "symbolic link")
	_, err = filesystem.Open(file)
	require.ErrorContains(t, err, "symbolic link")
	_, _, err = filesystem.CleanedAbs(file)
	require.ErrorContains(t, err, "symbolic link")
	content, err = os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "external", string(content))
}

func TestRenderRootResolvesRelativeBoundaryOnce(t *testing.T) {
	directory, err := os.Getwd()
	require.NoError(t, err)
	root := t.TempDir()
	relative, err := filepath.Rel(directory, root)
	require.NoError(t, err)
	require.NoError(t, kustomizedWorkload(sidecarDeployment)(t.Context(), root))
	_, err = validateTree(relative, &RenderOptions{Environment: "prod", Namespace: "target", Promotable: true})
	require.NoError(t, err)
}
