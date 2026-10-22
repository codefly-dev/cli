package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreservices "github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

func testRenderRoot(t *testing.T, directory string) *renderRoot {
	t.Helper()
	tree, err := newRenderRoot(directory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })
	return tree
}

func TestRenderRootRefusesSymlinkedServiceAncestors(t *testing.T) {
	for _, relative := range []string{"services", "services/api", "services/api/base", "services/api/overlays", "services/api/overlays/prod", "services/api/overlays/prod/kustomization.yaml"} {
		for _, operation := range []string{"patch", "configuration", "full render"} {
			t.Run(relative+"/"+operation, func(t *testing.T) {
				workspace, env := hostComposition(t)
				scope := moduleScope(env, workspace, "app")
				var err error
				root := t.TempDir()
				tree := testRenderRoot(t, root)
				body := strings.Replace(sidecarDeployment, "metadata:\n", "metadata:\n  namespace: "+scope.Namespace+"\n", 1)
				require.NoError(t, kustomizedWorkload(body)(t.Context(), root))
				outside := t.TempDir()
				target := filepath.Join(outside, "target")
				require.NoError(t, os.Rename(filepath.Join(root, relative), target))
				require.NoError(t, os.Symlink(target, filepath.Join(root, relative)))
				before := snapshotTree(t, outside)
				service := &resources.Service{Name: "api"}
				projection := podProjection{kind: kindDeployment, name: "api", container: "api", volume: projectedVolume("test"), mount: readOnlyMount("test", "/test")}
				switch operation {
				case "patch":
					// A base symlink is not read by the patch writer. Its whole subtree
					// preflight must still refuse before modifying the overlay index.
					err = addOverlayPatches(tree, filepath.Join(root, "services", "api"), env.Name, []*podProjection{&projection})
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
			require.NoError(t, addOverlayPatches(testRenderRoot(t, root), filepath.Join(root, "services", "api"), "prod", []*podProjection{&projection}))
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

func TestRenderRootRefusesReplacementOfRootOrAncestor(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		t.Run(fmt.Sprint(ancestor), func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "stage")
			directory := filepath.Join(parent, "tree")
			require.NoError(t, os.MkdirAll(directory, 0o755))
			tree := testRenderRoot(t, directory)
			require.NoError(t, tree.write(directory, "victim", []byte("owned")))
			filesystem, err := tree.fileSystem()
			require.NoError(t, err)
			outside := t.TempDir()
			replacement := directory
			if ancestor {
				replacement = parent
				require.NoError(t, os.Mkdir(filepath.Join(outside, "tree"), 0o755))
			}
			victim := filepath.Join(outside, "victim")
			if ancestor {
				victim = filepath.Join(outside, "tree", "victim")
			}
			require.NoError(t, os.WriteFile(victim, []byte("external"), 0o600))
			require.NoError(t, os.Rename(replacement, replacement+"-original"))
			require.NoError(t, os.Symlink(outside, replacement))
			require.Error(t, tree.write(directory, "victim", []byte("changed")))
			_, err = filesystem.ReadFile(filepath.Join(directory, "victim"))
			require.Error(t, err)
			require.Error(t, tree.mkdirAll(filepath.Join(directory, "new")))
			data, err := os.ReadFile(victim)
			require.NoError(t, err)
			require.Equal(t, "external", string(data))
		})
	}
}

// Swap after access has checked the paths. The returned handle must still be
// the original root, even when the pathname is temporarily redirected.
func TestRenderAccessRetainsOriginalHandle(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "stage")
	directory := filepath.Join(parent, "tree")
	require.NoError(t, os.MkdirAll(directory, 0o755))
	tree := testRenderRoot(t, directory)
	handle, relative, err := tree.access(directory, "victim")
	require.NoError(t, err)
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(outside, "tree"), 0o755))
	victim := filepath.Join(outside, "tree", "victim")
	require.NoError(t, os.WriteFile(victim, []byte("external"), 0o600))
	require.NoError(t, os.Rename(parent, parent+"-original"))
	require.NoError(t, os.Symlink(outside, parent))
	require.NoError(t, handle.WriteFile(relative, []byte("original handle"), 0o600))
	data, err := os.ReadFile(victim)
	require.NoError(t, err)
	require.Equal(t, "external", string(data))
	require.Error(t, tree.checkIdentity())
}

