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

// meshBlock is the posture declaration the fixture environment carries.
const meshBlock = `    posture:
      asserts:
        internal-transport/mesh-protected: true
`

// posturedFixture copies the container-ports workspace — module "shop", a go-grpc
// "api" depending on a postgres "store", environment "staging" — and appends a
// posture block to that environment, so a render is driven by a posture the
// workspace actually declares rather than one a test hands it.
func posturedFixture(t *testing.T, block string) (*resources.Workspace, *resources.Module, *environments.Environment) {
	t.Helper()
	workspace, module, env, err := posturedFixtureE(t, block)
	require.NoError(t, err)
	return workspace, module, env
}

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

// declaringFixture is posturedFixture with a block appended to one service's own
// spec, so a test can be about what the service declared.
func declaringFixture(t *testing.T, service, specBlock string) (*resources.Workspace, *resources.Module, *environments.Environment) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, copyTree(filepath.Join("testdata", "container-ports"), root))
	workspacePath := filepath.Join(root, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(workspacePath, append(data, []byte(meshBlock)...), 0o644))
	servicePath := filepath.Join(root, "modules", "shop", "services", service, resources.ServiceConfigurationName)
	declaration, err := os.ReadFile(servicePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(servicePath, append(declaration, []byte(specBlock)...), 0o644))
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	env, err := orchestration.SelectEnvironment(workspace, "staging")
	require.NoError(t, err)
	return workspace, module, env
}

// ephemeralStorageBlock is what a service declares when its state does not survive
// its process — which a deployed render refuses.
const ephemeralStorageBlock = "  deployment:\n    storage: ephemeral\n"

func meshedStaging(allowances ...posture.Allowance) *posture.Declaration {
	return &posture.Declaration{
		Asserts:    map[string]bool{posture.AssertMeshProtectedTransport: true},
		Allowances: allowances,
	}
}

// durableContracts declares, for every unit of these tests, what the platform
// renders for it — one scratch volume at one path — and durable storage, so a case
// is about the thing it is testing rather than about a missing declaration.
func durableContracts(services ...string) posture.Contracts {
	contracts := posture.Contracts{}
	for _, service := range services {
		contracts.Add(&posture.ServiceContract{
			Module: "shop", Service: service, StorageMode: posture.StorageModeDurable,
			ScratchVolumes: []posture.ScratchVolume{{Name: "tmp", Mount: "/tmp"}},
		})
	}
	return contracts
}

