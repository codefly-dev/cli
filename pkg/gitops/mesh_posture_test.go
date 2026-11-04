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
	"github.com/codefly-dev/core/resources"
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
	path, posture, err := projectMeshPosture(tree, root, opts)
	require.NoError(t, err)
	require.Equal(t, meshPostureDir, path)
	require.Equal(t, routing.MeshModeSTRICT, posture, "the posture the tree carries is recorded, so a verifier requires it")
	opts.MeshPosture = posture

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
	path, posture, err := projectMeshPosture(testRenderRoot(t, root), root, opts)
	require.NoError(t, err)
	require.Empty(t, path)
	require.Empty(t, posture)
	require.NoError(t, checkMeshPosture(nil, opts))
}

// A meshed render with no resolved namespace has no namespace to make strict.
func TestAMeshedRenderWithoutANamespaceIsRefused(t *testing.T) {
	root := t.TempDir()
	opts := meshedRenderOptions(t, root)
	opts.Namespace = ""
	_, _, err := projectMeshPosture(testRenderRoot(t, root), root, opts)
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

// A PORT-LEVEL MODE OVERRIDES THE NAMESPACE'S STRICT, so the check reads
// those entries too. Istio takes spec.portLevelMtls, a mode per workload port,
// and a policy that reads STRICT at spec.mtls.mode while carrying
// `portLevelMtls: {8081: {mode: DISABLE}}` leaves 8081 accepting plaintext —
// with no peer principal — while the edges delivered for that port still read
// "only the hop".
//
// The port key is a NUMBER, and written unquoted the mapping decodes with
// non-string keys, so a reader that looks only at map[string]any finds nothing
// and passes the policy. Both spellings are exercised here for that reason.
func TestThePostureCheckRefusesAPerPortRelaxation(t *testing.T) {
	root := t.TempDir()
	opts := meshedRenderOptions(t, root)
	policy := func(ports string) []manifest {
		decoded, _, err := decodeYAML("ported.yaml", []byte(`apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata:
  name: `+routing.MeshNamespacePolicyName+`
  namespace: cell-platform
spec:
  mtls:
    mode: STRICT
  portLevelMtls:
`+ports))
		require.NoError(t, err)
		return decoded
	}

	for _, tc := range []struct{ name, ports, contains string }{
		{"an unquoted port number", "    8081:\n      mode: DISABLE\n", "portLevelMtls port 8081 at mtls.mode \"DISABLE\""},
		{"a quoted port", "    \"8081\":\n      mode: PERMISSIVE\n", "portLevelMtls port 8081 at mtls.mode \"PERMISSIVE\""},
		{"a port with no mode at all", "    8081: {}\n", "portLevelMtls port 8081 at mtls.mode \"\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkMeshPosture(policy(tc.ports), opts)
			require.ErrorContains(t, err, tc.contains)
			require.ErrorContains(t, err, "a port-level mode overrides the namespace-wide STRICT")
		})
	}

	// What this check cannot read it must not wave through: an unreadable
	// shape is the one case where skipping would be indistinguishable from
	// passing.
	t.Run("a shape this check cannot read is refused rather than skipped", func(t *testing.T) {
		require.ErrorContains(t, checkMeshPosture(policy("    - 8081\n"), opts), "is not a mapping of port to mode")
		require.ErrorContains(t, checkMeshPosture(policy("    8081: DISABLE\n"), opts), "is not a mode setting")
	})

	t.Run("a redundant STRICT entry is harmless", func(t *testing.T) {
		require.NoError(t, checkMeshPosture(policy("    8081:\n      mode: STRICT\n"), opts))
	})
}

