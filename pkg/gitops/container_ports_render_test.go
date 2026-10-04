package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	coreservices "github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

// containerPortsFixture copies testdata/container-ports — module "shop", a
// go-grpc-shaped "api" with a named "authority" gRPC endpoint that depends on
// a postgres-shaped "store" — into a fresh workspace, appending declarations to
// a service's spec.deployment.endpoint-ports.
func containerPortsFixture(t *testing.T, declarations map[string]string) (*resources.Workspace, *resources.Module, *environments.Environment) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, copyTree(filepath.Join("testdata", "container-ports"), root))
	for service, block := range declarations {
		path := filepath.Join(root, "modules", "shop", "services", service, resources.ServiceConfigurationName)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(data, []byte(block)...), 0o644))
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	env, err := orchestration.SelectEnvironment(workspace, "staging")
	require.NoError(t, err)
	return workspace, module, env
}

// installFakeAgents stands in-process agents in for the service flow. Each one
// is handed the network mappings the real Builder.Deploy hands an agent — the
// same RemoteManager.GenerateNetworkMappings — and writes the kustomize tree
// its real agent's templates write: go-grpc publishes every listener with
// targetPort equal to its port and binds a named endpoint on the port it was
// handed; postgres publishes the handed port and keeps 5432 behind it.
func installFakeAgents(t *testing.T) {
	t.Helper()
	previous := serviceFlow
	t.Cleanup(func() { serviceFlow = previous })
	serviceFlow = func(
		ctx context.Context,
		workspace *resources.Workspace,
		module *resources.Module,
		root *resources.Service,
		env *environments.Environment,
		_ renderBuild,
		_ orchestration.OutputSink,
		destination func(*resources.Module, *resources.Service) string,
		record func(map[string]*builderv0.DeploymentOutput),
		recordServices func(map[string]*resources.Service),
		recordSelfEndpoints func(map[string]map[string]string),
		recordConfigurations func(map[string]*basev0.Configuration, map[string][]string),
		recordInClusterPorts func(map[string]map[string]uint32),
	) error {
		remote, err := orchestration.NewEnvironmentRemoteManager(ctx, workspace)
		if err != nil {
			return err
		}
		graph := []*resources.Service{root}
		for _, dependency := range root.ServiceDependencies {
			loaded, loadErr := module.LoadServiceFromName(ctx, dependency.Name)
			if loadErr != nil {
				return loadErr
			}
			graph = append(graph, loaded)
		}
		outputs := map[string]*builderv0.DeploymentOutput{}
		services := map[string]*resources.Service{}
		ports := map[string]map[string]uint32{}
		for _, service := range graph {
			identity, identityErr := service.Identity()
			if identityErr != nil {
				return identityErr
			}
			endpoints, endpointsErr := service.LoadEndpoints(ctx)
			if endpointsErr != nil {
				return endpointsErr
			}
			mappings, mappingErr := remote.GenerateNetworkMappings(ctx, env, workspace, identity, endpoints)
			if mappingErr != nil {
				return mappingErr
			}
			files, renderErr := fakeAgentTree(ctx, service, env, mappings)
			if renderErr != nil {
				return renderErr
			}
			writeFiles(t, destination(module, service), files)
			unique := resources.ServiceUnique(module.Name, service.Name)
			outputs[unique] = fakeRestrictedOutput()
			services[unique] = service
			ports[unique] = orchestration.InClusterPorts(ctx, mappings)
		}
		if record != nil {
			record(outputs)
		}
		if recordServices != nil {
			recordServices(services)
		}
		if recordSelfEndpoints != nil {
			recordSelfEndpoints(map[string]map[string]string{})
		}
		if recordConfigurations != nil {
			recordConfigurations(map[string]*basev0.Configuration{}, map[string][]string{})
		}
		if recordInClusterPorts != nil {
			recordInClusterPorts(ports)
		}
		return nil
	}
}

// fakeRestrictedOutput is what a real agent emits: the RESTRICTED security
// property. It set only `promotable` — the delivery decision — which no real
// agent could get past the builder gate, because that gate requires
// restricted. This double replaces serviceFlow wholesale, so it never met the
// gate and the inventory it produced was installed unvalidated.
func fakeRestrictedOutput() *builderv0.DeploymentOutput {
	return &builderv0.DeploymentOutput{Kind: &builderv0.DeploymentOutput_Kubernetes{Kubernetes: &builderv0.KubernetesDeploymentOutput{
		Kind:            builderv0.KubernetesDeploymentOutput_KUSTOMIZE,
		Profile:         builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1,
		ContractVersion: coreservices.KubernetesManifestContractVersion,
		Validation: &builderv0.KubernetesManifestValidation{
			StaticValidation:     builderv0.KubernetesManifestValidation_STATUS_PASSED,
			ServerSideValidation: builderv0.KubernetesManifestValidation_STATUS_NOT_RUN,
			Restricted:           true,
		},
	}}}
}

