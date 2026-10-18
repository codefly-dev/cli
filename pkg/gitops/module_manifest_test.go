package gitops

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/solutionhost"
	"gopkg.in/yaml.v3"
)

func testHost() *environments.EnvironmentHost {
	return &environments.EnvironmentHost{
		Coordinate:  "example/prod/region-a",
		Component:   "platform-host",
		Domain:      "example",
		Audience:    "accounts",
		TrustDomain: "cluster.example",
		Delivery:    "platform/accounts/rest",
	}
}

// testEndpointAddress is the in-cluster address the composition resolved for
// the api's routed endpoint.
const testEndpointAddress = "http://api.crm.svc.cluster.local:9090"

// solutionRenderOptions is a module render that delivers one solution
// instance's workloads, with the environment naming a host: one identity
// workload (api), one routed endpoint, and the request of a module that
// publishes no contract.
func solutionRenderOptions(destination string) *RenderOptions {
	return &RenderOptions{
		Destination: destination,
		Module:      "crm",
		Environment: "prod",
		Namespace:   "crm",
		Promotable:  true,
		Workspace:   "example",
		Host:        testHost(),
		Units:       promotableServiceGraph("crm", []string{"api"}),
		Instance: &ModuleInstance{
			Kind:       solutionhost.KindSolution,
			Name:       "crm",
			Package:    "example/crm",
			Version:    "1.4.0",
			Identities: []IdentityWorkload{{Service: "api", Path: "services/api"}},
			Endpoints:  []solutionhost.Endpoint{{Name: "grpc", Service: "api", API: "grpc", Address: testEndpointAddress}},
			Authority:  &solutionhost.Authority{Principal: "crm"},
		},
	}
}

// renderWorkload writes one promotable unit whose body the caller controls, so
// a test can change what the render delivers and watch the manifest follow.
func renderWorkload(body string) func(context.Context, string) error {
	return renderUnits(map[string]string{"api": body})
}

// renderUnits writes one promotable unit per service, each with its body.
func renderUnits(bodies map[string]string) func(context.Context, string) error {
	return func(_ context.Context, root string) error {
		for service, body := range bodies {
			overlay := filepath.Join(root, "services", service, "overlays", "prod")
			if err := os.MkdirAll(overlay, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(body), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
}

// workerDeployment is pinnedDeployment renamed to a second service, running
// as its own account.
var workerDeployment = strings.NewReplacer("name: api", "name: worker", "app: api", "app: worker",
	"serviceAccountName: api", "serviceAccountName: worker", "codefly-dev/api@", "codefly-dev/worker@").Replace(pinnedDeployment)

// deliveredModuleManifest parses the manifest out of the ConfigMap the render
// wrote.
func deliveredModuleManifest(t *testing.T, destination, binding string) *solutionhost.ModuleManifest {
	t.Helper()
	return deliveredModuleManifestIn(t, destination, "prod", binding)
}

func deliveredModuleManifestIn(t *testing.T, destination, environment, binding string) *solutionhost.ModuleManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(moduleManifestOverlay(environment)), binding+".yaml"))
	if err != nil {
		t.Fatalf("read delivered manifest: %v", err)
	}
	var carrier moduleManifestConfigMap
	if err = yaml.Unmarshal(data, &carrier); err != nil {
		t.Fatalf("decode delivered ConfigMap: %v", err)
	}
	if carrier.Kind != kindConfigMap || carrier.APIVersion != "v1" {
		t.Fatalf("delivered carrier is %s/%s, not v1/ConfigMap", carrier.APIVersion, carrier.Kind)
	}
	document, held := carrier.Data[solutionhost.FileName]
	if !held {
		t.Fatalf("delivered ConfigMap carries no %s, only %v", solutionhost.FileName, carrier.Data)
	}
	parsed, err := solutionhost.Parse([]byte(document))
	if err != nil {
		t.Fatalf("delivered manifest does not parse: %v", err)
	}
	return parsed
}

