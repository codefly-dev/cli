package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

func managedIdentityService() environments.EnvironmentManagedService {
	return environments.EnvironmentManagedService{
		Kind:         "external",
		ExternalName: "10.20.11.7",
		Port:         5432,
		EgressCIDRs:  []string{"10.20.11.0/28"},
		Identity: &environments.EnvironmentWorkloadIdentity{
			Kind:        "gcp-service-account",
			Principal:   "platform-db@obinh.iam.gserviceaccount.com",
			Annotations: map[string]string{"iam.gke.io/gcp-service-account": "platform-db@obinh.iam.gserviceaccount.com"},
			Labels:      map[string]string{"obin.ai/workload-identity": "true"},
		},
	}
}

func storeConsumer(name string) *resources.Service {
	return &resources.Service{
		Name:                name,
		ServiceDependencies: []*resources.ServiceDependency{{Name: "store"}},
	}
}

// writeConsumerTree renders the shape a service agent emits for a consuming
// service: a base holding the ConfigMap with the dependency's endpoint address
// and the Deployment, and an environment overlay pointing at it.
func writeConsumerTree(t *testing.T, root, environment, namespace, service, endpointAddress string) {
	t.Helper()
	base := filepath.Join(root, "base")
	overlay := filepath.Join(root, "overlays", environment)
	for _, dir := range []string{base, overlay} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configMap := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + service +
		"\n  namespace: " + namespace + "\ndata:\n" +
		"  CODEFLY__ENDPOINT__PAYMENTS__STORE__TCP__TCP: \"" + endpointAddress + "\"\n" +
		"  CODEFLY__SERVICE: \"" + service + "\"\n"
	deployment := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + service +
		"\n  namespace: " + namespace + "\nspec:\n  template:\n    metadata:\n      labels:\n        app: " + service +
		"\n    spec:\n      containers:\n        - name: " + service +
		"\n          image: registry.example.com/" + service + "@sha256:" + strings.Repeat("a", 64) + "\n"
	files := map[string]string{
		filepath.Join(base, "config-map.yaml"): configMap,
		filepath.Join(base, "deployment.yaml"): deployment,
		filepath.Join(base, "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
			"resources:\n  - config-map.yaml\n  - deployment.yaml\n",
		filepath.Join(overlay, "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func consumerEnvironment(managed environments.EnvironmentManagedService) *environments.Environment {
	return &environments.Environment{
		Name:            "production",
		Namespace:       "payments",
		ManagedServices: map[string]environments.EnvironmentManagedService{"store": managed},
	}
}

// buildOverlay runs kustomize over a rendered service overlay so assertions read
// what the cluster would receive rather than one file on disk.
func buildOverlay(t *testing.T, root, environment string) []manifest {
	t.Helper()
	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rendered, err := kustomizer.Run(filesys.MakeFsOnDisk(), filepath.Join(root, "overlays", environment))
	if err != nil {
		t.Fatalf("build overlay: %v", err)
	}
	encoded, err := rendered.AsYaml()
	if err != nil {
		t.Fatal(err)
	}
	manifests, _, err := decodeYAML("overlay.yaml", encoded)
	if err != nil {
		t.Fatal(err)
	}
	return manifests
}

func manifestOfKind(t *testing.T, manifests []manifest, kind string) manifest {
	t.Helper()
	for _, item := range manifests {
		if item.kind == kind {
			return item
		}
	}
	t.Fatalf("rendered overlay carries no %s", kind)
	return manifest{}
}

func TestProjectManagedIdentityProjectsWithoutRewritingEndpoints(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")

	if err := projectManagedIdentity(
		context.Background(), root, storeConsumer("accounts"), consumerEnvironment(managedIdentityService()),
	); err != nil {
		t.Fatal(err)
	}

	rendered := buildOverlay(t, root, "production")

	configMap := manifestOfKind(t, rendered, "ConfigMap")
	data, _ := configMap.value["data"].(map[string]any)
	if got := data["CODEFLY__ENDPOINT__PAYMENTS__STORE__TCP__TCP"]; got != "store.payments.svc:5432" {
		t.Errorf("dial address = %v, want the original rendered endpoint", got)
	}
	if got := data["CODEFLY__SERVICE"]; got != "accounts" {
		t.Errorf("unrelated configuration was rewritten: %v", got)
	}

	deployment := manifestOfKind(t, rendered, kindDeployment)
	spec, ok := podSpec(deployment)
	if !ok {
		t.Fatal("rendered Deployment carries no pod spec")
	}
	containers := sliceField(spec, "containers")
	if len(containers) != 1 {
		t.Fatalf("identity projection added containers: %d", len(containers))
	}

	// The identity the cell declared keys the platform's webhook, so it has to
	// reach the ServiceAccount and the pod template, not just the manifest tree.
	serviceAccount := manifestOfKind(t, rendered, "ServiceAccount")
	metadata, _ := serviceAccount.value["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations["iam.gke.io/gcp-service-account"] != "platform-db@obinh.iam.gserviceaccount.com" {
		t.Errorf("ServiceAccount annotations = %v", annotations)
	}
	if spec["serviceAccountName"] != "accounts" {
		t.Errorf("pod serviceAccountName = %v", spec["serviceAccountName"])
	}
	template, _ := mapField(deployment.value, "spec")["template"].(map[string]any)
	podLabels, _ := mapField(template, "metadata")["labels"].(map[string]any)
	if podLabels["obin.ai/workload-identity"] != "true" {
		t.Errorf("pod labels = %v, want the declared identity label", podLabels)
	}
}

// TestProjectManagedIdentityRefusesConflictingIdentities covers the one case a
// pod cannot express: it runs under a single ServiceAccount, so two managed
// endpoints naming different principals would silently leave one unauthenticated.
func TestProjectManagedIdentityRefusesConflictingIdentities(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")

	warehouse := managedIdentityService()
	warehouse.Identity = &environments.EnvironmentWorkloadIdentity{Principal: "warehouse@obinh.iam.gserviceaccount.com"}
	env := consumerEnvironment(managedIdentityService())
	env.ManagedServices["warehouse"] = warehouse

	service := storeConsumer("accounts")
	service.ServiceDependencies = append(service.ServiceDependencies, &resources.ServiceDependency{Name: "warehouse"})

	err := projectManagedIdentity(context.Background(), root, service, env)
	if err == nil || !strings.Contains(err.Error(), "authenticates as one") {
		t.Fatalf("err = %v, want a refusal naming the conflict", err)
	}

	// Two endpoints reached as the same principal are one identity, and render
	// as one rather than being refused.
	sameIdentity := managedIdentityService()
	env.ManagedServices["warehouse"] = sameIdentity
	if err = projectManagedIdentity(context.Background(), root, service, env); err != nil {
		t.Fatalf("two endpoints with one principal were refused: %v", err)
	}
}

// TestProjectManagedIdentityLeavesNonConsumersAlone pins that the projection
// follows the declared dependency graph: a service that never dials the managed
// endpoint does not acquire the endpoint's identity.
func TestProjectManagedIdentityLeavesNonConsumersAlone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "frontend")
	writeConsumerTree(t, root, "production", "payments", "frontend", "store.payments.svc:5432")
	before := readTree(t, root)

	if err := projectManagedIdentity(
		context.Background(), root, &resources.Service{Name: "frontend"}, consumerEnvironment(managedIdentityService()),
	); err != nil {
		t.Fatal(err)
	}
	if after := readTree(t, root); after != before {
		t.Errorf("tree of a non-consuming service changed:\n%s", after)
	}
}

// A service without a declared identity retains the renderer-owned configuration.
func TestProjectManagedIdentityLeavesAbsentIdentityAlone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")
	before := readTree(t, root)

	legacy := environments.EnvironmentManagedService{
		Kind:         "external",
		ExternalName: "p.postgres.database.azure.com",
		EgressCIDRs:  []string{"10.20.11.0/28"},
	}
	if err := projectManagedIdentity(
		context.Background(), root, storeConsumer("accounts"), consumerEnvironment(legacy),
	); err != nil {
		t.Fatal(err)
	}
	if after := readTree(t, root); after != before {
		t.Errorf("tree changed for a managed service that declares no identity:\n%s", after)
	}
}

func readTree(t *testing.T, root string) string {
	t.Helper()
	var contents strings.Builder
	err := walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		contents.WriteString(relative + "\n" + string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return contents.String()
}

// TestProjectManagedIdentityIgnoresBuildOnlyEdge pins that the projection
// follows what a pod dials, not what a toolchain reads: a build-stage edge on a
// managed service is consumed by codegen, so stamping its identity
// onto the workload would authenticate a container that never connects.
func TestProjectManagedIdentityIgnoresBuildOnlyEdge(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")
	before := readTree(t, root)

	service := &resources.Service{
		Name:                "accounts",
		ServiceDependencies: []*resources.ServiceDependency{{Name: "store", Kind: resources.DependencyKindBuild}},
	}
	if err := projectManagedIdentity(
		context.Background(), root, service, consumerEnvironment(managedIdentityService()),
	); err != nil {
		t.Fatal(err)
	}
	if after := readTree(t, root); after != before {
		t.Errorf("tree changed for a build-only edge:\n%s", after)
	}
}
