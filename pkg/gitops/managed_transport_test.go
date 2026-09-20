package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

const proxyImage = "us-central1-docker.pkg.dev/obinh/images/db-proxy@sha256:" + sixtyFourHex

const sixtyFourHex = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// managedIdentityService is the shape core's ToEnvironment produces for a
// passwordless database reached through an in-pod proxy: a port, a proxy
// transport and a runtime identity, and deliberately no secret references.
func managedIdentityService() resources.EnvironmentManagedService {
	return resources.EnvironmentManagedService{
		Kind:         "cloud-sql-postgres",
		ExternalName: "10.20.11.7",
		Port:         5432,
		EgressCIDRs:  []string{"10.20.11.0/28"},
		Transport: &resources.EnvironmentManagedTransport{
			Mode:      resources.TransportModeProxy,
			Image:     proxyImage,
			Args:      []string{"--private-ip", "--auto-iam-authn", "obinh:us-central1:platform"},
			LocalPort: 5432,
		},
		Identity: &resources.EnvironmentWorkloadIdentity{
			Kind:        "gcp-service-account",
			Principal:   "platform-db@obinh.iam.gserviceaccount.com",
			Annotations: map[string]string{"iam.gke.io/gcp-service-account": "platform-db@obinh.iam.gserviceaccount.com"},
			Labels:      map[string]string{"obin.ai/workload-identity": "true"},
		},
	}
}

func ptr(managed resources.EnvironmentManagedService) *resources.EnvironmentManagedService {
	return &managed
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

func consumerEnvironment(managed resources.EnvironmentManagedService) *resources.Environment {
	return &resources.Environment{
		Name:            "production",
		Namespace:       "payments",
		ManagedServices: map[string]resources.EnvironmentManagedService{"store": managed},
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

func TestManagedEgressPolicyTakesPortAndPeersFromTheContract(t *testing.T) {
	policy, err := managedEgressPolicy("store", "payments", ptr(managedIdentityService()))
	if err != nil {
		t.Fatal(err)
	}
	if policy == nil {
		t.Fatal("declared port and egress CIDRs rendered no policy")
	}
	if policy.Metadata.Namespace != "payments" || policy.Metadata.Name != "egress-store" {
		t.Errorf("policy metadata = %+v", policy.Metadata)
	}
	if len(policy.Spec.Egress) != 1 {
		t.Fatalf("policy egress rules = %d, want 1", len(policy.Spec.Egress))
	}
	rule := policy.Spec.Egress[0]
	if len(rule.Ports) != 1 || rule.Ports[0].Port != 5432 || rule.Ports[0].Protocol != "TCP" {
		t.Errorf("policy ports = %+v, want the declared 5432/TCP", rule.Ports)
	}
	if len(rule.To) != 1 || rule.To[0].IPBlock.CIDR != "10.20.11.0/28" {
		t.Errorf("policy peers = %+v, want the declared egress CIDR", rule.To)
	}
}

// TestManagedEgressPolicyRefusesTransportBindingWithoutPort covers the rule that
// separates this from an engine-keyed default: a declaration that says how the
// endpoint is reached but not on which port has no policy codefly can render, and
// guessing 5432 from the engine would open a rule that drops every connection.
func TestManagedEgressPolicyRefusesTransportBindingWithoutPort(t *testing.T) {
	managed := managedIdentityService()
	managed.Port = 0
	if _, err := managedEgressPolicy("store", "payments", &managed); err == nil ||
		!strings.Contains(err.Error(), "no port") {
		t.Fatalf("err = %v, want a refusal naming the missing port", err)
	}
}

// TestManagedEgressPolicyLeavesLegacyEntryAlone pins that a password-auth entry
// predating the transport contract — egress CIDRs, no port, no binding — renders
// no policy instead of being refused, so importing an older cell keeps working.
func TestManagedEgressPolicyLeavesLegacyEntryAlone(t *testing.T) {
	legacy := resources.EnvironmentManagedService{
		Kind:         "azure-postgres-flexible",
		ExternalName: "p.postgres.database.azure.com",
		EgressCIDRs:  []string{"10.20.11.0/28"},
	}
	policy, err := managedEgressPolicy("store", "payments", &legacy)
	if err != nil {
		t.Fatal(err)
	}
	if policy != nil {
		t.Errorf("legacy entry rendered a policy: %+v", policy)
	}
}

// TestRetainManagedBundleKeepsPasswordlessUnitForItsEgressPolicy is the
// regression for the interaction between core's passwordless behavior and this
// bundle: such a service projects no ExternalSecret, so without the policy the
// managed unit's whole tree is deleted and the endpoint is never opened.
func TestRetainManagedBundleKeepsPasswordlessUnitForItsEgressPolicy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "rendered.yaml"),
		[]byte("apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	managed := managedIdentityService()
	if len(managed.SecretReferences) != 0 {
		t.Fatal("fixture is not passwordless")
	}
	retained, err := retainManagedBundle(root, "store", "production", "payments", &managed)
	if err != nil {
		t.Fatal(err)
	}
	if !retained {
		t.Fatal("passwordless managed unit was dropped despite its declared egress")
	}
	policy := manifestOfKind(t, buildOverlay(t, root, "production"), kindNetworkPolicy)
	encoded, err := yaml.Marshal(policy.value)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cidr: 10.20.11.0/28", "port: 5432", "Egress"} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("rendered policy missing %q: %s", want, encoded)
		}
	}
}

