package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/posture"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// posturedFixture copies the container-ports workspace — module "shop", a
// go-grpc "api" depending on a postgres "store", environment "staging" — and
// appends a posture block to that environment's declaration, so a render is
// driven by a posture the workspace actually declares rather than one a test
// hands the render directly.
func posturedFixture(t *testing.T, block string) (*resources.Workspace, *resources.Module, *environments.Environment) {
	t.Helper()
	workspace, module, env, err := posturedFixtureE(t, block)
	require.NoError(t, err)
	return workspace, module, env
}

// posturedFixtureE is posturedFixture returning the error the workspace read
// raises, for a declaration that must be refused.
func posturedFixtureE(t *testing.T, block string) (*resources.Workspace, *resources.Module, *environments.Environment, error) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, copyTree(filepath.Join("testdata", "container-ports"), root))
	path := filepath.Join(root, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, []byte(block)...), 0o644))
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		return nil, nil, nil, err
	}
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	if err != nil {
		return nil, nil, nil, err
	}
	env, err := orchestration.SelectEnvironment(workspace, "staging")
	if err != nil {
		return nil, nil, nil, err
	}
	return workspace, module, env, nil
}

// meshedStaging is an environment that states the platform already protects
// transport between its workloads, with the allowances a test declares.
func meshedStaging(allowances ...posture.Allowance) *posture.Declaration {
	return &posture.Declaration{
		Asserts:    map[string]bool{posture.AssertMeshProtectedTransport: true},
		Allowances: allowances,
	}
}

// renderWorkloads renders a promotable owned tree holding one manifest per
// service, under services/<name>/overlays/staging, and returns the result.
func renderWorkloads(t *testing.T, declaration *posture.Declaration, promotable bool, workloads map[string]string) (RenderResult, string, error) {
	t.Helper()
	parent := t.TempDir()
	destination := filepath.Join(parent, "modules", "shop")
	names := make([]string, 0, len(workloads))
	for name := range workloads {
		names = append(names, name)
	}
	options := RenderOptions{
		Destination: destination, Module: "shop", UnitNames: names,
		Environment: "staging", Promotable: promotable, Posture: declaration,
		DeploysToCell: true,
	}
	result, err := RenderOwnedTree(context.Background(), &options, func(_ context.Context, root string) error {
		for name, manifest := range workloads {
			overlay := filepath.Join(root, "services", name, "overlays", "staging")
			if mkErr := os.MkdirAll(overlay, 0o755); mkErr != nil {
				return mkErr
			}
			if writeErr := os.WriteFile(filepath.Join(overlay, "workload.yaml"), []byte(manifest), 0o644); writeErr != nil {
				return writeErr
			}
		}
		return nil
	})
	return result, destination, err
}

// withCustody adds, to a workload's environment, the credential a store holds as
// its own state: what tells a store of credentials apart from a cache, which
// loses nothing it cannot recompute when it restarts.
func withCustody(manifest string) string {
	// A credential never carries a value in a restricted render — it arrives as a
	// reference, which the render's own credential rule already requires.
	const entry = "          env:\n            - name: STORE_ROOT_TOKEN\n" +
		"              valueFrom:\n                secretKeyRef:\n" +
		"                  name: secret-store\n                  key: STORE_ROOT_TOKEN\n"
	return strings.Replace(manifest, "          env:\n", entry, 1)
}

