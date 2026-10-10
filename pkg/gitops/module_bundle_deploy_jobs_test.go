package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type deployJobFixture struct {
	root, moduleRoot, destination string
	job                           moduleBundleDeployJob
	units                         []InventoryUnit
	services                      []*resources.Service
}

func newDeployJobFixture(t *testing.T, explicit string) deployJobFixture {
	t.Helper()
	f := deployJobFixture{root: t.TempDir(), moduleRoot: t.TempDir()}
	f.destination = filepath.Join(f.root, "module")
	env := &environments.Environment{Name: "production", Namespace: "accounts"}
	if explicit != "" {
		env.ServiceConfig = &environments.EnvironmentServiceConfig{Services: map[string]environments.EnvironmentServiceConfigMapping{"accounts": {Values: map[string]string{"AUDIT_SINK": explicit}}}}
	}
	accounts := &resources.Service{Name: "accounts", Spec: map[string]any{"environment-defaults": map[string]any{"AUDIT_SINK": "postgres"}}, ServiceDependencies: []*resources.ServiceDependency{{Name: "store", Endpoints: []*resources.EndpointReference{{Name: "tcp"}}}}}
	f.services = []*resources.Service{{Name: "store"}, accounts}
	for _, service := range f.services {
		unitRoot := filepath.Join(f.root, "services", service.Name)
		writeConsumerTree(t, unitRoot, env.Name, env.Namespace, service.Name, "declared.example")
		f.units = append(f.units, InventoryUnit{Kind: UnitKindService, Module: "accounts", Name: service.Name, Path: "services/" + service.Name, Output: restrictedRenderEvidence()})
	}
	accountsRoot := filepath.Join(f.root, "services", "accounts")
	updateDeployTestYAML(t, filepath.Join(accountsRoot, "base", "deployment.yaml"), func(doc map[string]any) {
		spec := mapField(doc, "spec")
		spec["selector"] = map[string]any{"matchLabels": map[string]any{"app": "accounts"}}
		pod := mapField(spec, "template")
		mapField(mapField(pod, "metadata"), "labels")["azure.workload.identity/use"] = "true"
		podSpec := mapField(pod, "spec")
		podSpec["serviceAccountName"] = "accounts"
		podSpec["volumes"] = []any{map[string]any{"name": "tokens", "emptyDir": map[string]any{"medium": "Memory"}}}
		podSpec["initContainers"] = []any{map[string]any{"name": "token-refresh", "image": "registry.example.com/tokens@sha256:" + strings.Repeat("b", 64), "restartPolicy": "Always"}}
		container := sliceField(podSpec, "containers")[0].(map[string]any)
		container["command"] = []any{"/app/app"}
		container["readinessProbe"] = map[string]any{"tcpSocket": map[string]any{"port": 8080}}
		container["env"] = []any{map[string]any{"name": "DB_CONNECTION", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "secret-accounts", "key": "control-plane-connection"}}}}
		container["volumeMounts"] = []any{map[string]any{"name": "tokens", "mountPath": "/tokens", "readOnly": true}}
		podSpec["containers"] = append(sliceField(podSpec, "containers"), map[string]any{"name": "db-proxy", "image": "registry.example.com/proxy@sha256:" + strings.Repeat("c", 64)})
	})
	require.NoError(t, projectServiceConfiguration(t.Context(), accountsRoot, accounts, env, scopeOf(env), serviceInjection{}))
	// The store's migration runs against the store this same Application
	// deploys, so it belongs AFTER the store's own workload. That is what the
	// barrier annotation declares; it cannot be read off the manifest, and a
	// PreSync hook would say the opposite — before the whole Sync phase.
	bootstrap := map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": "store-migrate", "namespace": "accounts", "labels": map[string]any{"codefly.dev/bootstrap-service": "store"}, "annotations": map[string]any{deployBarrierAnnotation: deployBarrierAfter}}, "spec": map[string]any{"ttlSecondsAfterFinished": 30, "template": map[string]any{"spec": map[string]any{"restartPolicy": "Never", "containers": []any{map[string]any{"name": "migrate", "image": "registry.example.com/store@sha256:" + strings.Repeat("d", 64)}}}}}}
	storeBase := filepath.Join(f.root, "services", "store", "base")
	require.NoError(t, writeArgoYAML(filepath.Join(storeBase, "migration.yaml"), bootstrap))
	updateDeployTestYAML(t, filepath.Join(storeBase, kustomizationFile), func(doc map[string]any) { doc[resourcesKey] = append(sliceField(doc, resourcesKey), "migration.yaml") })
	overlay := filepath.Join(f.destination, "overlays", env.Name)
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	foundation := []map[string]any{
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "accounts", "namespace": "accounts"}},
		{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "accounts-http", "namespace": "accounts"}, "spec": map[string]any{"selector": map[string]any{"app": "accounts"}, "ports": []any{map[string]any{"port": 8080}}}},
		{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"name": "accounts-egress", "namespace": "accounts"}, "spec": map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app": "accounts"}}, "policyTypes": []any{"Egress"}}},
	}
	require.NoError(t, writeDeploymentDocuments(filepath.Join(overlay, "foundation.yaml"), foundation))
	require.NoError(t, writeArgoYAML(filepath.Join(overlay, kustomizationFile), map[string]any{"apiVersion": kustomizeAPIVersion, "kind": kindKustomization, "resources": []any{"foundation.yaml"}}))
	require.NoError(t, os.MkdirAll(filepath.Join(f.moduleRoot, "deployment", "generated"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.moduleRoot, "deployment", "generated", "roles.json"), []byte(`{"roles":[]}`), 0o600))
	f.job = moduleBundleDeployJob{Name: "role-catalog-import", Service: "accounts", Command: "role-catalog-import", Catalog: "deployment/generated/roles.json", Force: true, Writes: moduleBundleDeployTarget{Service: "store", Endpoint: "tcp", Port: 5432}, After: []string{"store"}, ServiceEnvironment: []string{"AUDIT_SINK"}}
	return f
}