func TestProjectManagedTransportRendersProxyAndDialsLoopback(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")

	if err := projectManagedTransport(
		context.Background(), root, "payments", storeConsumer("accounts"), consumerEnvironment(managedIdentityService()),
	); err != nil {
		t.Fatal(err)
	}

	rendered := buildOverlay(t, root, "production")

	configMap := manifestOfKind(t, rendered, kindConfigMap)
	data, _ := configMap.value["data"].(map[string]any)
	if got := data["CODEFLY__ENDPOINT__PAYMENTS__STORE__TCP__TCP"]; got != "127.0.0.1:5432" {
		t.Errorf("dial address = %v, want the proxy's loopback port", got)
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
	if len(containers) != 2 {
		t.Fatalf("pod carries %d containers, want the workload and its proxy", len(containers))
	}
	sidecar, _ := containers[1].(map[string]any)
	if sidecar["name"] != "store-proxy" || sidecar["image"] != proxyImage {
		t.Errorf("sidecar = %+v", sidecar)
	}
	args := sliceField(sidecar, "args")
	if len(args) != 3 || args[1] != "--auto-iam-authn" {
		t.Errorf("sidecar args = %v, want the declared ones verbatim", args)
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

// TestProjectManagedTransportDirectModeDialsTheEndpoint pins that DialHost, not
// a second derivation, is what decides the address: in direct mode the
// application keeps dialing the endpoint itself and no proxy is rendered.
func TestProjectManagedTransportDirectModeDialsTheEndpoint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")

	managed := managedIdentityService()
	managed.Transport = &resources.EnvironmentManagedTransport{Mode: resources.TransportModeDirect}

	if err := projectManagedTransport(
		context.Background(), root, "payments", storeConsumer("accounts"), consumerEnvironment(managed),
	); err != nil {
		t.Fatal(err)
	}

	rendered := buildOverlay(t, root, "production")
	data, _ := manifestOfKind(t, rendered, kindConfigMap).value["data"].(map[string]any)
	if got := data["CODEFLY__ENDPOINT__PAYMENTS__STORE__TCP__TCP"]; got != "10.20.11.7:5432" {
		t.Errorf("dial address = %v, want the declared endpoint", got)
	}
	spec, _ := podSpec(manifestOfKind(t, rendered, kindDeployment))
	if containers := sliceField(spec, "containers"); len(containers) != 1 {
		t.Errorf("direct transport rendered %d containers, want the workload alone", len(containers))
	}
}

func TestProjectManagedTransportRefusesUnpinnedProxyImage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")

	managed := managedIdentityService()
	managed.Transport.Image = "us-central1-docker.pkg.dev/obinh/images/db-proxy:1.33.9"

	err := projectManagedTransport(
		context.Background(), root, "payments", storeConsumer("accounts"), consumerEnvironment(managed),
	)
	if err == nil || !strings.Contains(err.Error(), "sha256 digest") {
		t.Fatalf("err = %v, want a refusal naming the unpinned image", err)
	}
}

func TestProjectManagedTransportRefusesUnimplementedMode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")

	managed := managedIdentityService()
	managed.Transport.Mode = "service-mesh"

	err := projectManagedTransport(
		context.Background(), root, "payments", storeConsumer("accounts"), consumerEnvironment(managed),
	)
	if err == nil || !strings.Contains(err.Error(), "service-mesh") {
		t.Fatalf("err = %v, want a refusal naming the mode", err)
	}
}

// TestProjectManagedTransportRefusesConflictingIdentities covers the one case a
// pod cannot express: it runs under a single ServiceAccount, so two managed
// endpoints naming different principals would silently leave one unauthenticated.
func TestProjectManagedTransportRefusesConflictingIdentities(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")

	warehouse := managedIdentityService()
	warehouse.Identity = &resources.EnvironmentWorkloadIdentity{Principal: "warehouse@obinh.iam.gserviceaccount.com"}
	env := consumerEnvironment(managedIdentityService())
	env.ManagedServices["warehouse"] = warehouse

	service := storeConsumer("accounts")
	service.ServiceDependencies = append(service.ServiceDependencies, &resources.ServiceDependency{Name: "warehouse"})

	err := projectManagedTransport(context.Background(), root, "payments", service, env)
	if err == nil || !strings.Contains(err.Error(), "authenticates as one") {
		t.Fatalf("err = %v, want a refusal naming the conflict", err)
	}

	// Two endpoints reached as the same principal are one identity, and render
	// as one rather than being refused.
	sameIdentity := managedIdentityService()
	sameIdentity.Transport = nil
	env.ManagedServices["warehouse"] = sameIdentity
	if err = projectManagedTransport(context.Background(), root, "payments", service, env); err != nil {
		t.Fatalf("two endpoints with one principal were refused: %v", err)
	}
}