// conformingWorkload is what the platform's own projection produces: scratch
// space for a read-only root filesystem, a durable claim for the data it owns,
// and every value and credential arriving as an environment variable.
func conformingWorkload(name string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: %[1]s
  namespace: acme
spec:
  serviceName: %[1]s
  template:
    spec:
      containers:
        - name: %[1]s
          image: registry.example.com/acme/%[1]s@sha256:%[2]s
          env:
            - name: ACME__ENDPOINT__SHOP__STORE__TCP
              value: store.acme.svc.cluster.local:5432
            - name: ACME__SERVICE_SECRET_CONFIGURATION__SHOP__%[3]s__STORE__PASSWORD
              valueFrom:
                secretKeyRef:
                  name: secret-%[1]s
                  key: ACME__SERVICE_SECRET_CONFIGURATION__SHOP__%[3]s__STORE__PASSWORD
      volumes:
        - name: tmp
          emptyDir: {}
        - name: data
          persistentVolumeClaim:
            claimName: %[1]s-data
`, name, strings.Repeat("a", 64), strings.ToUpper(name))
}

// workloadWith renders a conforming workload with one field changed, which is
// the field each refusal below must name.
func workloadWith(name, replace, with string) string {
	manifest := conformingWorkload(name)
	if !strings.Contains(manifest, replace) {
		panic("fixture does not carry " + replace)
	}
	return strings.Replace(manifest, replace, with, 1)
}

func TestDeployedRenderRefusesAServicesOwnTLSForItsInCellPeers(t *testing.T) {
	workload := workloadWith("api", `            - name: ACME__ENDPOINT__SHOP__STORE__TCP
              value: store.acme.svc.cluster.local:5432`,
		`            - name: API_TLS_CERT_FILE
              value: /etc/api/peer.crt`)
	_, _, err := renderWorkloads(t, meshedStaging(), true, map[string]string{"api": workload})
	require.ErrorContains(t, err, "deployed render refuses service shop/api")
	require.ErrorContains(t, err, posture.RulePeerTransportMaterial)
	require.ErrorContains(t, err, "API_TLS_CERT_FILE")
	require.ErrorContains(t, err, "spec.template.spec.containers[0].env[0]")
	require.ErrorContains(t, err, posture.AssertMeshProtectedTransport)
}

func TestDeployedRenderRefusesAMountBeyondTheScratchVolume(t *testing.T) {
	workload := workloadWith("api", `        - name: data
          persistentVolumeClaim:
            claimName: api-data`,
		`        - name: config
          configMap:
            name: api-config`)
	_, destination, err := renderWorkloads(t, meshedStaging(), true, map[string]string{"api": workload})
	require.ErrorContains(t, err, "deployed render refuses service shop/api")
	require.ErrorContains(t, err, posture.RuleNonScratchMount)
	require.ErrorContains(t, err, `volume "config" takes its contents from a configMap source`)
	require.ErrorContains(t, err, "spec.template.spec.volumes[1].configMap")
	_, statErr := os.Stat(destination)
	require.True(t, os.IsNotExist(statErr), "a refused render installs nothing")
}

func TestDeployedRenderRefusesADevelopmentModeStore(t *testing.T) {
	workload := workloadWith("store", `        - name: store
          image:`,
		`        - name: store
          command: ["store", "server", "-dev"]
          image:`)
	// A store holds credential material as its own state — that is what makes an
	// in-memory mode a loss rather than a cold cache.
	workload = withCustody(workload)
	_, _, err := renderWorkloads(t, meshedStaging(), true, map[string]string{"store": workload})
	require.ErrorContains(t, err, "deployed render refuses service shop/store")
	require.ErrorContains(t, err, posture.RuleInMemoryStateStore)
	require.ErrorContains(t, err, "-dev starts its development server")
	require.ErrorContains(t, err, "spec.template.spec.containers[0].command[2]")
}

func TestDeployedRenderAcceptsAConformingTree(t *testing.T) {
	result, _, err := renderWorkloads(t, meshedStaging(), true, map[string]string{
		"api": conformingWorkload("api"), "store": conformingWorkload("store"),
	})
	require.NoError(t, err)
	require.Empty(t, result.PostureAllowances)
	require.NotEmpty(t, result.Inventory.Digest)
}

// A local or ephemeral render is not a cell: the same tree that a deployed
// render refuses renders unchanged.
func TestLocalRenderIsNotHeldToTheDeployedPosture(t *testing.T) {
	mounted := workloadWith("api", `        - name: data
          persistentVolumeClaim:
            claimName: api-data`,
		`        - name: config
          configMap:
            name: api-config`)
	development := workloadWith("store", `        - name: store
          image:`,
		`        - name: store
          command: ["store", "server", "-dev"]
          image:`)
	development = withCustody(development)
	_, _, err := renderWorkloads(t, meshedStaging(), false, map[string]string{"api": mounted, "store": development})
	require.NoError(t, err)
}

// An allowance is the deliberate exception, and the render says so on every run
// — including the run where the allowance is not exercised at all.
func TestADeclaredAllowanceIsHonouredAndPrintedOnEveryRun(t *testing.T) {
	mounted := workloadWith("api", `        - name: data
          persistentVolumeClaim:
            claimName: api-data`,
		`        - name: config
          configMap:
            name: api-config`)
	declaration := meshedStaging(
		posture.Allowance{
			Rule: posture.RuleNonScratchMount, Service: "shop/api",
			Reason: "the asset bundle is built into the image, reviewed by the platform owner",
		},
		posture.Allowance{
			Rule: posture.RulePeerTransportMaterial, Service: "shop/store",
			Reason: "an external peer pins its own CA",
		},
	)
	result, _, err := renderWorkloads(t, declaration, true, map[string]string{
		"api": mounted, "store": conformingWorkload("store"),
	})
	require.NoError(t, err)
	require.Equal(t, []string{
		"security posture: service shop/api is allowed to break rule non-scratch-mount — " +
			"the asset bundle is built into the image, reviewed by the platform owner",
		"security posture: service shop/store is allowed to break rule peer-transport-material — " +
			"an external peer pins its own CA",
	}, result.PostureAllowances)
}

// The posture is checked against the manifests the cell would apply, so a volume
// an environment overlay patches into a conforming base is refused too.
func TestDeployedRenderSeesAVolumeAnOverlayPatchesIn(t *testing.T) {
	parent := t.TempDir()
	destination := filepath.Join(parent, "modules", "shop")
	options := RenderOptions{
		Destination: destination, Module: "shop", UnitNames: []string{"api"},
		Environment: "staging", Promotable: true, Posture: meshedStaging(),
		DeploysToCell: true,
	}
	_, err := RenderOwnedTree(context.Background(), &options, func(_ context.Context, root string) error {
		base := filepath.Join(root, "services", "api", "base")
		overlay := filepath.Join(root, "services", "api", "overlays", "staging")
		for _, dir := range []string{base, overlay} {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return mkErr
			}
		}
		files := map[string]string{
			filepath.Join(base, "workload.yaml"):         conformingWorkload("api"),
			filepath.Join(base, "kustomization.yaml"):    "resources:\n  - workload.yaml\n",
			filepath.Join(overlay, "kustomization.yaml"): "resources:\n  - ../../base\npatches:\n  - path: mount.yaml\n",
			filepath.Join(overlay, "mount.yaml"): `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: api
  namespace: acme
spec:
  template:
    spec:
      volumes:
        - name: config
          configMap:
            name: api-config
`,
		}
		for path, content := range files {
			if writeErr := os.WriteFile(path, []byte(content), 0o644); writeErr != nil {
				return writeErr
			}
		}
		return nil
	})
	require.ErrorContains(t, err, "deployed render refuses service shop/api")
	require.ErrorContains(t, err, posture.RuleNonScratchMount)
	require.ErrorContains(t, err, `volume "config" takes its contents from a configMap source`)
}

// The environment's own declaration is what reaches the render: a module render
// of a real workspace refuses the workload its agent wrote, naming the service.
func TestRenderModuleIsHeldToTheEnvironmentsDeclaredPosture(t *testing.T) {
	installFakeAgents(t)
	t.Cleanup(func() { fakeAgentWorkloadPatch = nil })
	fakeAgentWorkloadPatch = func(service, workload string) string {
		if service != "store" {
			return workload
		}
		return workload + `      volumes:
        - name: config
          configMap:
            name: store-config
`
	}
	workspace, module, env := posturedFixture(t, meshBlock)
	_, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
	require.ErrorContains(t, err, "deployed render refuses service shop/store")
	require.ErrorContains(t, err, posture.RuleNonScratchMount)
	require.ErrorContains(t, err, "spec.template.spec.volumes[0].configMap")

	allowed, module, env := posturedFixture(t, `    posture:
      asserts:
        internal-transport/mesh-protected: true
      allowances:
        - rule: non-scratch-mount
          service: shop/store
          reason: reviewed by the platform owner
`)
	result, err := RenderModule(context.Background(), allowed, module, env, "acme-staging", nil)
	require.NoError(t, err)
	require.Equal(t, []string{
		"security posture: service shop/store is allowed to break rule non-scratch-mount — reviewed by the platform owner",
	}, result.PostureAllowances)
}

// An environment declaring a posture the render could not act on is refused
// when the workspace is validated, not silently ignored.
func TestAnUnknownPostureRuleIsRefusedWhenTheEnvironmentIsRead(t *testing.T) {
	_, _, _, err := posturedFixtureE(t, `    posture:
      allowances:
        - rule: no-mounts-at-all
          service: shop/store
          reason: reviewed
`)
	require.ErrorContains(t, err, `posture allowance 1 names unknown rule "no-mounts-at-all"`)
}

// A dev deployment ships the image of a service it renders here and now, so that
// render is held to the same posture as a full one.
func TestDevServiceBuildIsHeldToTheDeployedPosture(t *testing.T) {
	installFakeAgents(t)
	t.Cleanup(func() { fakeAgentWorkloadPatch = nil })
	workspace, module, env := posturedFixture(t, meshBlock)
	store, err := module.LoadServiceFromName(context.Background(), "store")
	require.NoError(t, err)

	images, err := buildRenderedServiceImages(context.Background(), workspace, module, store, env, false, nil)
	require.NoError(t, err)
	require.NotEmpty(t, images, "a conforming dev render reports the images it pinned")

	fakeAgentWorkloadPatch = func(service, workload string) string {
		if service != "store" {
			return workload
		}
		return workload + `      volumes:
        - name: credentials
          secret:
            secretName: store-credentials
`
	}
	_, err = buildRenderedServiceImages(context.Background(), workspace, module, store, env, false, nil)
	require.ErrorContains(t, err, "deployed render refuses service shop/store")
	require.ErrorContains(t, err, posture.RuleNonScratchMount)
	require.ErrorContains(t, err, "spec.template.spec.volumes[0].secret")
}

// Each entry point must carry the environment's posture into its render. These
// are per-entry-point regression tests: deleting the declaration at any one of
// them fails here, which is what keeps a path from silently losing the guard.
func TestEveryRenderEntryPointCarriesTheEnvironmentPosture(t *testing.T) {
	mounted := func(service, workload string) string {
		if service != "store" {
			return workload
		}
		return workload + `      volumes:
        - name: credentials
          secret:
            secretName: store-credentials
`
	}
	t.Run("a module render", func(t *testing.T) {
		installFakeAgents(t)
		t.Cleanup(func() { fakeAgentWorkloadPatch = nil })
		fakeAgentWorkloadPatch = mounted
		workspace, module, env := posturedFixture(t, meshBlock)
		_, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
		require.ErrorContains(t, err, "deployed render refuses service shop/store")
		require.ErrorContains(t, err, posture.RuleNonScratchMount)
	})
	t.Run("a single-service render", func(t *testing.T) {
		installFakeAgents(t)
		t.Cleanup(func() { fakeAgentWorkloadPatch = nil })
		fakeAgentWorkloadPatch = mounted
		workspace, module, env := posturedFixture(t, meshBlock)
		api, err := module.LoadServiceFromName(context.Background(), "api")
		require.NoError(t, err)
		_, err = RenderService(context.Background(), workspace, module, api, env, "acme-staging", false, nil)
		require.ErrorContains(t, err, "deployed render refuses service shop/store",
			"a service render drives its dependency graph, and every unit it stages is held to the posture")
		require.ErrorContains(t, err, posture.RuleNonScratchMount)
	})
	// A solution renders through its own entry point, with its own options, and
	// drives an executor an in-repo test cannot stand in for. Its options are
	// assembled by one function, so the declaration it must carry is covered here.
	t.Run("a solution render", func(t *testing.T) {
		_, _, env := posturedFixture(t, meshBlock)
		options := solutionRenderOptions(
			&SolutionRenderRequest{Name: "checkout", Environment: env, AppProject: "acme-staging"},
			env, "/tmp/destination", "deployments/modules/checkout", "acme-checkout")
		require.True(t, options.deployedRender(),
			"a solution rendered for a cell is held to the posture")
		require.Same(t, env.Posture, options.Posture,
			"the environment's declaration, including its allowances, reaches the render")
		require.True(t, options.Posture.Asserted(posture.AssertMeshProtectedTransport))
	})
}

// meshBlock is the posture declaration the fixture environment carries.
const meshBlock = `    posture:
      asserts:
        internal-transport/mesh-protected: true
`

// A dev build is held to the posture of the environment it is building for, and
// to that environment's manifests only: another environment's overlay in the same
// scratch tree is not what this deployment ships.
func TestDevServiceBuildReadsOnlyTheRequestedEnvironment(t *testing.T) {
	installFakeAgents(t)
	t.Cleanup(func() { fakeAgentExtraFiles = nil })
	fakeAgentExtraFiles = func(service string) map[string]string {
		if service != "store" {
			return nil
		}
		return map[string]string{
			filepath.Join("overlays", "local", "kustomization.yaml"): "resources:\n  - ../../base\n  - development.yaml\n",
			filepath.Join("overlays", "local", "development.yaml"): `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store-development
spec:
  template:
    spec:
      containers:
        - name: store
          args: ["server", "-dev"]
          env:
            - name: STORE_ROOT_TOKEN
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: STORE_ROOT_TOKEN
`,
		}
	}
	workspace, module, env := posturedFixture(t, meshBlock)
	store, err := module.LoadServiceFromName(context.Background(), "store")
	require.NoError(t, err)
	images, err := buildRenderedServiceImages(context.Background(), workspace, module, store, env, false, nil)
	require.NoError(t, err,
		"a development overlay the requested environment never applies is not this build's manifests")
	require.NotEmpty(t, images)
}
