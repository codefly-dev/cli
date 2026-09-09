package deployments

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// schemaThenConsumer renders a consumer workload ahead of the schema Job it
// depends on, so a test can prove ordering comes from the kinds rather than
// from the position of a document in the rendered stream.
const schemaThenConsumer = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: backend
spec:
  replicas: 1
  selector:
    matchLabels:
      app: api
---
apiVersion: batch/v1
kind: Job
metadata:
  name: schema-migrate
  namespace: backend
spec:
  completions: 1
`

const completeJob = `{"spec":{"completions":1},"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}}`

const readyDeployment = `{"metadata":{"generation":1},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"api"}}},` +
	`"status":{"observedGeneration":1,"replicas":1,"readyReplicas":1,"updatedReplicas":1}}`

func TestCompletionStagesAreCumulative(t *testing.T) {
	require.True(t, StageHealthy.AtLeast(StageApplied))
	require.True(t, StageApplied.AtLeast(StageApplied))
	require.False(t, StageApplied.AtLeast(StageBootstrapped))
	require.False(t, CompletionStage("").AtLeast(StageRendered))

	stage, err := ParseCompletionStage(" Healthy ")
	require.NoError(t, err)
	require.Equal(t, StageHealthy, stage)

	_, err = ParseCompletionStage("running")
	require.ErrorContains(t, err, "unknown completion stage")
}

func TestDeployCompletionDefaultsToAppliedRatherThanHealthy(t *testing.T) {
	require.Equal(t, StageApplied, DefaultDeployCompletion().Stage)
}

func TestNewLocalApplyManagerRejectsACompletionItCannotEstablish(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	workspace, _, _ := deploymentFixture(t)

	_, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageRendered},
	)

	require.ErrorContains(t, err, "direct apply cannot complete at")
	require.NoFileExists(t, harness.applyLog)
}

func TestLocalApplyManagerAppliesSchemaPreparationBeforeConsumerRollout(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", completeJob)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageBootstrapped, Timeout: 2 * time.Second},
	)
	require.NoError(t, err)

	require.NoError(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))

	applied := harness.applied()
	require.Less(t, strings.Index(applied, "kind: Job"), strings.Index(applied, "kind: Deployment"))
	evidence := manager.Evidence()
	require.Equal(t, StageBootstrapped, evidence.Reached)
	require.Equal(t, StageBootstrapped, evidence.Required)
	require.False(t, evidence.RenderedTrees[0].AppliedAt.IsZero())
	require.False(t, evidence.RenderedTrees[0].ObservedAt.IsZero())
}

func TestLocalApplyManagerRefusesToApplyConsumersWhenTheSchemaJobFails(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", `{"spec":{"completions":1},"status":{"failed":3,"conditions":[`+
		`{"type":"Failed","status":"True","reason":"BackoffLimitExceeded","message":"Job has reached the specified backoff limit"}]}}`)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageBootstrapped, Timeout: 2 * time.Second},
	)
	require.NoError(t, err)

	err = manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput())

	require.ErrorContains(t, err, "BackoffLimitExceeded")
	require.ErrorContains(t, err, "Job backend/schema-migrate")
	require.NotContains(t, harness.applied(), "kind: Deployment")
	evidence := manager.Evidence()
	require.False(t, evidence.Reached.AtLeast(StageBootstrapped))
	require.Len(t, evidence.RenderedTrees[0].Diagnostics, 1)
	require.Contains(t, evidence.RenderedTrees[0].Diagnostics[0], "BackoffLimitExceeded")
}

func TestLocalApplyManagerReportsAppliedWhenAnImageCannotBePulled(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", completeJob)
	harness.clusterHas("deployment", "api", `{"metadata":{"generation":1},"spec":{"replicas":1,`+
		`"selector":{"matchLabels":{"app":"api"}}},"status":{"observedGeneration":1,"replicas":1,"readyReplicas":0,"updatedReplicas":1}}`)
	harness.clusterHas("pods", "all", `{"items":[{"metadata":{"name":"api-7f9"},"status":{"containerStatuses":[`+
		`{"name":"api","state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image"}}}]}}]}`)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageHealthy, Timeout: 2 * time.Second},
	)
	require.NoError(t, err)

	err = manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput())

	require.ErrorContains(t, err, "ImagePullBackOff")
	require.ErrorContains(t, err, "Deployment backend/api")
	require.Contains(t, harness.applied(), "kind: Deployment")
	evidence := manager.Evidence()
	require.True(t, evidence.Reached.AtLeast(StageApplied))
	require.False(t, evidence.Reached.AtLeast(StageHealthy))
	require.Equal(t, StageHealthy, evidence.Required)
}