func TestRenderDeclaresOneManifestPerModuleInstance(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	result, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), renderWorkload(pinnedDeployment))
	if err != nil {
		t.Fatal(err)
	}
	declared := result.ModuleManifest
	if declared == nil || declared.Path != "module-manifests/overlays/prod/example.prod.crm.yaml" ||
		declared.Binding != "example.prod.crm" || declared.Generation != 1 {
		t.Fatalf("declared manifest %+v", declared)
	}
	manifest := deliveredModuleManifest(t, destination, "example.prod.crm")
	if manifest.Binding != "example.prod.crm" || manifest.Alias != "crm" || manifest.Kind != solutionhost.KindSolution || manifest.Generation != 1 {
		t.Fatalf("identity %s %s %s %d", manifest.Binding, manifest.Alias, manifest.Kind, manifest.Generation)
	}
	if manifest.Host != (solutionhost.HostTarget{Coordinate: "example/prod/region-a", Domain: "example"}) {
		t.Fatalf("host %+v", manifest.Host)
	}
	if manifest.Release != (solutionhost.Release{Publisher: "example", Name: "crm", Version: "1.4.0"}) {
		t.Fatalf("release %+v", manifest.Release)
	}
	want := solutionhost.Workload{Service: "api", Container: "api"}
	if len(manifest.Workloads) != 1 || !reflect.DeepEqual(manifest.Workloads[0], want) {
		t.Fatalf("workloads %+v, want %+v", manifest.Workloads, want)
	}
	if len(manifest.Endpoints) != 1 || manifest.Endpoints[0] != (solutionhost.Endpoint{Name: "grpc", Service: "api", API: "grpc", Address: testEndpointAddress}) {
		t.Fatalf("endpoints %+v", manifest.Endpoints)
	}
	if manifest.Authority == nil || manifest.Authority.Principal != "crm" || len(manifest.Authority.Bindings) != 0 {
		t.Fatalf("authority %+v: a module with workloads always runs as its principal", manifest.Authority)
	}
	var carried bool
	for _, file := range result.Inventory.Files {
		if file.Path == "module-manifests/overlays/prod/example.prod.crm.yaml" {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the manifest is not in the render inventory: %+v", result.Inventory.Files)
	}
	if err = ValidateRenderedTree(destination, "", true); err != nil {
		t.Fatalf("installed tree with a manifest does not validate: %v", err)
	}
}

// TestTwoIdentityWorkloadsAreOneManifest: a module runs as every service
// declaring module-identity — a serving process and a worker — so its one
// manifest lists both, each with its own account, and both are projected the
// workload credential.
func TestTwoIdentityWorkloadsAreOneManifest(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	options.Units = promotableServiceGraph("crm", []string{"api", "worker"})
	options.Instance.Identities = append(options.Instance.Identities, IdentityWorkload{Service: "worker", Path: "services/worker"})
	if _, err := RenderOwnedTree(context.Background(), options, renderUnits(map[string]string{"api": pinnedDeployment, "worker": workerDeployment})); err != nil {
		t.Fatal(err)
	}
	manifest := deliveredModuleManifest(t, destination, "example.prod.crm")
	want := []solutionhost.Workload{
		{Service: "api", Container: "api"},
		{Service: "worker", Container: "worker"},
	}
	if !reflect.DeepEqual(manifest.Workloads, want) {
		t.Fatalf("workloads %+v, want %+v", manifest.Workloads, want)
	}
	for _, service := range []string{"api", "worker"} {
		volume, mounts := renderedVolumeOf(t, destination, service, identityTokenVolume)
		if volume == nil || len(mounts[service]) != 1 {
			t.Fatalf("%s is not projected the workload credential: %v %v", service, volume, mounts)
		}
	}
}

