package gitops

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/routing"
	"github.com/stretchr/testify/require"
)

func meshedRenderOptions(t *testing.T, root string) *RenderOptions {
	t.Helper()
	return &RenderOptions{
		Module: "platform", Environment: "production", Namespace: "cell-platform",
		Promotable: true, OwnedPath: root,
		Host: &environments.EnvironmentHost{
			Coordinate: "cell/prod", Component: "host", Domain: "cell",
			Audience: "mint", TrustDomain: "prod.cell", Delivery: "authority/hop/http",
		},
	}
}

// The render delivers mesh edges naming the principals admitted to each
// endpoint; a principal is a fact about a connection only where the peer
// authenticated it. So a meshed render projects the namespace-wide STRICT
// policy as its own layer — every workload of the namespace, including pods
// this render never saw — and carries its path in the inventory so the
// promotion points an Application at it.
func TestAMeshedRenderProjectsNamespaceWideSTRICT(t *testing.T) {
	root := t.TempDir()
	tree := testRenderRoot(t, root)
	opts := meshedRenderOptions(t, root)
	path, err := projectMeshPosture(tree, root, opts)
	require.NoError(t, err)
	require.Equal(t, meshPostureDir, path)

	overlay := filepath.Join(root, meshPostureDir, "overlays", "production")
	manifests, err := overlayManifests(overlay)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	policy := manifests[0]
	require.Equal(t, routing.PeerAuthenticationGroup, policy.group)
	require.Equal(t, routing.PeerAuthenticationKind, policy.kind)
	require.Equal(t, routing.MeshNamespacePolicyName, metadataString(policy.value, keyName))
	require.Equal(t, "cell-platform", metadataString(policy.value, "namespace"))
	spec := mapField(policy.value, "spec")
	require.Nil(t, mapField(spec, "selector"), "no selector is every workload of the namespace")
	require.Equal(t, routing.MeshModeSTRICT, mapField(spec, "mtls")["mode"])

	// The check reads what reached the tree, so it accepts exactly this.
	require.NoError(t, checkMeshPosture(manifests, opts))
}

// Off a mesh there is nothing to make strict, and nothing is projected or
// required: a hostless environment delivers no edges either.
func TestAnUnmeshedRenderProjectsNoPosture(t *testing.T) {
	root := t.TempDir()
	opts := meshedRenderOptions(t, root)
	opts.Host = nil
	path, err := projectMeshPosture(testRenderRoot(t, root), root, opts)
	require.NoError(t, err)
	require.Empty(t, path)
	require.NoError(t, checkMeshPosture(nil, opts))
}

// A meshed render with no resolved namespace has no namespace to make strict.
func TestAMeshedRenderWithoutANamespaceIsRefused(t *testing.T) {
	root := t.TempDir()
	opts := meshedRenderOptions(t, root)
	opts.Namespace = ""
	_, err := projectMeshPosture(testRenderRoot(t, root), root, opts)
	require.ErrorContains(t, err, "no resolved namespace to require STRICT mTLS in")
}

// The posture check is the half that holds whatever reached the tree: a
// meshed tree carrying no namespace-wide policy, one selecting only some
// workloads, or one an overlay relaxed to PERMISSIVE.
func TestThePostureCheckRefusesAMeshedTreeThatIsNotSTRICT(t *testing.T) {
	root := t.TempDir()
	opts := meshedRenderOptions(t, root)

	t.Run("no policy at all", func(t *testing.T) {
		err := checkMeshPosture(nil, opts)
		require.ErrorContains(t, err, "carries no namespace-wide PeerAuthentication")
		require.ErrorContains(t, err, "a plaintext peer has no principal")
	})

	t.Run("a selector is not the namespace", func(t *testing.T) {
		decoded, _, err := decodeYAML("selected.yaml", []byte(`apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata:
  name: `+routing.MeshNamespacePolicyName+`
  namespace: cell-platform
spec:
  selector:
    matchLabels:
      app: api
  mtls:
    mode: STRICT
`))
		require.NoError(t, err)
		require.ErrorContains(t, checkMeshPosture(decoded, opts), "carries no namespace-wide PeerAuthentication")
	})

	t.Run("a relaxed mode is refused wherever it came from", func(t *testing.T) {
		for _, mode := range []string{"PERMISSIVE", "DISABLE", ""} {
			decoded, _, err := decodeYAML("relaxed.yaml", []byte(`apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata:
  name: agent-own
  namespace: cell-platform
spec:
  mtls:
    mode: `+mode+`
`))
			require.NoError(t, err)
			err = checkMeshPosture(decoded, opts)
			require.ErrorContains(t, err, "agent-own")
			require.ErrorContains(t, err, "every policy of the namespace is STRICT")
		}
	})
}