func TestServiceAccountProjectionRefusesRootReplacementDuringWrite(t *testing.T) {
	workspace, env := hostComposition(t)
	scope := moduleScope(env, workspace, "app")
	parent := filepath.Join(t.TempDir(), "stage")
	directory := filepath.Join(parent, "tree")
	require.NoError(t, os.MkdirAll(directory, 0o755))
	require.NoError(t, kustomizedWorkload(sidecarDeployment)(t.Context(), directory))
	tree := testRenderRoot(t, directory)
	filesystem, err := tree.fileSystem()
	require.NoError(t, err)
	outside := t.TempDir()
	require.NoError(t, copyTree(directory, filepath.Join(outside, "tree")))
	before := snapshotTree(t, outside)
	projection := &coreservices.PodTemplateOverlay{}
	require.NoError(t, projection.AttachServiceAccount(&coreservices.WorkloadServiceAccount{}, nil))
	writer := &replaceRootOnWrite{
		FileSystem: &projectionFileSystem{FileSystem: filesystem, tree: tree},
		replace: func() {
			require.NoError(t, os.Rename(parent, parent+"-original"))
			require.NoError(t, os.Symlink(outside, parent))
		},
	}
	err = coreservices.ProjectServiceAccountOnFS(t.Context(), writer, filepath.Join(directory, "services", "api", "base"), scope.Namespace, "api", projection)
	require.Error(t, err)
	require.Equal(t, before, snapshotTree(t, outside))
}

type replaceRootOnWrite struct {
	filesys.FileSystem
	replace func()
}

func (filesystem *replaceRootOnWrite) WriteFile(name string, data []byte) error {
	if filesystem.replace != nil {
		filesystem.replace()
		filesystem.replace = nil
	}
	return filesystem.FileSystem.WriteFile(name, data)
}

func TestRenderPromotionRefusesReplacedStagingParent(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(outside, "tree"), 0o755))
	victim := filepath.Join(outside, "tree", InventoryFilename)
	require.NoError(t, os.WriteFile(victim, []byte("external"), 0o600))
	before := snapshotTree(t, outside)
	opts := &RenderOptions{Destination: filepath.Join(t.TempDir(), "promoted"), Environment: "prod", Promotable: true}
	_, err := RenderOwnedTree(t.Context(), opts, func(ctx context.Context, root string) error {
		require.NoError(t, kustomizedWorkload(sidecarDeployment)(ctx, root))
		parent := filepath.Dir(root)
		require.NoError(t, os.Rename(parent, parent+"-original"))
		require.NoError(t, os.Symlink(outside, parent))
		return nil
	})
	require.Error(t, err)
	require.Equal(t, before, snapshotTree(t, outside))
	_, err = os.Stat(opts.Destination)
	require.True(t, os.IsNotExist(err))
}

func TestRenderBundleOperationsRefuseReplacedRoot(t *testing.T) {
	for _, operation := range []string{"bootstrap", "managed"} {
		t.Run(operation, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "stage")
			directory := filepath.Join(parent, "tree")
			require.NoError(t, os.MkdirAll(directory, 0o755))
			tree := testRenderRoot(t, directory)
			outside := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(outside, "tree", "bundle"), 0o755))
			victim := filepath.Join(outside, "tree", "bundle", "victim.yaml")
			require.NoError(t, os.WriteFile(victim, []byte("external"), 0o600))
			before := snapshotTree(t, outside)
			source := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(source, "overlays", "prod"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(source, "victim.yaml"), []byte("changed"), 0o600))
			require.NoError(t, os.Rename(parent, parent+"-original"))
			require.NoError(t, os.Symlink(outside, parent))
			var err error
			if operation == "bootstrap" {
				err = copyEnvironmentBootstrapInTree(tree, source, "prod", filepath.Join(directory, "bundle"))
			} else {
				_, err = retainManagedBundleInTree(tree, filepath.Join(directory, "bundle"), "cache", "prod", "app", nil)
			}
			require.Error(t, err)
			require.Equal(t, before, snapshotTree(t, outside))
		})
	}
}