// TestManifestGenerationAdvancesOnlyWhenItsContentChanges: a re-render of the
// same desired state keeps the generation and the bytes; a change to what the
// manifest says — here the account a workload runs as — is the next one. A
// change the manifest does not carry (a label on the Deployment) is not.
func TestManifestGenerationAdvancesOnlyWhenItsContentChanges(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	path := filepath.Join(destination, filepath.FromSlash(moduleManifestOverlay("prod")), "example.prod.crm.yaml")
	if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	labelled := strings.Replace(pinnedDeployment, "name: api\nspec:", "name: api\n  labels:\n    changed: \"true\"\nspec:", 1)
	if _, err = RenderOwnedTree(context.Background(), options, renderWorkload(labelled)); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(again) {
		t.Fatalf("a change the manifest does not carry rewrote it:\n%s\n---\n%s", first, again)
	}
	// A workload that runs as an account other than its service's name is not
	// a move: it is refused, because the host reads a pod back by that rule.
	moved := strings.Replace(pinnedDeployment, "serviceAccountName: api", "serviceAccountName: api-v2", 1)
	if _, err = RenderOwnedTree(context.Background(), options, renderWorkload(moved)); err == nil || !strings.Contains(err.Error(), "account named after it") {
		t.Fatalf("a workload running as another account must be refused, got %v", err)
	}
	// The authenticating container's name is part of the declaration: renaming
	// it moves the manifest to the next generation.
	renamed := strings.Replace(pinnedDeployment, "- name: api\n", "- name: api-v2\n", 1)
	if _, err = RenderOwnedTree(context.Background(), options, renderWorkload(renamed)); err != nil {
		t.Fatal(err)
	}
	if bumped := deliveredModuleManifest(t, destination, "example.prod.crm"); bumped.Generation != 2 || bumped.Workloads[0].Container != "api-v2" {
		t.Fatalf("a changed manifest is generation %d with %+v, want 2", bumped.Generation, bumped.Workloads)
	}
}

func TestBindingIDIsStablePerInstanceAndDistinctPerSecondInstance(t *testing.T) {
	first, err := bindingID("example", "prod", "crm")
	if err != nil {
		t.Fatal(err)
	}
	again, err := bindingID("example", "prod", "crm")
	if err != nil || first != again {
		t.Fatalf("binding ID is not stable: %q != %q (%v)", first, again, err)
	}
	second, err := bindingID("example", "prod", "crm-eu")
	if err != nil || second == first {
		t.Fatalf("a second instance reuses binding ID %q (%v)", first, err)
	}
	other, err := bindingID("acme", "prod", "crm")
	if err != nil || other == first {
		t.Fatalf("a second workspace reuses binding ID %q (%v)", first, err)
	}
	if _, err = bindingID("example", "prod", strings.Repeat("a", 64)); err == nil {
		t.Fatal("an over-long binding ID was accepted")
	}
	if _, err = bindingID("example", "prod", "crm.eu"); err == nil {
		t.Fatal("an instance name carrying the join separator was accepted")
	}
}

// TestBindingIDRefusesAPartThatCannotNameAKubernetesObject: core admits
// uppercase and underscores in a binding; a ConfigMap name does not.
func TestBindingIDRefusesAPartThatCannotNameAKubernetesObject(t *testing.T) {
	for name, part := range map[string]struct{ workspace, environment, instance string }{
		"uppercase workspace":   {"My_Workspace", "prod", "crm"},
		"underscore":            {"obin_prod", "prod", "crm"},
		"uppercase instance":    {"obin", "prod", "CRM"},
		"leading dash":          {"obin", "prod", "-crm"},
		"uppercase environment": {"obin", "Prod", "crm"},
	} {
		t.Run(name, func(t *testing.T) {
			if id, err := bindingID(part.workspace, part.environment, part.instance); err == nil || !strings.Contains(err.Error(), "Kubernetes object") {
				t.Fatalf("binding ID %q: %v", id, err)
			}
		})
	}
	options := solutionRenderOptions(filepath.Join(t.TempDir(), "modules", "crm"))
	options.Workspace = "My_Workspace"
	if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err == nil || !strings.Contains(err.Error(), "Kubernetes object") {
		t.Fatalf("the render delivered a manifest ArgoCD would refuse: %v", err)
	}
}

func TestRenderRefusesARouteAliasAHostCannotKeyOn(t *testing.T) {
	options := solutionRenderOptions(filepath.Join(t.TempDir(), "modules", "crm"))
	options.Instance.Name = "crm.eu"
	options.Workspace = "example"
	if _, err := ModuleManifestOf("example.prod.crm", testHost(), options.Instance, nil); err == nil || !strings.Contains(err.Error(), "URL path segment") {
		t.Fatalf("a dotted route alias was accepted: %v", err)
	}
}

