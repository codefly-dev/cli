package gitops

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// deployAnnotationsOf returns one built object's annotations.
func deployAnnotationsOf(t *testing.T, documents []manifest, kind, name string) map[string]any {
	t.Helper()
	return mapField(mapField(deployObject(t, documents, kind, name).value, "metadata"), "annotations")
}

func deployWaveOf(t *testing.T, documents []manifest, kind, name string) int {
	t.Helper()
	raw, ok := deployAnnotationsOf(t, documents, kind, name)[argoSyncWaveAnnotation].(string)
	require.True(t, ok, "%s %s carries no sync-wave", kind, name)
	wave, err := strconv.Atoi(raw)
	require.NoError(t, err)
	return wave
}

// addUnitDocument writes one extra manifest into a unit's base and lists it.
func addUnitDocument(t *testing.T, root, unit, file string, document map[string]any) {
	t.Helper()
	base := filepath.Join(root, "services", unit, "base")
	require.NoError(t, writeArgoYAML(filepath.Join(base, file), document))
	updateDeployTestYAML(t, filepath.Join(base, kustomizationFile), func(doc map[string]any) {
		doc[resourcesKey] = append(sliceField(doc, resourcesKey), file)
	})
}

func deployTestJob(name string, annotations map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": "accounts", "annotations": annotations},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"restartPolicy": "Never",
			"containers":    []any{map[string]any{"name": "migrate", "image": "registry.example.com/accounts@sha256:" + strings.Repeat("a", 64)}}}}},
	}
}

// A unit's own Job is the unit's. The aggregate never invents a hook phase for
// it, so a PreSync migration keeps meaning "before the whole Sync phase" —
// before its own workload — instead of being rewritten to a Sync hook placed
// after the rollout it was meant to precede.
func TestDeployJobsPreserveAUnitJobDeclaredHookPhase(t *testing.T) {
	f := newDeployJobFixture(t, "")
	addUnitDocument(t, f.root, "accounts", "migrate.yaml",
		deployTestJob("accounts-migrate", map[string]any{argoHookAnnotation: "PreSync"}))
	require.NoError(t, f.render())
	documents := buildOverlay(t, f.destination, "production")
	annotations := deployAnnotationsOf(t, documents, kindJob, "accounts-migrate")
	require.Equal(t, "PreSync", annotations[argoHookAnnotation], "the declared phase must survive")
	require.NotContains(t, annotations, argoHookDeletePolicy, "the aggregate must not make it a deleted-on-success hook")
}

// The placement of a unit's Job relative to its own workload comes from what
// the Job declares, never from the unit's level alone: before-workload for a
// Job preparing the schema its own workload is about to serve, after-workload
// for one migrating a dependency's datastore.
func TestDeployJobsPlaceAUnitJobByItsDeclaredBarrier(t *testing.T) {
	for barrier, ordered := range map[string]func(t *testing.T, own, job int){
		deployBarrierBefore: func(t *testing.T, own, job int) { require.Less(t, job, own) },
		deployBarrierAfter:  func(t *testing.T, own, job int) { require.Greater(t, job, own) },
	} {
		t.Run(barrier, func(t *testing.T) {
			f := newDeployJobFixture(t, "")
			addUnitDocument(t, f.root, "accounts", "migrate.yaml",
				deployTestJob("accounts-migrate", map[string]any{deployBarrierAnnotation: barrier}))
			require.NoError(t, f.render())
			documents := buildOverlay(t, f.destination, "production")
			ordered(t, deployWaveOf(t, documents, kindDeployment, "accounts"),
				deployWaveOf(t, documents, kindJob, "accounts-migrate"))
		})
	}
}

// A declared sync-wave is the author's placement and a Skip hook is a manifest
// shipped for somebody else to run. Replacing either is what made Argo execute,
// and then re-execute on every sync, a Job the renderer said to leave alone.
func TestDeployJobsLeaveAnAuthorPlacedUnitJobAlone(t *testing.T) {
	for name, annotations := range map[string]map[string]any{
		"declared-wave": {argoSyncWaveAnnotation: "50"},
		"skip-hook":     {argoHookAnnotation: argoHookSkip},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDeployJobFixture(t, "")
			addUnitDocument(t, f.root, "accounts", "untouched.yaml", deployTestJob("accounts-untouched", annotations))
			require.NoError(t, f.render())
			documents := buildOverlay(t, f.destination, "production")
			built := deployAnnotationsOf(t, documents, kindJob, "accounts-untouched")
			require.Equal(t, annotations[argoSyncWaveAnnotation], built[argoSyncWaveAnnotation])
			require.Equal(t, annotations[argoHookAnnotation], built[argoHookAnnotation])
			require.NotContains(t, built, argoHookDeletePolicy)
		})
	}
}