func updateDeployTestYAML(t *testing.T, path string, update func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, yaml.Unmarshal(data, &value))
	update(value)
	require.NoError(t, writeArgoYAML(path, value))
}

func (f deployJobFixture) render() error {
	return renderModuleDeployJobs(f.moduleRoot, f.destination, "production", "accounts", "accounts", []moduleBundleDeployJob{f.job}, f.units, f.services)
}

func deployObject(t *testing.T, documents []manifest, kind, name string) manifest {
	t.Helper()
	for _, document := range documents {
		if document.kind == kind && metadataString(document.value, "name") == name {
			return document
		}
	}
	t.Fatalf("missing %s %s", kind, name)
	return manifest{}
}

func TestModuleDeployJobsInheritResolvedRuntimeAndGateConsumers(t *testing.T) {
	for _, explicit := range []string{"", "bigquery"} {
		t.Run("mode="+explicit, func(t *testing.T) {
			f := newDeployJobFixture(t, explicit)
			require.NoError(t, f.render())
			documents := buildOverlay(t, f.destination, "production")
			importJob := deployObject(t, documents, kindJob, "role-catalog-import")
			pod := podTemplate(importJob)
			spec, _ := podSpec(importJob)
			require.Equal(t, "accounts", spec["serviceAccountName"])
			require.Equal(t, "Never", spec["restartPolicy"])
			require.Len(t, sliceField(spec, "containers"), 1)
			require.Len(t, sliceField(spec, "initContainers"), 2)
			for _, sidecar := range sliceField(spec, "initContainers") {
				require.Equal(t, "Always", sidecar.(map[string]any)["restartPolicy"])
			}
			labels := mapField(mapField(pod, "metadata"), "labels")
			require.Equal(t, "accounts", labels["app"])
			require.Equal(t, "deploy-job", labels["codefly.dev/workload-role"])
			require.Equal(t, "true", labels["azure.workload.identity/use"])
			require.Equal(t, "true", mapField(mapField(pod, "metadata"), "annotations")["sidecar.istio.io/nativeSidecar"])
			container := sliceField(spec, "containers")[0].(map[string]any)
			require.Equal(t, "registry.example.com/accounts@sha256:"+strings.Repeat("a", 64), container["image"])
			require.Equal(t, []any{"/app/app"}, container["command"])
			require.Equal(t, []any{"role-catalog-import", "-catalog", "/codefly/deploy-catalog/catalog.json", "-force"}, container["args"])
			require.NotContains(t, container, "readinessProbe")
			require.Len(t, sliceField(container, "volumeMounts"), 2)
			want := explicit
			if want == "" {
				want = "postgres"
			}
			var mode, connection map[string]any
			for _, raw := range sliceField(container, "env") {
				entry := raw.(map[string]any)
				if entry["name"] == "AUDIT_SINK" {
					mode = entry
				}
				if entry["name"] == "DB_CONNECTION" {
					connection = entry
				}
			}
			require.Equal(t, want, mode["value"])
			require.Equal(t, map[string]any{"secretKeyRef": map[string]any{"name": "secret-accounts", "key": "control-plane-connection"}}, connection["valueFrom"])
			workload, _ := podSpec(deployObject(t, documents, kindDeployment, "accounts"))
			for _, raw := range sliceField(sliceField(workload, "containers")[0].(map[string]any), "env") {
				entry := raw.(map[string]any)
				if entry["name"] == "AUDIT_SINK" {
					require.Equal(t, want, entry["value"])
				}
			}
			wave := func(kind, name string) int {
				value, err := strconv.Atoi(mapField(mapField(deployObject(t, documents, kind, name).value, "metadata"), "annotations")["argocd.argoproj.io/sync-wave"].(string))
				require.NoError(t, err)
				return value
			}
			require.Less(t, wave(kindDeployment, "store"), wave(kindJob, "store-migrate"))
			require.Less(t, wave(kindJob, "store-migrate"), wave(kindJob, "role-catalog-import"))
			require.Less(t, wave(kindJob, "role-catalog-import"), wave(kindDeployment, "accounts"))
			// The import Job is the CLI's own, so it is the one the aggregate
			// makes a Sync hook. The unit's Job keeps its own spec: Argo makes
			// a plain Job in a wave a barrier without a hook conversion, and
			// converting one would re-run it on every sync.
			importer := deployObject(t, documents, kindJob, "role-catalog-import")
			importAnnotations := mapField(mapField(importer.value, "metadata"), "annotations")
			require.Equal(t, "Sync", importAnnotations["argocd.argoproj.io/hook"])
			require.Equal(t, "BeforeHookCreation,HookSucceeded", importAnnotations["argocd.argoproj.io/hook-delete-policy"])
			require.NotContains(t, mapField(importer.value, "spec"), "ttlSecondsAfterFinished")
			migration := deployObject(t, documents, kindJob, "store-migrate")
			migrationAnnotations := mapField(mapField(migration.value, "metadata"), "annotations")
			require.NotContains(t, migrationAnnotations, "argocd.argoproj.io/hook")
			require.NotContains(t, migrationAnnotations, "argocd.argoproj.io/hook-delete-policy")
			require.Equal(t, 30, mapField(migration.value, "spec")["ttlSecondsAfterFinished"])
			selector := mapField(mapField(deployObject(t, documents, "Service", "accounts-http").value, "spec"), "selector")
			require.Equal(t, "service", selector["codefly.dev/workload-role"])
			require.Equal(t, "accounts", selector["app"])
			policy := mapField(mapField(mapField(deployObject(t, documents, "NetworkPolicy", "accounts-egress").value, "spec"), "podSelector"), "matchLabels")
			require.Equal(t, "accounts", policy["app"])
			require.NotContains(t, policy, "codefly.dev/workload-role")
		})
	}
}