// TestRenderRefusesAWorkloadThatRunsAsNoAccountOfItsOwn: the account is all a
// mint finds a workload by, so the namespace's default account (every pod's
// that names none) and the delivery Job's account are refused.
func TestRenderRefusesAWorkloadThatRunsAsNoAccountOfItsOwn(t *testing.T) {
	for name, account := range map[string]string{"default": "      serviceAccountName: default\n", "delivery": "      serviceAccountName: delivery\n", "none": ""} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(pinnedDeployment, "      serviceAccountName: api\n", account, 1)
			_, err := RenderOwnedTree(context.Background(), solutionRenderOptions(filepath.Join(t.TempDir(), "modules", "crm")), renderWorkload(body))
			if err == nil || !strings.Contains(err.Error(), "ServiceAccount") {
				t.Fatalf("a workload running as %q was declared: %v", name, err)
			}
		})
	}
}

// TestRenderRefusesACompositionWithNoHost: an environment that composes a
// module declares the host it runs on.
func TestRenderRefusesACompositionWithNoHost(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	options.Host = nil
	_, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err == nil || !strings.Contains(err.Error(), "composes crm but declares no host block") {
		t.Fatalf("a composition with no host must be refused by name, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(destination, InventoryFilename)); !os.IsNotExist(statErr) {
		t.Fatalf("a refused render must leave no tree behind: %v", statErr)
	}
}

// TestRenderRefusesAnUnreadableRenderedManifest: falling back to generation 1
// would deliver a manifest every host that applied the binding refuses.
func TestRenderRefusesAnUnreadableRenderedManifest(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(destination, filepath.FromSlash(moduleManifestOverlay("prod")), "example.prod.crm.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  labels:\n    codefly.dev/document: module-manifest\ndata: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err == nil || !strings.Contains(err.Error(), "holds no "+solutionhost.FileName) {
		t.Fatalf("an unreadable rendered manifest was ignored: %v", err)
	}
}

// TestDeliveredCarrierMatchesTheGolden pins the whole carrier, byte for byte:
// the ConfigMap, the data key the manifest is under and the manifest itself.
func TestDeliveredCarrierMatchesTheGolden(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	if _, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(moduleManifestOverlay("prod")), "example.prod.crm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	goldenPath := filepath.Join("testdata", "module-manifest.golden.yaml")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err = os.WriteFile(goldenPath, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(golden) != string(body) {
		t.Fatalf("the delivered carrier changed:\nwant:\n%s\ngot:\n%s", golden, body)
	}
}