// THE REQUIREMENT TRAVELS WITH THE TREE. The posture was projected and checked
// by the producing render alone, so every reader of a promotable tree —
// ValidateRenderedTree, the service-snapshot check, an observation — accepted
// one rendered by a CLI that wrote no mesh layer, or one whose policy was
// edited to PERMISSIVE after the render. The render records the posture it
// delivered, and a verifier requires it whenever the record carries mesh edges.
func TestAVerifierRequiresThePostureTheRecordDeclares(t *testing.T) {
	root := t.TempDir()
	opts := meshedRenderOptions(t, root)
	opts.Host = nil

	t.Run("a recorded posture is required with no environment host in hand", func(t *testing.T) {
		verifying := *opts
		verifying.MeshPosture = routing.MeshModeSTRICT
		require.ErrorContains(t, checkMeshPosture(nil, &verifying), "carries no namespace-wide PeerAuthentication")
	})

	t.Run("edges with no recorded posture are refused", func(t *testing.T) {
		verifying := *opts
		verifying.MeshEdges = &MeshEdges{SchemaVersion: MeshEdgesSchemaVersion}
		err := checkMeshPosture(nil, &verifying)
		require.ErrorContains(t, err, "carries mesh edges but no delivered mesh posture")
		require.ErrorContains(t, err, "re-render it with a CLI that writes the mesh/ layer")
	})

	t.Run("a tree that is not meshed at all requires nothing", func(t *testing.T) {
		require.NoError(t, checkMeshPosture(nil, opts))
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

// AND THE RENDER SAYS WHOSE ACCOUNT CHANGES. Giving every service of a meshed
// namespace its own ServiceAccount is a cell-wide behaviour change on the next
// render of an already-running namespace: a rolling restart of each workload,
// and — the part nothing in the tree shows — the end of any RBAC or cloud
// workload-identity binding made against the namespace's `default` account for
// it. The render reports the set so an operator can move those bindings; a
// service that already held an account of its own is not in it, because
// nothing changes for it.
func TestTheRenderNamesTheWorkloadsWhoseAccountChanges(t *testing.T) {
	meshed := &environments.Environment{
		Name: "production",
		Host: &environments.EnvironmentHost{TrustDomain: "prod.cell"},
		ServiceIdentity: &environments.EnvironmentServiceIdentity{
			Services: map[string]environments.EnvironmentWorkloadIdentity{
				"platform/declared": {Annotations: map[string]string{"iam.gke.io/gcp-service-account": "declared@project.iam"}},
			},
		},
		ManagedServices: map[string]environments.EnvironmentManagedService{"platform/redis": {}},
	}
	services := []*resources.Service{
		{Name: "api"},
		{Name: "worker"},
		{Name: "declared"},
		{Name: "gateway", ModuleIdentity: true},
		{Name: "authority", WorkspaceConfigurationDependencies: []string{environments.ModuleAuthorityConfiguration}},
		{Name: "redis"},
		nil,
	}
	require.Equal(t, []string{"platform/api", "platform/worker"}, meshNamedAccounts("platform", services, meshed),
		"only the services the mesh alone gives an account: not one with a declared identity, not one that presents its module's, not one the environment manages")

	t.Run("off a mesh nothing changes", func(t *testing.T) {
		unmeshed := *meshed
		unmeshed.Host = nil
		require.Empty(t, meshNamedAccounts("platform", services, &unmeshed))
	})
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

// A VERIFIER READS A TREE IT DID NOT WRITE, AND HOLDS IT TO THE POSTURE.
//
// The projection and the check were both the producing render's: every other
// reader of a promotable tree — ValidateRenderedTree, the service-snapshot
// check, an observation of the reconciled Git tree — trusted the producer for
// the one guarantee that makes the delivered edges mean anything. So a tree
// whose policy was relaxed to PERMISSIVE after the render, or one rendered by
// a CLI that wrote no mesh layer at all, verified and promoted without
// complaint. The posture travels with the tree in its own record, and the
// check runs on every promotable pass.
func TestAVerifierHoldsARenderedTreeToItsRecordedPosture(t *testing.T) {
	_, workspace, _ := developerPublishWorkspace(t)
	tree := filepath.Join(workspace.Dir(), "deployments", "modules", "payments")
	require.NoError(t, ValidateRenderedTree(tree, "", true), "the render's own tree verifies")

	inventory, err := LoadInventory(tree)
	require.NoError(t, err)
	require.Equal(t, routing.MeshModeSTRICT, inventory.MeshPosture, "the render records the posture it delivered")

	t.Run("a policy relaxed after the render is refused", func(t *testing.T) {
		policy := filepath.Join(tree, meshPostureDir, "overlays", "production", meshPostureFile)
		data, readErr := os.ReadFile(policy)
		require.NoError(t, readErr)
		relaxed := strings.Replace(string(data), "mode: "+routing.MeshModeSTRICT, "mode: PERMISSIVE", 1)
		require.NotEqual(t, string(data), relaxed, "the fixture's policy was not found")
		require.NoError(t, os.WriteFile(policy, []byte(relaxed), 0o644))
		defer func() { require.NoError(t, os.WriteFile(policy, data, 0o644)) }()

		err := ValidateRenderedTree(tree, "", true)
		require.ErrorContains(t, err, `mtls.mode "PERMISSIVE"`)
		require.ErrorContains(t, err, "a principal is only a fact about a connection the peer authenticated")
	})

	t.Run("a tree that delivers edges and no posture is refused", func(t *testing.T) {
		// The record of a render that delivered edges and no posture layer:
		// what a pre-#1280 CLI produced, and what dropping the layer from a
		// current one would produce.
		stripped := inventory
		stripped.MeshPath = ""
		stripped.MeshPosture = ""
		stripped.MeshEdges = &MeshEdges{SchemaVersion: MeshEdgesSchemaVersion, TrustDomain: "prod.cell"}
		require.NoError(t, os.RemoveAll(filepath.Join(tree, meshPostureDir)))
		require.NoError(t, writeCanonicalInventory(filepath.Join(tree, InventoryFilename), &stripped))

		err := ValidateRenderedTree(tree, "", true)
		require.ErrorContains(t, err, "carries mesh edges but no delivered mesh posture",
			"this is the pre-#1280 render: the edges arrive with nothing making them enforceable")
	})
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
