package deployments

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// schemaThenConsumer renders a consumer workload ahead of the schema Job it
// depends on, so a test can prove ordering comes from the bootstrap marker
// rather than from the position of a document in the rendered stream.
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
  labels:
    codefly.dev/bootstrap-service: store
spec:
  completions: 1
`

// verificationJob is an ordinary Job: it exercises the workload it is rendered
// after, so hoisting it ahead of that workload breaks it.
const verificationJob = `apiVersion: batch/v1
kind: Job
metadata:
  name: smoke-test
  namespace: backend
spec:
  completions: 1
`

// generousBudget is an upper bound the settled fixtures below never reach:
// their observations return on the first read. Sizing such a budget near one
// sweep makes the assertion depend on how fast the machine runs subprocesses,
// which is what broke these tests under -race. Only a test asserting the bound
// itself sits near it.
const generousBudget = time.Minute

const completeJob = `{"spec":{"completions":1},"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}}`

const readyDeployment = `{"metadata":{"generation":1,"uid":"deploy-uid"},"spec":{"replicas":1},` +
	`"status":{"observedGeneration":1,"replicas":1,"readyReplicas":1,"updatedReplicas":1}}`

const pendingDeployment = `{"metadata":{"generation":1,"uid":"deploy-uid"},"spec":{"replicas":1},` +
	`"status":{"observedGeneration":1,"replicas":1,"readyReplicas":0,"updatedReplicas":1}}`

// ownedReplicaSet is the ReplicaSet a Deployment owns, the link that makes a pod
// this Deployment's pod.
const ownedReplicaSet = `{"items":[{"metadata":{"uid":"rs-uid","ownerReferences":[{"uid":"deploy-uid"}]}}]}`

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

// Mutated must not be claimed for a phase that had nothing to apply: it is the
// signal an operator uses to decide whether a namespace needs reconciling.
func TestNothingToApplyIsNotReportedAsAChangedTarget(t *testing.T) {
	plan, err := planApply([]string{"", "   \n"}, "backend", true)

	require.NoError(t, err)
	require.Empty(t, plan.Preparation)
	require.Empty(t, plan.Rollout)
}

// A caller that asked only for applied must get its manifests in the order
// kustomize rendered them: the barrier moves resources, and moving them is a
// behavior change nobody requested.
func TestDefaultCompletionAppliesEveryDocumentInRenderedOrder(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(), DefaultDeployCompletion(),
	)
	require.NoError(t, err)

	require.NoError(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))

	applied := harness.applied()
	require.Less(t, strings.Index(applied, "kind: Deployment"), strings.Index(applied, "kind: Job"))
	require.Empty(t, harness.read())
	require.Equal(t, StageApplied, manager.Evidence().Reached)
}

// Only a Job marked as schema preparation forms the barrier. An ordinary Job
// hoisted ahead of the workload it exercises would fail, and a barrier waiting
// on it would deadlock against a dependency that is still unapplied.
func TestOrdinaryJobStaysWithTheRolloutEvenUnderABarrier(t *testing.T) {
	plan, err := planApply(
		[]string{"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n", verificationJob},
		"backend",
		true,
	)

	require.NoError(t, err)
	require.Empty(t, plan.BootstrapJobs)
	require.Empty(t, plan.Preparation)
	require.Len(t, plan.Rollout, 2)
	require.Equal(t, []ownedResource{
		{kind: "Deployment", namespace: "backend", name: "api"},
		{kind: "Job", namespace: "backend", name: "smoke-test"},
	}, plan.RolloutTargets)
}

func TestLocalApplyManagerAppliesSchemaPreparationBeforeConsumerRollout(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", completeJob)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageBootstrapped, Timeout: generousBudget},
	)
	require.NoError(t, err)

	require.NoError(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))

	applied := harness.applied()
	require.Less(t, strings.Index(applied, "kind: Job"), strings.Index(applied, "kind: Deployment"))
	evidence := manager.Evidence()
	require.Equal(t, StageBootstrapped, evidence.Reached)
	require.Equal(t, StageBootstrapped, evidence.Required)
	require.True(t, evidence.RenderedTrees[0].Mutated)
	require.False(t, evidence.RenderedTrees[0].AppliedAt.IsZero())
}

func TestLocalApplyManagerRefusesToApplyConsumersWhenTheSchemaJobFails(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", `{"spec":{"completions":1},"status":{"failed":3,"conditions":[`+
		`{"type":"Failed","status":"True","reason":"BackoffLimitExceeded","message":"Job has reached the specified backoff limit"}]}}`)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageBootstrapped, Timeout: generousBudget},
	)
	require.NoError(t, err)

	err = manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput())

	require.ErrorContains(t, err, "BackoffLimitExceeded")
	require.ErrorContains(t, err, "Job backend/schema-migrate")
	require.NotContains(t, harness.applied(), "kind: Deployment")
	evidence := manager.Evidence()
	require.False(t, evidence.Reached.AtLeast(StageBootstrapped))
	require.Contains(t, evidence.RenderedTrees[0].Diagnostics[0], "BackoffLimitExceeded")
}