// TestDeclaredManifestReachesArgo: Argo points an Application at each overlay
// the ApplicationSet names, so a manifest written anywhere else is committed
// and applied to nothing; and the derived AppProject must admit its kinds.
func TestDeclaredManifestReachesArgo(t *testing.T) {
	root := t.TempDir()
	targetPath := "environments/deployments/modules/crm"
	inventory := &Inventory{
		SchemaVersion: SchemaVersion, Module: "crm", Environment: "prod", Namespace: "crm", AppProject: "crm",
		Units:              []InventoryUnit{{Kind: UnitKindService, Module: "crm", Name: "api", Path: "services/api"}},
		ModuleManifestPath: moduleManifestDir,
	}
	writeOverlay(t, filepath.Join(root, "services", "api", "overlays", "prod"))
	writeOverlay(t, filepath.Join(root, moduleManifestDir, "overlays", "prod"))
	config := &repositoryConfig{RepoURL: "https://github.com/codefly-dev/manifests.git"}
	if err := generateArgoBootstrap(context.Background(), config, root, targetPath, inventory, "prod", strings.Repeat("c", 40), ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "bootstrap", "applicationset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	overlay := targetPath + "/" + moduleManifestDir + "/overlays/prod"
	if !strings.Contains(string(data), "overlay: "+overlay+"\n") || !strings.Contains(string(data), "wave: \""+deliveryWave+"\"") {
		t.Fatalf("no Argo Application delivers the manifest in the delivery wave:\n%s", data)
	}
	hostRoot := t.TempDir()
	if err := copyTree(root, hostRoot); err != nil {
		t.Fatal(err)
	}
	hostInventory := *inventory
	hostInventory.HostsDelivery = true
	if err := generateArgoBootstrap(context.Background(), config, hostRoot, targetPath, &hostInventory, "prod", strings.Repeat("c", 40), ""); err != nil {
		t.Fatal(err)
	}
	hostSet, err := os.ReadFile(filepath.Join(hostRoot, "bootstrap", "applicationset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hostSet), "wave: \""+hostDeliveryWave+"\"") {
		t.Fatalf("the host module's delivery is not ordered after its units:\n%s", hostSet)
	}
	_, namespaced, err := snapshotAuthority(root, inventory, "prod")
	if err != nil {
		t.Fatal(err)
	}
	var covered bool
	for _, authority := range namespaced {
		covered = covered || authority.Kind == kindConfigMap
	}
	if !covered {
		t.Fatalf("the AppProject does not admit the manifest's ConfigMap: %+v", namespaced)
	}
}

// TestRenderedTreeDeliversItsManifest: what the render wrote is what the
// inventory records and the overlay's kustomization names.
func TestRenderedTreeDeliversItsManifest(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	result, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), renderWorkload(pinnedDeployment))
	if err != nil {
		t.Fatal(err)
	}
	if result.Inventory.ModuleManifestPath != moduleManifestDir {
		t.Fatalf("the inventory records %q", result.Inventory.ModuleManifestPath)
	}
	kustomization, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(moduleManifestOverlay("prod")), "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"example.prod.crm.yaml", "delivery-service-account.yaml"} {
		if !strings.Contains(string(kustomization), file) {
			t.Fatalf("the kustomization does not name %s:\n%s", file, kustomization)
		}
	}
	rebuilt, err := buildInventory(destination, inventoryRenderOptions(&result.Inventory))
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ModuleManifestPath != result.Inventory.ModuleManifestPath {
		t.Fatalf("re-deriving the inventory dropped the delivery path: %q", rebuilt.ModuleManifestPath)
	}
}

// TestManifestNamesOnlyTheWorkloadsThatRunAsTheModule: a bootstrap Job runs
// its own image and never authenticates as the service, so it is no workload
// of the manifest.
func TestManifestNamesOnlyTheWorkloadsThatRunAsTheModule(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	if _, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), renderWorkload(hostedDeployment)); err != nil {
		t.Fatal(err)
	}
	manifest := deliveredModuleManifest(t, destination, "example.prod.crm")
	if len(manifest.Workloads) != 1 || manifest.Workloads[0].Service != "api" || manifest.Workloads[0].Container != "api" {
		t.Fatalf("workloads %+v, want the serving Deployment's api alone", manifest.Workloads)
	}
}

func TestReleaseIdentityRefusesAPackageItCannotName(t *testing.T) {
	for name, instance := range map[string]ModuleInstance{
		"no publisher": {Package: "crm", Version: "1.0.0"},
		"three parts":  {Package: "example/crm/eu", Version: "1.0.0"},
		"empty name":   {Package: "example/", Version: "1.0.0"},
		"no version":   {Package: "example/crm"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := releaseIdentity(&instance); err == nil {
				t.Fatalf("release %+v was named", instance)
			}
		})
	}
	publisher, name, err := releaseIdentity(&ModuleInstance{Package: "example/crm", Version: "1.4.0"})
	if err != nil || publisher != "example" || name != "crm" {
		t.Fatalf("releaseIdentity = %q, %q, %v", publisher, name, err)
	}
}

// TestARenderThatDeclaresNothingClearsTheDeliveryPath: the inventory field
// points publish at the overlay, so a reused RenderOptions that declares
// nothing must not keep a previous render's value.
func TestARenderThatDeclaresNothingClearsTheDeliveryPath(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	options.Instance = nil
	second, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err != nil {
		t.Fatal(err)
	}
	if second.Inventory.ModuleManifestPath != "" {
		t.Fatalf("the inventory still claims %q", second.Inventory.ModuleManifestPath)
	}
	if _, statErr := os.Stat(filepath.Join(destination, moduleManifestDir)); !os.IsNotExist(statErr) {
		t.Fatalf("the manifest directory survived a render that declared none: %v", statErr)
	}
}