// A workload running as the namespace's default ServiceAccount has an identity
// every such pod of the namespace shares, so a cell cannot admit callers by
// principal at all — and the compiled policies of a real cell allowed
// `sa/default` 109 times. The render projects the account named after the
// service on a meshed environment; a tree that still names none is refused by
// name, as the delivery Job's account already is.
func TestTheCellRefusesAWorkloadRunningAsTheDefaultAccount(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	result := renderCellTree(t, workspace, "shop", "api", "acme-shop", "staging")
	rendered := filepath.Join(result.Path, "services", "api", "overlays", "staging", "deployment.yaml")
	data, err := os.ReadFile(rendered)
	require.NoError(t, err)
	require.Contains(t, string(data), "serviceAccountName: api",
		"a meshed render gives every service the account its principal names")
	stripped := regexp.MustCompile(`(?m)^\s*serviceAccountName:.*\n`).ReplaceAllString(string(data), "")
	require.NoError(t, os.WriteFile(rendered, []byte(stripped), 0o644))
	_, err = RenderCell(context.Background(), workspace, env)
	require.ErrorContains(t, err, `workload api runs as the namespace's "default" ServiceAccount`)
	require.ErrorContains(t, err, "every pod of the namespace that names no account shares")
}

// The guarantee has to reach the cell, not just the staged tree: the layer is
// in the rendered module, in the promoted commit, and has an Argo Application
// of its own in the module-resources wave — before the workloads whose
// principals it makes meaningful.
func TestThePostureLayerIsPromotedAsItsOwnApplication(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	overlay := filepath.Join(workspace.Dir(), "deployments", "modules", "payments", meshPostureDir, "overlays", "production")
	manifests, err := overlayManifests(overlay)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	require.Equal(t, routing.MeshNamespacePolicyName, metadataString(manifests[0].value, keyName))

	inventory, err := LoadInventory(filepath.Join(workspace.Dir(), "deployments", "modules", "payments"))
	require.NoError(t, err)
	require.Equal(t, meshPostureDir, inventory.MeshPath, "the inventory records the delivered layer")

	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, &request)
	require.NoError(t, err)
	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
	require.NoError(t, err)

	committed := gitOutput(t, "", "--git-dir", remote, "show",
		result.Commit+":"+result.Path+"/"+meshPostureDir+"/overlays/production/"+meshPostureFile)
	require.Contains(t, committed, "kind: PeerAuthentication")
	require.Contains(t, committed, "mode: STRICT")

	tree := gitOutput(t, "", "--git-dir", remote, "ls-tree", "-r", "--name-only", result.Commit)
	require.Contains(t, tree, result.Path+"/"+meshPostureDir+"/overlays/production/"+meshPostureFile,
		"the promoted commit carries the layer the cell applies")
}

// The layer reaches the cluster through an Application of its own, in the
// module-resources wave: before the workloads whose principals it makes
// meaningful, and never inside a unit, which two units would both claim.
func TestThePostureLayerIsItsOwnArgoApplication(t *testing.T) {
	root := t.TempDir()
	targetPath := "environments/modules/platform"
	inventory := &Inventory{
		SchemaVersion: SchemaVersion, Module: "platform", Environment: "production",
		Namespace: "cell-platform", AppProject: "platform", MeshPath: meshPostureDir,
		Units: []InventoryUnit{{Kind: UnitKindService, Module: "platform", Name: "api", Path: "services/api"}},
	}
	writeOverlay(t, filepath.Join(root, "services", "api", "overlays", "production"))
	meshOverlay := filepath.Join(root, meshPostureDir, "overlays", "production")
	require.NoError(t, os.MkdirAll(meshOverlay, 0o755))
	tree := testRenderRoot(t, root)
	require.NoError(t, writeMeshPostureKustomizationInTree(tree, meshOverlay))
	require.NoError(t, writeOverlayResourceInTree(tree, meshOverlay, meshPostureFile, routing.NamespaceMeshPolicy("cell-platform")))

	config := &repositoryConfig{RepoURL: "https://github.com/codefly-dev/manifests.git"}
	require.NoError(t, generateArgoBootstrap(context.Background(), config, root, targetPath, inventory, "production", strings.Repeat("c", 40), ""))

	data, err := os.ReadFile(filepath.Join(root, "bootstrap", "applicationset.yaml"))
	require.NoError(t, err)
	set := string(data)
	require.Contains(t, set, "overlay: "+targetPath+"/"+meshPostureDir+"/overlays/production")
	require.Contains(t, set, "wave: \""+moduleResourcesWave+"\"")
	// The AppProject's authority is derived from the promoted sources, so the
	// policy's kind is whitelisted by being there rather than by a list.
	project, err := os.ReadFile(filepath.Join(root, "bootstrap", "project.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(project), routing.PeerAuthenticationKind)
	require.Contains(t, string(project), routing.PeerAuthenticationGroup)
	// And the layer is part of the exact graph every publish gate walks.
	require.NoError(t, validateBootstrapUnits(filepath.Join(root, "bootstrap"), targetPath, inventory, "production"))
}