func TestLocalApplyManagerReachesHealthyWhenEveryOwnedRolloutIsReady(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", completeJob)
	harness.clusterHas("deployment", "api", readyDeployment)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageHealthy, Timeout: 2 * time.Second},
	)
	require.NoError(t, err)

	require.NoError(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))

	require.Equal(t, StageHealthy, manager.Evidence().Reached)
}

func TestLocalApplyManagerObservesOnlyTheResourcesTheTreeOwns(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer + `---
apiVersion: v1
kind: Service
metadata:
  name: api
  namespace: backend
`)
	harness.clusterHas("job", "schema-migrate", completeJob)
	harness.clusterHas("deployment", "api", readyDeployment)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageHealthy, Timeout: 2 * time.Second},
	)
	require.NoError(t, err)

	require.NoError(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))

	read := harness.read()
	require.Contains(t, read, "job schema-migrate")
	require.Contains(t, read, "deployment api")
	require.NotContains(t, read, "service")
	require.NotContains(t, read, "pods")
}

func TestLocalApplyManagerBoundsObservationAndNamesWhatNeverFinished(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", `{"spec":{"completions":1},"status":{"succeeded":0,"failed":0}}`)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageBootstrapped, Timeout: 300 * time.Millisecond},
	)
	require.NoError(t, err)

	err = manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput())

	require.ErrorContains(t, err, "did not reach bootstrapped within 300ms")
	require.ErrorContains(t, err, "Job backend/schema-migrate: 0/1 completions")
}

// The budget bounds observation itself: a resource that never finishes returns
// rather than polling until the caller gives up.
func TestObservationStopsAtTheCallerBudget(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.clusterHas("job", "schema-migrate", `{"spec":{"completions":1},"status":{"succeeded":0}}`)
	env := harness.verifiedEnvironment()
	target, err := VerifyLocalK3dTarget(context.Background(), env)
	require.NoError(t, err)

	start := time.Now()
	_, err = completionObserver{env: env, target: &target}.await(
		context.Background(),
		[]ownedResource{{kind: "Job", namespace: "backend", name: "schema-migrate"}},
		StageBootstrapped,
		2*time.Second,
	)

	require.ErrorContains(t, err, "did not reach bootstrapped within 2s")
	require.Less(t, time.Since(start), 30*time.Second)
}

func TestObservationReportsCancellationRatherThanTimeout(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.clusterHas("job", "schema-migrate", `{"spec":{"completions":1},"status":{"succeeded":0}}`)
	env := harness.verifiedEnvironment()
	target, err := VerifyLocalK3dTarget(context.Background(), env)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	_, err = completionObserver{env: env, target: &target}.await(
		ctx,
		[]ownedResource{{kind: "Job", namespace: "backend", name: "schema-migrate"}},
		StageBootstrapped,
		time.Minute,
	)

	require.ErrorContains(t, err, "cancelled before reaching bootstrapped")
}

func TestDefaultCompletionAppliesWithoutContactingTheClusterForStatus(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(), DefaultDeployCompletion(),
	)
	require.NoError(t, err)

	require.NoError(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))

	require.Empty(t, harness.read())
	require.Equal(t, StageApplied, manager.Evidence().Reached)
}

func TestOwnedResourcesIgnoresManifestsThatCarryNoRolloutOrJob(t *testing.T) {
	owned, err := ownedResources(schemaThenConsumer+`---
apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
  namespace: backend
`, nil)

	require.NoError(t, err)
	require.Equal(t, []ownedResource{
		{kind: "Deployment", namespace: "backend", name: "api"},
		{kind: "Job", namespace: "backend", name: "schema-migrate"},
	}, owned)
}