func TestDeployJobsRefuseAnUnknownBarrierDeclaration(t *testing.T) {
	f := newDeployJobFixture(t, "")
	addUnitDocument(t, f.root, "accounts", "migrate.yaml",
		deployTestJob("accounts-migrate", map[string]any{deployBarrierAnnotation: "whenever"}))
	require.ErrorContains(t, f.render(), deployBarrierAnnotation)
}

// Every kind podSpec treats as a workload has to receive the discriminator the
// Service selector is narrowed with. A selectable kind left unlabelled gave a
// Service that resolved to no endpoints while every manifest applied cleanly.
func TestDeployJobsLabelEveryWorkloadKindTheSelectorNarrowsAgainst(t *testing.T) {
	f := newDeployJobFixture(t, "")
	addUnitDocument(t, f.root, "accounts", "pod.yaml", map[string]any{
		"apiVersion": "v1", "kind": kindPod,
		"metadata": map[string]any{"name": "accounts-static", "namespace": "accounts", "labels": map[string]any{"app": "accounts"}},
		"spec": map[string]any{"containers": []any{map[string]any{
			"name": "static", "image": "registry.example.com/accounts@sha256:" + strings.Repeat("a", 64)}}},
	})
	addUnitDocument(t, f.root, "accounts", "cron.yaml", map[string]any{
		"apiVersion": "batch/v1", "kind": kindCronJob,
		"metadata": map[string]any{"name": "accounts-sweep", "namespace": "accounts"},
		"spec": map[string]any{"schedule": "0 * * * *", "jobTemplate": map[string]any{"spec": map[string]any{
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "accounts"}},
				"spec": map[string]any{"restartPolicy": "Never", "containers": []any{map[string]any{
					"name": "sweep", "image": "registry.example.com/accounts@sha256:" + strings.Repeat("a", 64)}}}}}}},
	})
	require.NoError(t, f.render())
	documents := buildOverlay(t, f.destination, "production")

	selector := mapField(mapField(deployObject(t, documents, kindService, "accounts-http").value, "spec"), "selector")
	require.Equal(t, UnitKindService, selector[deployWorkloadRoleLabel])

	// A bare Pod IS its own template, so the label lands beside its name and
	// the narrowed Service still selects it.
	pod := deployObject(t, documents, kindPod, "accounts-static")
	podLabels := mapField(mapField(pod.value, "metadata"), "labels")
	require.Equal(t, UnitKindService, podLabels[deployWorkloadRoleLabel])
	require.Equal(t, "accounts", podLabels["app"])
	for key, want := range selector {
		require.Equal(t, want, podLabels[key], "narrowed selector key %q must still match the Pod", key)
	}

	// A CronJob's pods are never an endpoint, so they are labelled as jobs.
	cron := deployObject(t, documents, kindCronJob, "accounts-sweep")
	cronLabels := mapField(mapField(podTemplate(cron), "metadata"), "labels")
	require.Equal(t, deployRoleJob, cronLabels[deployWorkloadRoleLabel])
	require.NotEqual(t, selector[deployWorkloadRoleLabel], cronLabels[deployWorkloadRoleLabel])
}

// A malformed agent render is refused by name. It used to reach
// spec.containers = []any{nil} and panic on a nil map assignment, or fail
// claiming it could not inherit an environment key that was never the cause.
func TestDeployJobsRefuseAContainerWithNoName(t *testing.T) {
	for _, required := range [][]string{nil, {"AUDIT_SINK"}} {
		f := newDeployJobFixture(t, "")
		f.job.ServiceEnvironment = required
		updateDeployTestYAML(t, filepath.Join(f.root, "services", "accounts", "base", "deployment.yaml"), func(doc map[string]any) {
			spec := mapField(mapField(mapField(doc, "spec"), "template"), "spec")
			delete(sliceField(spec, "containers")[0].(map[string]any), "name")
		})
		err := f.render()
		require.ErrorContains(t, err, "container with no name")
		require.ErrorContains(t, err, "accounts")
	}
}

func TestDeployJobsRefuseAnUnpinnedInheritedInitContainer(t *testing.T) {
	f := newDeployJobFixture(t, "")
	updateDeployTestYAML(t, filepath.Join(f.root, "services", "accounts", "base", "deployment.yaml"), func(doc map[string]any) {
		spec := mapField(mapField(mapField(doc, "spec"), "template"), "spec")
		sliceField(spec, "initContainers")[0].(map[string]any)["image"] = "registry.example.com/tokens:latest"
	})
	require.ErrorContains(t, f.render(), "init container \"token-refresh\"")
}

// The catalog is mounted from a ConfigMap, and client-side apply keeps a second
// copy of that object in its last-applied-configuration annotation, so the
// bound has to leave room for the catalog twice.
func TestDeployJobsBoundTheCatalogForADoubledObject(t *testing.T) {
	f := newDeployJobFixture(t, "")
	oversized := `{"roles":["` + strings.Repeat("r", deployCatalogMaxBytes) + `"]}`
	require.NoError(t, os.WriteFile(filepath.Join(f.moduleRoot, "deployment", "generated", "roles.json"), []byte(oversized), 0o600))
	require.ErrorContains(t, f.render(), "last-applied-configuration")
}