// TestProjectManagedTransportLeavesNonConsumersAlone pins that the projection
// follows the declared dependency graph: a service that never dials the managed
// endpoint gets neither the proxy nor the endpoint's identity.
func TestProjectManagedTransportLeavesNonConsumersAlone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "frontend")
	writeConsumerTree(t, root, "production", "payments", "frontend", "store.payments.svc:5432")
	before := readTree(t, root)

	if err := projectManagedTransport(
		context.Background(), root, "payments", &resources.Service{Name: "frontend"}, consumerEnvironment(managedIdentityService()),
	); err != nil {
		t.Fatal(err)
	}
	if after := readTree(t, root); after != before {
		t.Errorf("tree of a non-consuming service changed:\n%s", after)
	}
}

// TestProjectManagedTransportLeavesLegacyBindingAlone pins that a managed
// service declaring no transport carries no new fact, so the tree the agent
// rendered is not rewritten — the password-auth path keeps working untouched.
func TestProjectManagedTransportLeavesLegacyBindingAlone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")
	before := readTree(t, root)

	legacy := resources.EnvironmentManagedService{
		Kind:         "azure-postgres-flexible",
		ExternalName: "p.postgres.database.azure.com",
		EgressCIDRs:  []string{"10.20.11.0/28"},
	}
	if err := projectManagedTransport(
		context.Background(), root, "payments", storeConsumer("accounts"), consumerEnvironment(legacy),
	); err != nil {
		t.Fatal(err)
	}
	if after := readTree(t, root); after != before {
		t.Errorf("tree changed for a managed service that declares no transport:\n%s", after)
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

// TestProjectManagedTransportIgnoresBuildOnlyEdge pins that the projection
// follows what a pod dials, not what a toolchain reads: a build-stage edge on a
// managed service is consumed by codegen, so stamping its proxy and identity
// onto the workload would authenticate a container that never connects.
func TestProjectManagedTransportIgnoresBuildOnlyEdge(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")
	before := readTree(t, root)

	service := &resources.Service{
		Name:                "accounts",
		ServiceDependencies: []*resources.ServiceDependency{{Name: "store", Kind: resources.DependencyKindBuild}},
	}
	if err := projectManagedTransport(
		context.Background(), root, "payments", service, consumerEnvironment(managedIdentityService()),
	); err != nil {
		t.Fatal(err)
	}
	if after := readTree(t, root); after != before {
		t.Errorf("tree changed for a build-only edge:\n%s", after)
	}
}

// TestProjectManagedTransportRefusesProxyWithNoAddressToRepoint covers the
// failure a rendered sidecar cannot show: with nothing in the configuration
// naming the endpoint, the proxy would run beside an application still dialing
// the endpoint itself, and the deploy would reconcile clean.
func TestProjectManagedTransportRefusesProxyWithNoAddressToRepoint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")
	base := filepath.Join(root, "base")
	configMap, err := os.ReadFile(filepath.Join(base, "config-map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	stripped := strings.ReplaceAll(string(configMap), "CODEFLY__ENDPOINT__PAYMENTS__STORE__TCP__TCP", "CODEFLY__UNRELATED")
	if err = os.WriteFile(filepath.Join(base, "config-map.yaml"), []byte(stripped), 0o644); err != nil {
		t.Fatal(err)
	}

	err = projectManagedTransport(
		context.Background(), root, "payments", storeConsumer("accounts"), consumerEnvironment(managedIdentityService()),
	)
	if err == nil || !strings.Contains(err.Error(), "no endpoint address") {
		t.Fatalf("err = %v, want a refusal naming the missing address", err)
	}
}

// TestProjectManagedTransportKeepsManifestShape pins that the projection edits
// manifests in place rather than re-serializing them: a rendered tree's review
// diff has to show the transport and nothing else, so key order and the comments
// an agent wrote have to survive.
func TestProjectManagedTransportKeepsManifestShape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeConsumerTree(t, root, "production", "payments", "accounts", "store.payments.svc:5432")
	path := filepath.Join(root, "base", "config-map.yaml")
	annotated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append([]byte("# rendered by the accounts agent\n"), annotated...), 0o644); err != nil {
		t.Fatal(err)
	}

	if err = projectManagedTransport(
		context.Background(), root, "payments", storeConsumer("accounts"), consumerEnvironment(managedIdentityService()),
	); err != nil {
		t.Fatal(err)
	}

	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(rewritten)), "\n")
	if lines[0] != "# rendered by the accounts agent" || lines[1] != "apiVersion: v1" {
		t.Errorf("manifest was re-serialized rather than edited in place:\n%s", rewritten)
	}
	if !strings.Contains(string(rewritten), "127.0.0.1:5432") {
		t.Errorf("dial address was not rewritten:\n%s", rewritten)
	}
}