// A barrier that fails has already applied the preparation resources, so the
// evidence must say the target was changed. Reporting only StageRendered would
// tell an operator no cluster was contacted while new config is live there.
func TestFailedBarrierRecordsThatTheTargetWasChanged(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", `{"spec":{"completions":1},"status":{"conditions":[`+
		`{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}}`)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageBootstrapped, Timeout: generousBudget},
	)
	require.NoError(t, err)

	require.Error(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))

	tree := manager.Evidence().RenderedTrees[0]
	require.Equal(t, StageRendered, tree.Stage)
	require.True(t, tree.Mutated)
	require.Contains(t, strings.Join(tree.Diagnostics, " "), "partial revision")
}

func TestLocalApplyManagerReportsAppliedWhenAnImageCannotBePulled(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", completeJob)
	harness.clusterHas("deployment", "api", pendingDeployment)
	harness.clusterHas("replicasets", "all", ownedReplicaSet)
	harness.clusterHas("pods", "all", `{"items":[{"metadata":{"name":"api-7f9","ownerReferences":[{"uid":"rs-uid"}]},`+
		`"status":{"containerStatuses":[{"name":"api","state":{"waiting":{"reason":"ImagePullBackOff",`+
		`"message":"Back-off pulling image"}}}]}}]}`)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageHealthy, Timeout: generousBudget},
	)
	require.NoError(t, err)

	err = manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput())

	require.ErrorContains(t, err, "ImagePullBackOff")
	require.ErrorContains(t, err, "Deployment backend/api")
	require.Contains(t, harness.applied(), "kind: Deployment")
	evidence := manager.Evidence()
	require.True(t, evidence.Reached.AtLeast(StageApplied))
	require.False(t, evidence.Reached.AtLeast(StageHealthy))
}

// A pod that merely shares the workload's labels is not the workload's pod.
// kustomize commonLabels put a selector's labels on resources it does not own,
// so diagnosing by label would fail a healthy Deployment on a sibling's pod.
func TestPendingWorkloadIsNotDiagnosedWithAPodItDoesNotOwn(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.clusterHas("deployment", "api", pendingDeployment)
	harness.clusterHas("replicasets", "all", ownedReplicaSet)
	harness.clusterHas("pods", "all", `{"items":[{"metadata":{"name":"other-job-xyz",`+
		`"ownerReferences":[{"uid":"someone-else"}]},"status":{"containerStatuses":[{"name":"migrate",`+
		`"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image"}}}]}}]}`)
	env := harness.verifiedEnvironment()
	target, err := VerifyLocalK3dTarget(context.Background(), env)
	require.NoError(t, err)
	kubeconfig, err := verifiedKubeconfigSnapshot(context.Background(), env, &target)
	require.NoError(t, err)

	observed, readable := completionObserver{env: env, target: &target}.observe(
		context.Background(),
		kubeconfig,
		ownedResource{kind: kindDeployment, namespace: "backend", name: "api"},
	)

	require.True(t, readable)
	require.Equal(t, ResourcePending, observed.State)
	require.Contains(t, observed.Message, "0/1 replicas ready")
	require.NotContains(t, observed.Message, "ImagePullBackOff")
	require.NotContains(t, observed.Message, "other-job-xyz")
}

// ProgressDeadlineExceeded is not terminal: the controller keeps reconciling and
// the condition clears when the slow pull finishes. Treating it as a hard
// failure fails a rollout that would have completed.
func TestProgressDeadlineExceededExplainsAPendingRolloutInsteadOfFailingIt(t *testing.T) {
	document := &workloadDocument{}
	document.Metadata.Generation = 1
	document.Status.ObservedGeneration = 1
	document.Status.Replicas = 1
	document.Status.UpdatedReplicas = 1
	document.Status.Conditions = []statusCondition{{
		Type: "Progressing", Status: "False",
		Reason: "ProgressDeadlineExceeded", Message: `ReplicaSet "api-7f9" has timed out progressing.`,
	}}

	state, message := deploymentState(document)

	require.Equal(t, ResourcePending, state)
	require.Contains(t, message, "ProgressDeadlineExceeded")
	require.Contains(t, message, "0/1 replicas ready")
}

func TestLocalApplyManagerReachesHealthyWhenEveryOwnedRolloutIsReady(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", completeJob)
	harness.clusterHas("deployment", "api", readyDeployment)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageHealthy, Timeout: generousBudget},
	)
	require.NoError(t, err)

	require.NoError(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))

	require.Equal(t, StageHealthy, manager.Evidence().Reached)
}