func TestDeployJobsHonourADeclaredBudgetWithinBounds(t *testing.T) {
	f := newDeployJobFixture(t, "")
	deadline, backoff := 3600, 1
	f.job.ActiveDeadlineSeconds, f.job.BackoffLimit = &deadline, &backoff
	require.NoError(t, f.render())
	spec := mapField(deployObject(t, buildOverlay(t, f.destination, "production"), kindJob, "role-catalog-import").value, "spec")
	require.Equal(t, 3600, spec["activeDeadlineSeconds"])
	require.Equal(t, 1, spec["backoffLimit"])

	over := deployJobMaxDeadline + 1
	f.job.ActiveDeadlineSeconds = &over
	require.ErrorContains(t, f.render(), "activeDeadlineSeconds")
}

func TestDeployJobsAcceptEveryKustomizationSpelling(t *testing.T) {
	for _, spelling := range kustomizationFileNames {
		t.Run(spelling, func(t *testing.T) {
			f := newDeployJobFixture(t, "")
			overlay := filepath.Join(f.destination, "overlays", "production")
			if spelling != kustomizationFile {
				data, err := os.ReadFile(filepath.Join(overlay, kustomizationFile))
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(overlay, spelling), data, 0o600))
				require.NoError(t, os.Remove(filepath.Join(overlay, kustomizationFile)))
			}
			require.NoError(t, f.render())
			deployObject(t, buildOverlay(t, f.destination, "production"), kindJob, "role-catalog-import")
		})
	}
}

func TestDeployJobsNameTheOverlayWhenItHasNoKustomization(t *testing.T) {
	f := newDeployJobFixture(t, "")
	require.NoError(t, os.Remove(filepath.Join(f.destination, "overlays", "production", kustomizationFile)))
	err := f.render()
	require.ErrorContains(t, err, "declares no kustomization")
	require.ErrorContains(t, err, "production")
}

// A reference the service's own render already bound overrides the default, the
// way the environment's own secret references do. Projecting the default as a
// literal beside it refused the whole render.
func TestServiceEnvironmentDefaultsYieldToARenderedSecretReference(t *testing.T) {
	root := t.TempDir()
	env := &environments.Environment{Name: "production", Namespace: "accounts"}
	service := &resources.Service{Name: "accounts", Spec: map[string]any{
		"environment-defaults": map[string]any{"DB_CONNECTION": "postgres://fallback", "AUDIT_SINK": "postgres"}}}
	writeConsumerTree(t, root, env.Name, env.Namespace, service.Name, "declared.example")
	updateDeployTestYAML(t, filepath.Join(root, "base", "deployment.yaml"), func(doc map[string]any) {
		container := sliceField(mapField(mapField(mapField(doc, "spec"), "template"), "spec"), "containers")[0].(map[string]any)
		container["env"] = []any{map[string]any{"name": "DB_CONNECTION",
			"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "secret-accounts", "key": "conn"}}}}
	})
	require.NoError(t, projectServiceConfiguration(t.Context(), root, service, env, scopeOf(env), serviceInjection{}))

	entries := map[string]map[string]any{}
	workload, _ := podSpec(deployObject(t, buildOverlay(t, root, env.Name), kindDeployment, "accounts"))
	for _, raw := range sliceField(sliceField(workload, "containers")[0].(map[string]any), "env") {
		entry := raw.(map[string]any)
		entries[quantityString(entry["name"])] = entry
	}
	require.NotContains(t, entries["DB_CONNECTION"], "value", "the rendered reference must survive the default")
	require.NotNil(t, entries["DB_CONNECTION"]["valueFrom"])
	require.Equal(t, "postgres", entries["AUDIT_SINK"]["value"], "an unreferenced default still applies")
}

// A Skip hook is never applied, so it cannot stand in for the migration
// barrier a managed handoff's deploy job requires.
func TestDeployJobsRefuseASkippedJobAsAManagedMigrationBarrier(t *testing.T) {
	f := newDeployJobFixture(t, "bigquery")
	f.units[0].Managed = true
	updateDeployTestYAML(t, filepath.Join(f.root, "services", "store", "base", kustomizationFile), func(doc map[string]any) {
		doc[resourcesKey] = []any{"config-map.yaml", "migration.yaml"}
	})
	// Declared as a barrier, this renders; marked Skip, it never runs.
	require.NoError(t, f.render())
	updateDeployTestYAML(t, filepath.Join(f.root, "services", "store", "base", "migration.yaml"), func(doc map[string]any) {
		mapField(mapField(doc, "metadata"), "annotations")[argoHookAnnotation] = argoHookSkip
	})
	require.ErrorContains(t, f.render(), "no rendered readiness/migration barrier")
}