func TestModuleDeployJobsUseOneArgoApplication(t *testing.T) {
	f := newDeployJobFixture(t, "bigquery")
	require.NoError(t, f.render())
	inventory := &Inventory{SchemaVersion: SchemaVersion, Module: "accounts", Environment: "production", Namespace: "accounts", AppProject: "accounts-production", ModulePath: "module", ModuleIncludesUnits: true, Units: f.units}
	require.NoError(t, generateArgoBootstrap(context.Background(), &repositoryConfig{RepoURL: "https://github.com/example/manifests.git"}, f.root, "environments/accounts", inventory, "production", strings.Repeat("a", 40), ""))
	require.NoError(t, validateBootstrapUnits(filepath.Join(f.root, "bootstrap"), "environments/accounts", inventory, "production"))
	var sources []string
	require.NoError(t, walkBootstrapApplications(filepath.Join(f.root, "bootstrap"), func(_, _, path string) error { sources = append(sources, path); return nil }))
	require.Equal(t, []string{"environments/accounts/module/overlays/production"}, sources)
}

func TestModuleDeployJobsRefuseMissingAuthorityAndBarriers(t *testing.T) {
	for _, change := range []func(*deployJobFixture){
		func(f *deployJobFixture) { f.job.After = nil },
		func(f *deployJobFixture) { f.job.Writes.Endpoint = "undeclared" },
		func(f *deployJobFixture) { f.job.Catalog = "../outside.json" },
		func(f *deployJobFixture) { f.job.ServiceEnvironment = []string{"MISSING_AUDIT_SINK"} },
		func(f *deployJobFixture) { f.units[0].Managed = true; f.units[0].Path = "" },
	} {
		f := newDeployJobFixture(t, "")
		change(&f)
		require.Error(t, f.render())
	}
}