// A schema Job with ttlSecondsAfterFinished is deleted shortly after it
// completes. Its stage was established at the barrier, so the health observation
// must not read it again and call the deployment missing.
func TestCompletedBootstrapJobIsNotReadAgainWhileTheRolloutSettles(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	// The Job answers the barrier once, then its TTL removes it — exactly the
	// window between the barrier completing and the rollout settling.
	harness.clusterHasOnce("job", "schema-migrate", completeJob)
	harness.clusterHas("deployment", "api", readyDeployment)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageHealthy, Timeout: generousBudget},
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
		CompletionCondition{Stage: StageHealthy, Timeout: generousBudget},
	)
	require.NoError(t, err)

	require.NoError(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))

	read := harness.read()
	require.Contains(t, read, "job schema-migrate")
	require.Contains(t, read, "deployment api")
	require.NotContains(t, read, "service")
	require.NotContains(t, read, "pods")
}

// A manifest that declares no namespace lands in the namespace the verified
// context selects, because apply passes no --namespace. Observing it anywhere
// else finds nothing and burns the whole budget on a deployment that worked.
func TestOwnedResourcesFallBackToTheNamespaceApplyUses(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.writeSelected(kubeconfigDocumentInNamespace("k3d-dev", "k3d-dev", "k3d-dev", "payments"))
	harness.writeOwned(kubeconfigDocument("k3d-dev", "k3d-dev", "k3d-dev", "https://127.0.0.1:6443"))

	target, err := VerifyLocalK3dTarget(context.Background(), harness.environment("k3d-dev"))
	require.NoError(t, err)
	require.Equal(t, "payments", target.Namespace)

	owned, err := ownedResources("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: migrate\n", target.Namespace)
	require.NoError(t, err)
	require.Equal(t, []ownedResource{{kind: "Job", namespace: "payments", name: "migrate"}}, owned)
}

// The budget is the caller's total, not a fresh allowance per tree: a module
// deploy must not be able to spend a stated 10m once per service per stage.
func TestObservationBudgetIsSharedAcrossEveryTree(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", `{"spec":{"completions":1},"status":{"succeeded":0}}`)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageBootstrapped, Timeout: 400 * time.Millisecond},
	)
	require.NoError(t, err)

	require.Error(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))
	second := manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput())

	require.ErrorContains(t, second, "observation budget of 400ms is exhausted")
}

func TestLocalApplyManagerBoundsObservationAndNamesWhatNeverFinished(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.renders(schemaThenConsumer)
	harness.clusterHas("job", "schema-migrate", `{"spec":{"completions":1},"status":{"succeeded":0,"failed":0}}`)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(
		context.Background(), workspace, harness.verifiedEnvironment(),
		CompletionCondition{Stage: StageBootstrapped, Timeout: 8 * time.Second},
	)
	require.NoError(t, err)

	err = manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput())

	require.ErrorContains(t, err, "did not reach bootstrapped within 8s")
	require.ErrorContains(t, err, "Job backend/schema-migrate: 0/1 completions")
}

// When the budget lands in the middle of a sweep, the readings that sweep took
// were cut short by our own deadline. Reporting them would replace the state the
// cluster actually gave us with "context deadline exceeded", which tells an
// operator nothing about why the deployment stalled.
func TestBudgetExpiringMidSweepReportsTheLastCompleteReading(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.clusterHasThenStalls("job", "schema-migrate", `{"spec":{"completions":1},"status":{"succeeded":0,"failed":0}}`)
	env := harness.verifiedEnvironment()
	target, err := VerifyLocalK3dTarget(context.Background(), env)
	require.NoError(t, err)

	_, err = completionObserver{env: env, target: &target}.await(
		context.Background(),
		[]ownedResource{{kind: "Job", namespace: "backend", name: "schema-migrate"}},
		StageBootstrapped,
		8*time.Second,
	)

	require.ErrorContains(t, err, "did not reach bootstrapped within 8s")
	require.ErrorContains(t, err, "Job backend/schema-migrate: 0/1 completions")
	require.NotContains(t, err.Error(), "context deadline exceeded")
	require.NotContains(t, err.Error(), "no longer reports it")
}

// An owned resource is applied before observation starts, so one that never
// reports back is gone, not slow. Waiting out the budget would hide it.
func TestObservationFailsAResourceTheTargetStopsReporting(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	env := harness.verifiedEnvironment()
	target, err := VerifyLocalK3dTarget(context.Background(), env)
	require.NoError(t, err)

	start := time.Now()
	_, err = completionObserver{env: env, target: &target}.await(
		context.Background(),
		[]ownedResource{{kind: "Job", namespace: "backend", name: "schema-migrate"}},
		StageBootstrapped,
		generousBudget,
	)

	require.ErrorContains(t, err, "applied but the target no longer reports it")
	require.Less(t, time.Since(start), 30*time.Second)
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
		4*time.Second,
	)

	require.ErrorContains(t, err, "did not reach bootstrapped within 4s")
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

func TestOwnedResourcesIgnoresManifestsThatCarryNoRolloutOrJob(t *testing.T) {
	owned, err := ownedResources(schemaThenConsumer+`---
apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
  namespace: backend
`, "fallback")

	require.NoError(t, err)
	require.Equal(t, []ownedResource{
		{kind: "Deployment", namespace: "backend", name: "api"},
		{kind: "Job", namespace: "backend", name: "schema-migrate"},
	}, owned)
}