// renderWorkloads renders a deployed owned tree holding one manifest per unit.
func renderWorkloads(
	t *testing.T,
	declaration *posture.Declaration,
	contracts posture.Contracts,
	deployed bool,
	units map[string]string,
) (RenderResult, string, error) {
	t.Helper()
	parent := t.TempDir()
	destination := filepath.Join(parent, "modules", "shop")
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	options := RenderOptions{
		Destination: destination, Module: "shop", UnitNames: names,
		Environment: "staging", Promotable: true, Posture: declaration,
		Contracts: contracts, DeploysToCell: deployed,
	}
	result, err := RenderOwnedTree(context.Background(), &options, func(_ context.Context, root string) error {
		for name, manifest := range units {
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

// conformingUnit is what the platform's own projection produces: the standard
// scratch volume at the standard scratch path, every value and credential arriving
// as an environment variable.
func conformingUnit(name string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: acme
spec:
  template:
    spec:
      containers:
        - name: %[1]s
          image: registry.example.com/acme/%[1]s@sha256:%[2]s
          env:
            - name: ACME__ENDPOINT__SHOP__STORE__TCP
              value: store.acme.svc.cluster.local:5432
            - name: ACME__SERVICE_SECRET_CONFIGURATION__SHOP__STORE__POSTGRES__PASSWORD
              valueFrom:
                secretKeyRef:
                  name: secret-%[1]s
                  key: ACME__SERVICE_SECRET_CONFIGURATION__SHOP__STORE__POSTGRES__PASSWORD
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
`, name, strings.Repeat("a", 64))
}

// statefulUnit keeps its own state, which is what makes a storage declaration
// required. Its claim also needs a mount allowance: whether a store may carry a
// durable claim is the platform owner's decision, not a default.
func statefulUnit(name string) string {
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
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: ["ReadWriteOnce"]
`, name, strings.Repeat("a", 64))
}

func TestDeployedRenderAcceptsAConformingTree(t *testing.T) {
	result, _, err := renderWorkloads(t, meshedStaging(), durableContracts("api", "store"), true, map[string]string{
		"api": conformingUnit("api"), "store": conformingUnit("store"),
	})
	require.NoError(t, err)
	require.Empty(t, result.PostureAllowances)
	require.NotEmpty(t, result.Inventory.Digest)
}

func TestDeployedRenderRefusesAnythingButTheStandardScratchVolume(t *testing.T) {
	unit := strings.Replace(conformingUnit("api"), `        - name: tmp
          emptyDir: {}
`, `        - name: tmp
          emptyDir: {}
        - name: config
          configMap:
            name: api-config
`, 1)
	_, destination, err := renderWorkloads(t, meshedStaging(), durableContracts("api"), true,
		map[string]string{"api": unit})
	require.ErrorContains(t, err, "deployed render refuses service shop/api")
	require.ErrorContains(t, err, posture.RuleNonScratchMount)
	require.ErrorContains(t, err, `volume "config" (configMap) is not declared by service shop/api`)
	_, statErr := os.Stat(destination)
	require.True(t, os.IsNotExist(statErr), "a refused render installs nothing")
}

// Rule 1 is decided by what the render delivers, not by anything a container does
// with it.
func TestDeployedRenderRefusesCertificateMaterialItDelivers(t *testing.T) {
	material := `apiVersion: v1
kind: ConfigMap
metadata:
  name: api-trust
  namespace: acme
data:
  ca.crt: |
    -----BEGIN CERTIFICATE-----
`
	_, _, err := renderWorkloads(t, meshedStaging(), durableContracts("api"), true,
		map[string]string{"api": material})
	require.ErrorContains(t, err, "deployed render refuses service shop/api")
	require.ErrorContains(t, err, posture.RulePeerTransportMaterial)
	require.ErrorContains(t, err, "data.ca.crt")

	_, _, err = renderWorkloads(t, &posture.Declaration{}, durableContracts("api"), true,
		map[string]string{"api": material})
	require.NoError(t, err, "without the mesh assertion this material is the only transport protection there is")
}

// Rule 3 is decided by the declaration: a store that has not declared is refused.
func TestDeployedRenderRefusesAStoreThatDeclaresNoStorageMode(t *testing.T) {
	allowance := posture.Allowance{
		Rule: posture.RuleNonScratchMount, Service: "shop/store", Reason: "a durable data claim, reviewed",
	}
	_, _, err := renderWorkloads(t, meshedStaging(allowance), posture.Contracts{}, true,
		map[string]string{"store": statefulUnit("store")})
	require.ErrorContains(t, err, "deployed render refuses service shop/store")
	require.ErrorContains(t, err, posture.RuleInMemoryStateStore)
	require.ErrorContains(t, err, "declares no storage mode")

	_, _, err = renderWorkloads(t, meshedStaging(allowance), durableContracts("store"), true,
		map[string]string{"store": statefulUnit("store")})
	require.NoError(t, err, "a declared durable store renders")
}

// A render that does not target a cell is not held to the posture.
func TestARenderThatIsNotForACellIsNotHeldToThePosture(t *testing.T) {
	unit := strings.Replace(conformingUnit("api"), `        - name: tmp
          emptyDir: {}
`, `        - name: config
          configMap:
            name: api-config
`, 1)
	_, _, err := renderWorkloads(t, meshedStaging(), posture.Contracts{}, false,
		map[string]string{"api": unit, "store": statefulUnit("store")})
	require.NoError(t, err)
}

func TestADeclaredAllowanceIsHonouredAndPrintedOnEveryRun(t *testing.T) {
	declaration := meshedStaging(
		posture.Allowance{
			Rule: posture.RuleNonScratchMount, Service: "shop/api",
			Reason: "the asset bundle is delivered as a ConfigMap, reviewed by the platform owner",
		},
		posture.Allowance{
			Rule: posture.RulePeerTransportMaterial, Service: "shop/store",
			Reason: "an external peer pins its own CA",
		},
	)
	unit := strings.Replace(conformingUnit("api"), `        - name: tmp
          emptyDir: {}
`, `        - name: tmp
          emptyDir: {}
        - name: config
          configMap:
            name: api-config
`, 1)
	result, _, err := renderWorkloads(t, declaration, durableContracts("api", "store"), true, map[string]string{
		"api": unit, "store": conformingUnit("store"),
	})
	require.NoError(t, err)
	require.Equal(t, []string{
		"security posture: service shop/api is allowed to break rule non-scratch-mount — " +
			"the asset bundle is delivered as a ConfigMap, reviewed by the platform owner",
		"security posture: service shop/store is allowed to break rule peer-transport-material — " +
			"an external peer pins its own CA",
	}, result.PostureAllowances)
}

// Every entry point must carry the environment's posture AND the declarations of
// the services it renders. Each case here fails when either is dropped at that
// entry point.
func TestEveryRenderEntryPointCarriesTheEnvironmentPosture(t *testing.T) {
	// A ConfigMap of certificate material: mesh-dependent, so losing the posture
	// declaration (asserts and allowances both) changes the verdict.
	material := func(service string) map[string]string {
		if service != "store" {
			return nil
		}
		return map[string]string{
			filepath.Join("base", "trust.yaml"): `apiVersion: v1
kind: ConfigMap
metadata:
  name: store-trust
  namespace: acme
data:
  ca.crt: |
    -----BEGIN CERTIFICATE-----
`,
			filepath.Join("base", "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - stateful-set.yaml\n  - service.yaml\n  - trust.yaml\n",
		}
	}
	t.Run("a module render", func(t *testing.T) {
		installFakeAgents(t)
		t.Cleanup(func() { fakeAgentExtraFiles = nil })
		fakeAgentExtraFiles = material
		workspace, module, env := posturedFixture(t, meshBlock)
		_, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
		require.ErrorContains(t, err, "deployed render refuses service shop/store")
		require.ErrorContains(t, err, posture.RulePeerTransportMaterial)
	})
	t.Run("a module render honours the allowance", func(t *testing.T) {
		installFakeAgents(t)
		t.Cleanup(func() { fakeAgentExtraFiles = nil })
		fakeAgentExtraFiles = material
		workspace, module, env := posturedFixture(t, meshBlock+`      allowances:
        - rule: peer-transport-material
          service: shop/store
          reason: an external peer pins its own CA
`)
		result, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
		require.NoError(t, err)
		require.Equal(t, []string{
			"security posture: service shop/store is allowed to break rule peer-transport-material — an external peer pins its own CA",
		}, result.PostureAllowances)
	})
	t.Run("a single-service render", func(t *testing.T) {
		installFakeAgents(t)
		t.Cleanup(func() { fakeAgentExtraFiles = nil })
		fakeAgentExtraFiles = material
		workspace, module, env := posturedFixture(t, meshBlock)
		api, err := module.LoadServiceFromName(context.Background(), "api")
		require.NoError(t, err)
		_, err = RenderService(context.Background(), workspace, module, api, env, "acme-staging", false, nil)
		require.ErrorContains(t, err, "deployed render refuses service shop/store",
			"a service render stages its dependency graph, and every unit it stages is held to the posture")
		require.ErrorContains(t, err, posture.RulePeerTransportMaterial)
	})
	t.Run("a single-service render honours the allowance", func(t *testing.T) {
		installFakeAgents(t)
		t.Cleanup(func() { fakeAgentExtraFiles = nil })
		fakeAgentExtraFiles = material
		workspace, module, env := posturedFixture(t, meshBlock+`      allowances:
        - rule: peer-transport-material
          service: shop/store
          reason: an external peer pins its own CA
`)
		api, err := module.LoadServiceFromName(context.Background(), "api")
		require.NoError(t, err)
		_, err = RenderService(context.Background(), workspace, module, api, env, "acme-staging", false, nil)
		require.NoError(t, err, "the service render carries the environment's allowances too")
	})
	// The contracts are the other half of what an entry point must carry: a
	// declaration the render never read is a declaration the render cannot enforce.
	t.Run("a module render carries the service declarations", func(t *testing.T) {
		installFakeAgents(t)
		workspace, module, env := declaringFixture(t, "store", ephemeralStorageBlock)
		_, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
		require.ErrorContains(t, err, "deployed render refuses service shop/store")
		require.ErrorContains(t, err, posture.RuleInMemoryStateStore)
		require.ErrorContains(t, err, "declares ephemeral storage")
	})
	t.Run("a single-service render carries the service declarations", func(t *testing.T) {
		installFakeAgents(t)
		workspace, module, env := declaringFixture(t, "store", ephemeralStorageBlock)
		api, err := module.LoadServiceFromName(context.Background(), "api")
		require.NoError(t, err)
		_, err = RenderService(context.Background(), workspace, module, api, env, "acme-staging", false, nil)
		require.ErrorContains(t, err, "deployed render refuses service shop/store",
			"a dependency's declaration is read too: it is part of what this render stages")
		require.ErrorContains(t, err, posture.RuleInMemoryStateStore)
	})
	t.Run("a solution render", func(t *testing.T) {
		_, _, env := posturedFixture(t, meshBlock)
		options := solutionRenderOptions(
			&SolutionRenderRequest{Name: "checkout", Environment: env, AppProject: "acme-staging"},
			env, "/tmp/destination", "deployments/modules/checkout", "acme-checkout")
		require.True(t, options.deployedRender())
		require.Same(t, env.Posture, options.Posture)
		require.True(t, options.Posture.Asserted(posture.AssertMeshProtectedTransport))
	})
}

func TestAnUnknownPostureRuleIsRefusedWhenTheEnvironmentIsRead(t *testing.T) {
	_, _, _, err := posturedFixtureE(t, `    posture:
      allowances:
        - rule: no-mounts-at-all
          service: shop/store
          reason: reviewed
`)
	require.ErrorContains(t, err, `posture allowance 1 names unknown rule "no-mounts-at-all"`)
}

// A dev deployment ships the image of a render made here and now, so that render
// is held to the posture — through the same selector, for the same environment.
func TestDevServiceBuildIsHeldToTheDeployedPosture(t *testing.T) {
	installFakeAgents(t)
	t.Cleanup(func() { fakeAgentExtraFiles = nil })
	workspace, module, env := posturedFixture(t, meshBlock)
	store, err := module.LoadServiceFromName(context.Background(), "store")
	require.NoError(t, err)

	images, err := buildRenderedServiceImages(context.Background(), workspace, module, store, env, false, nil)
	require.NoError(t, err)
	require.NotEmpty(t, images, "a conforming dev render reports the images it pinned")

	fakeAgentExtraFiles = func(service string) map[string]string {
		if service != "store" {
			return nil
		}
		return map[string]string{
			filepath.Join("base", "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - stateful-set.yaml\n  - service.yaml\n  - mount.yaml\n",
			filepath.Join("base", "mount.yaml"): `apiVersion: apps/v1
kind: Deployment
metadata:
  name: store-sidecar
  namespace: acme
spec:
  template:
    spec:
      containers:
        - name: sidecar
      volumes:
        - name: credentials
          secret:
            secretName: store-credentials
`,
		}
	}
	_, err = buildRenderedServiceImages(context.Background(), workspace, module, store, env, false, nil)
	require.ErrorContains(t, err, "deployed render refuses service shop/store")
	require.ErrorContains(t, err, posture.RuleNonScratchMount)
}

// The dev build reads the requested environment's overlay: a violation only that
// overlay carries is still found, and one only another environment carries is not.
func TestDevServiceBuildReadsTheRequestedEnvironmentsOverlay(t *testing.T) {
	installFakeAgents(t)
	t.Cleanup(func() { fakeAgentExtraFiles = nil })
	fakeAgentExtraFiles = func(service string) map[string]string {
		if service != "store" {
			return nil
		}
		return map[string]string{
			filepath.Join("overlays", "staging", "kustomization.yaml"): "resources:\n  - ../../base\n  - mount.yaml\n",
			filepath.Join("overlays", "staging", "mount.yaml"): `apiVersion: apps/v1
kind: Deployment
metadata:
  name: store-sidecar
  namespace: acme
spec:
  template:
    spec:
      containers:
        - name: sidecar
      volumes:
        - name: credentials
          secret:
            secretName: store-credentials
`,
		}
	}
	workspace, module, env := posturedFixture(t, meshBlock)
	store, err := module.LoadServiceFromName(context.Background(), "store")
	require.NoError(t, err)
	_, err = buildRenderedServiceImages(context.Background(), workspace, module, store, env, false, nil)
	require.ErrorContains(t, err, posture.RuleNonScratchMount,
		"the violation is in the overlay this build renders for")
}

// A violation another environment's overlay carries is not this build's.
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
kind: Deployment
metadata:
  name: store-development
  namespace: acme
spec:
  template:
    spec:
      containers:
        - name: store
      volumes:
        - name: credentials
          secret:
            secretName: store-credentials
`,
		}
	}
	workspace, module, env := posturedFixture(t, meshBlock)
	store, err := module.LoadServiceFromName(context.Background(), "store")
	require.NoError(t, err)
	images, err := buildRenderedServiceImages(context.Background(), workspace, module, store, env, false, nil)
	require.NoError(t, err, "another environment's overlay is not this build's manifests")
	require.NotEmpty(t, images)
}

// A dev build reads what the services it renders declared about themselves, so a
// store whose declaration says its state is ephemeral is refused here too.
func TestDevServiceBuildCarriesTheServiceDeclarations(t *testing.T) {
	installFakeAgents(t)
	workspace, module, env := declaringFixture(t, "store", ephemeralStorageBlock)
	store, err := module.LoadServiceFromName(context.Background(), "store")
	require.NoError(t, err)
	_, err = buildRenderedServiceImages(context.Background(), workspace, module, store, env, false, nil)
	require.ErrorContains(t, err, "deployed render refuses service shop/store")
	require.ErrorContains(t, err, posture.RuleInMemoryStateStore)
	require.ErrorContains(t, err, "declares ephemeral storage")
}