func TestModuleDeployJobsManagedMigrationBarrier(t *testing.T) {
	f := newDeployJobFixture(t, "bigquery")
	f.units[0].Managed = true
	updateDeployTestYAML(t, filepath.Join(f.root, "services", "store", "base", kustomizationFile), func(doc map[string]any) { doc[resourcesKey] = []any{"config-map.yaml", "migration.yaml"} })
	require.NoError(t, f.render())
	documents := buildOverlay(t, f.destination, "production")
	deployObject(t, documents, kindJob, "store-migrate")
	deployObject(t, documents, kindJob, "role-catalog-import")
}

func TestModuleDeployJobsDevImageRemainsTheServiceImage(t *testing.T) {
	f := newDeployJobFixture(t, "bigquery")
	require.NoError(t, f.render())
	options := &RenderOptions{Module: "accounts", Environment: "production", Namespace: "accounts", AppProject: "accounts-production", ModulePath: "module", ModuleIncludesUnits: true, Units: f.units}
	inventory, err := buildInventory(f.root, options)
	require.NoError(t, err)
	newImage := "registry.example.com/accounts@sha256:" + strings.Repeat("e", 64)
	var accounts *InventoryUnit
	for i := range inventory.Units {
		if inventory.Units[i].Name == "accounts" {
			accounts = &inventory.Units[i]
		}
	}
	require.NotNil(t, accounts)
	changed, err := applyDevImages(f.root, &inventory, accounts, []string{newImage}, &InventoryDevDeployment{Service: "accounts", Commit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	require.Contains(t, changed, "module/overlays/production/deploy-jobs.yaml")
	documents := buildOverlay(t, f.destination, "production")
	for _, kind := range []string{kindJob, kindDeployment} {
		name := "accounts"
		if kind == kindJob {
			name = "role-catalog-import"
		}
		spec, _ := podSpec(deployObject(t, documents, kind, name))
		require.Equal(t, newImage, sliceField(spec, "containers")[0].(map[string]any)["image"])
	}
	refreshed, err := LoadInventory(f.root)
	require.NoError(t, err)
	require.True(t, refreshed.ModuleIncludesUnits)
}

func TestModuleDeployJobsRequireInventoryBoundary(t *testing.T) {
	f := newDeployJobFixture(t, "")
	require.NoError(t, f.render())
	_, err := validateTree(f.root, &RenderOptions{Module: "accounts", Environment: "production", ModulePath: "module"})
	require.ErrorContains(t, err, "reconciliation boundary disagree")
}

func TestModuleDeployJobsGuardPublishedOwnershipBeforeStaging(t *testing.T) {
	for _, aggregate := range []bool{false, true} {
		t.Run(strconv.FormatBool(aggregate), func(t *testing.T) {
			repo := t.TempDir()
			gitRun(t, repo, "init", "--initial-branch=main")
			gitRun(t, repo, "config", "user.name", "Codefly Test")
			gitRun(t, repo, "config", "user.email", "codefly@example.com")
			gitRun(t, repo, "config", "commit.gpgsign", "false")
			const targetPath = "deployments/modules/accounts"
			target := filepath.Join(repo, targetPath)
			previous := Inventory{SchemaVersion: SchemaVersion, Module: "accounts", ModuleIncludesUnits: aggregate}
			require.NoError(t, writeCanonicalInventory(filepath.Join(target, InventoryFilename), &previous))
			gitRun(t, repo, "add", "-A")
			gitRun(t, repo, "commit", "-m", "published ownership")
			revision := gitOutput(t, repo, "rev-parse", "HEAD")
			gitRun(t, repo, "update-ref", "refs/remotes/origin/"+serviceSnapshotBranch("accounts", "production"), revision)
			next := previous
			require.NoError(t, guardModuleAggregationTransition(context.Background(), repo, targetPath, revision, &next))
			require.NoError(t, guardModuleAggregationTransition(context.Background(), repo, "new-module", "", &next))
			next.ModuleIncludesUnits = !aggregate
			next.Units = []InventoryUnit{{Name: "accounts", Kind: UnitKindService, Path: "services/accounts"}}
			_, err := prepareServiceSnapshot(context.Background(), repo, target, targetPath, t.TempDir(), &next, "production", true, nil)
			require.ErrorContains(t, err, "governed non-cascading ownership transfer")
			require.Empty(t, gitOutput(t, repo, "status", "--porcelain"), "ownership refusal must precede source tree staging")
			// A legacy published inventory without a snapshot ref is equally guarded.
			require.ErrorContains(t, guardModuleAggregationTransition(context.Background(), repo, targetPath, "", &next), "ownership transfer")
		})
	}
}