// fakeAgentTree is the base and overlay one agent writes for a service.
func fakeAgentTree(ctx context.Context, service *resources.Service, env *environments.Environment, mappings []*basev0.NetworkMapping) (map[string]string, error) {
	image := fmt.Sprintf("registry.example.com/acme/%s@sha256:%s", service.Name, strings.Repeat("a", 64))
	var workload, workloadFile, servicePorts string
	switch service.Agent.Name {
	case "go-grpc":
		servicePorts, workload = fakeGoGrpcPorts(ctx, service, mappings)
		workloadFile = "deployment.yaml"
		workload = fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: acme
spec:
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      containers:
        - name: %[1]s
          image: %[2]s
          ports:
%[3]s`, service.Name, image, workload)
	case "postgres":
		port := uint32(5432)
		for _, mapping := range mappings {
			if instance := containerInstance(ctx, mappings, mapping.GetEndpoint()); instance != nil {
				port = instance.GetPort()
			}
		}
		servicePorts = fmt.Sprintf("    - name: postgres\n      port: %d\n      targetPort: 5432\n", port)
		workloadFile = "stateful-set.yaml"
		workload = fmt.Sprintf(`apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: %[1]s
  namespace: acme
spec:
  serviceName: %[1]s
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      containers:
        - name: %[1]s
          image: %[2]s
          ports:
            - name: postgres
              containerPort: 5432
`, service.Name, image)
	default:
		return nil, fmt.Errorf("no fake agent for %s", service.Agent.Name)
	}
	return map[string]string{
		filepath.Join("base", "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - " +
			workloadFile + "\n  - service.yaml\n",
		filepath.Join("base", workloadFile): workload,
		filepath.Join("base", "service.yaml"): fmt.Sprintf("apiVersion: v1\nkind: Service\nmetadata:\n  name: %[1]s\n  namespace: acme\n"+
			"spec:\n  selector:\n    app: %[1]s\n  ports:\n%[2]s", service.Name, servicePorts),
		filepath.Join("overlays", env.Name, "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n",
	}, nil
}

// fakeGoGrpcPorts mirrors service-go-grpc's templates: grpc on 9090 always,
// http on 8080 when rest is enabled, and every other endpoint on the
// in-cluster port it was handed, as both the Service port and the target.
func fakeGoGrpcPorts(ctx context.Context, service *resources.Service, mappings []*basev0.NetworkMapping) (string, string) {
	servicePorts := "    - protocol: TCP\n      name: grpc-port\n      port: 9090\n      targetPort: 9090\n"
	containerPorts := "            - name: grpc\n              containerPort: 9090\n"
	if enabled, _ := service.Spec["rest-endpoint"].(bool); enabled {
		servicePorts += "    - protocol: TCP\n      name: http-port\n      port: 8080\n      targetPort: 8080\n"
		containerPorts += "            - name: http\n              containerPort: 8080\n"
	}
	named := make([]string, 0, len(mappings))
	byName := map[string]uint32{}
	for _, mapping := range mappings {
		endpoint := mapping.GetEndpoint()
		if endpoint.GetName() == standards.GRPC || endpoint.GetName() == standards.REST || endpoint.GetName() == standards.CONNECT {
			continue
		}
		if instance := containerInstance(ctx, mappings, endpoint); instance != nil {
			name := endpoint.GetApi() + "-" + endpoint.GetName()
			named = append(named, name)
			byName[name] = instance.GetPort()
		}
	}
	sort.Strings(named)
	for _, name := range named {
		servicePorts += fmt.Sprintf("    - protocol: TCP\n      name: %s\n      port: %d\n      targetPort: %d\n", name, byName[name], byName[name])
		containerPorts += fmt.Sprintf("            - containerPort: %d\n", byName[name])
	}
	return servicePorts, containerPorts
}

func containerInstance(ctx context.Context, mappings []*basev0.NetworkMapping, endpoint *basev0.Endpoint) *basev0.NetworkInstance {
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, mappings, endpoint, resources.NewContainerNetworkAccess())
	if err != nil {
		return nil
	}
	return instance
}

// treeDigest hashes every file of a render, path and bytes, in path order.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	files := snapshotTree(t, root)
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		fmt.Fprintf(hash, "%s\x00%d\x00%s", path, len(files[path]), files[path])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// undeclaredFixtureDigest is treeDigest of the fixture's module render with
// no declaration. A render that declares nothing must stay byte-identical.
//
// It was captured on main before the container-port check existed (b990c162,
// with only the serviceFlow seam added) as
// "aaca0799de4ebe4f9afa38c7cb84594a63b7b2dfd984251de5d90c4fd03cee0d", and
// re-captured when the render inventory schema went from 5 to 6 for declared
// solution presence, again from 6 to 7 when publish took over settling and
// signing the delivery documents, and once more when the output profile the
// render requests and records became RESTRICTED_PORTABLE_V1 (the deprecated
// PROMOTABLE_GITOPS_V1 retired). Each bump is the ONLY difference: a module
// with no solution instance renders the same tree, byte for byte, with one
// field changed in .codefly-render.json.
const undeclaredFixtureDigest = "702022b9cb5b9174cbe00b48c0918a52fb6c4f381766231d3fd94ea3ca999f3c"

func TestRenderModuleWithoutDeclarationsIsByteIdentical(t *testing.T) {
	installFakeAgents(t)
	workspace, module, env := containerPortsFixture(t, nil)
	result, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
	require.NoError(t, err)
	require.Equal(t, undeclaredFixtureDigest, treeDigest(t, result.Path))
}

// The allocated port of api's named "authority" endpoint in module "shop":
// the go-grpc agent binds it as the container port too.
const authorityPort = 19043

func TestRenderModuleRefusesADeclarationDisagreeingWithTheAgentsContainerPort(t *testing.T) {
	installFakeAgents(t)
	workspace, module, env := containerPortsFixture(t, map[string]string{
		"api": "  deployment:\n    endpoint-ports:\n      authority: 9091\n      grpc: 9090\n",
	})
	_, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "service shop/api: spec.deployment.endpoint-ports disagrees with the manifests its agent rendered for environment staging:\n"+
		fmt.Sprintf("  authority = 9091, but its pods listen on container port %d (the Service publishes port %d with targetPort %d)", authorityPort, authorityPort, authorityPort))
	require.NotContains(t, err.Error(), "grpc = 9090", "a matching declaration is not reported")
	_, statErr := os.Stat(moduleRenderDestination(workspace, "shop"))
	require.True(t, os.IsNotExist(statErr), "a refused render installs nothing")
}

func TestRenderModuleAcceptsDeclarationsMatchingTheContainerPorts(t *testing.T) {
	installFakeAgents(t)
	workspace, module, env := containerPortsFixture(t, map[string]string{
		// The postgres agent publishes the allocated port 80 and targets 5432.
		"store": "  deployment:\n    endpoint-ports:\n      tcp: 5432\n",
		"api":   fmt.Sprintf("  deployment:\n    endpoint-ports:\n      authority: %d\n      grpc: 9090\n      rest: 8080\n", authorityPort),
	})
	result, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
	require.NoError(t, err)
	require.Equal(t, undeclaredFixtureDigest, treeDigest(t, result.Path), "a declaration is checked, never rendered")
}

func TestRenderModuleRefusesADeclarationNamingNoInClusterEndpoint(t *testing.T) {
	installFakeAgents(t)
	workspace, module, env := containerPortsFixture(t, map[string]string{
		"store": "  deployment:\n    endpoint-ports:\n      metrics: 9187\n      tcp: 5432\n",
	})
	_, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
	require.ErrorContains(t, err, "service shop/store: spec.deployment.endpoint-ports disagrees with the manifests its agent rendered for environment staging:\n"+
		"  metrics = 9187 names an endpoint that is not rendered in-cluster in environment staging (in-cluster endpoints: tcp)")
}

// A single-service render checks every service of the graph it stages: here
// the store dependency of the api it renders.
func TestRenderServiceRefusesADependencysDisagreeingDeclaration(t *testing.T) {
	installFakeAgents(t)
	workspace, module, env := containerPortsFixture(t, map[string]string{
		"store": "  deployment:\n    endpoint-ports:\n      tcp: 80\n",
	})
	api, err := module.LoadServiceFromName(context.Background(), "api")
	require.NoError(t, err)
	_, err = RenderService(context.Background(), workspace, module, api, env, "acme-staging", false, nil)
	require.ErrorContains(t, err, "service shop/store: spec.deployment.endpoint-ports disagrees with the manifests its agent rendered for environment staging:\n"+
		"  tcp = 80, but its pods listen on container port 5432 (the Service publishes port 80 with targetPort 5432)")
}
