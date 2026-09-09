package deployments

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// completionQualification exercises the completion contract against a real
// disposable k3d cluster: only a live API server settles what "applied" versus
// "bootstrapped" versus "healthy" actually mean.
type completionQualification struct {
	t          *testing.T
	env        *resources.Environment
	target     VerifiedKubernetesTarget
	namespace  string
	kubeconfig string
}

func requireDisposableK3d(t *testing.T) completionQualification {
	t.Helper()
	if os.Getenv("CODEFLY_DEPLOYMENTS_K3D_QUALIFY") != "1" {
		t.Skip("set CODEFLY_DEPLOYMENTS_K3D_QUALIFY=1 to run the disposable k3d completion qualification")
	}
	for _, binary := range []string{"docker", "k3d", "kubectl"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatalf("%s is required: %v", binary, err)
		}
	}
	// k3d caps a cluster name at 32 characters.
	cluster := fmt.Sprintf("codefly-done-%x", time.Now().UnixNano())
	runQualificationCommand(t, "k3d", "cluster", "create", cluster,
		"--servers", "1", "--agents", "0", "--wait", "--timeout", "3m",
		"--kubeconfig-update-default=false", "--kubeconfig-switch-context=false")
	t.Cleanup(func() {
		_ = exec.Command("k3d", "cluster", "delete", cluster).Run()
	})

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	require.NoError(t, os.WriteFile(kubeconfig,
		[]byte(runQualificationCommand(t, "k3d", "kubeconfig", "get", cluster)), 0o600))

	namespace := "completion"
	runQualificationCommand(t, "kubectl", "--kubeconfig", kubeconfig, "create", "namespace", namespace)

	env := &resources.Environment{
		Name:      "local",
		Namespace: namespace,
		Cluster: &resources.EnvironmentCluster{
			Kind:       "k3d",
			Kubeconfig: kubeconfig,
			Context:    "k3d-" + cluster,
		},
	}
	target, err := VerifyLocalK3dTarget(context.Background(), env)
	require.NoError(t, err)
	return completionQualification{t: t, env: env, target: target, namespace: namespace, kubeconfig: kubeconfig}
}

func (q completionQualification) apply(manifests string) {
	q.t.Helper()
	command := exec.Command("kubectl", "--kubeconfig", q.kubeconfig, "--namespace", q.namespace, "apply", "-f", "-")
	command.Stdin = strings.NewReader(manifests)
	output, err := command.CombinedOutput()
	require.NoError(q.t, err, string(output))
}

func (q completionQualification) await(stage CompletionStage, timeout time.Duration, manifests string) ([]ObservedResource, error) {
	q.t.Helper()
	owned, err := ownedResources(manifests, q.env)
	require.NoError(q.t, err)
	return completionObserver{env: q.env, target: &q.target}.await(context.Background(), owned, stage, timeout)
}

func runQualificationCommand(t *testing.T, name string, args ...string) string {
	t.Helper()
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func unpullableDeployment(namespace string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: unpullable
  namespace: ` + namespace + `
spec:
  replicas: 1
  selector:
    matchLabels:
      app: unpullable
  template:
    metadata:
      labels:
        app: unpullable
    spec:
      containers:
        - name: app
          image: registry.invalid/codefly/does-not-exist:missing
`
}

func migrationJob(namespace, name, script string) string {
	return `apiVersion: batch/v1
kind: Job
metadata:
  name: ` + name + `
  namespace: ` + namespace + `
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: busybox:1.36
          command: ["sh", "-c", "` + script + `"]
`
}

// A kubectl apply that succeeds establishes applied and nothing more: an image
// the cluster cannot pull must still fail a caller that requires health.
func TestDisposableK3dAppliedDoesNotImplyHealthy(t *testing.T) {
	qualification := requireDisposableK3d(t)
	manifests := unpullableDeployment(qualification.namespace)

	qualification.apply(manifests)

	observed, err := qualification.await(StageHealthy, 90*time.Second, manifests)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Deployment "+qualification.namespace+"/unpullable")
	require.Len(t, observed, 1)
	require.Equal(t, ResourceFailed, observed[0].State)
	require.Contains(t, observed[0].Message, "ImagePull")
}

// A failed bootstrap Job cannot be reported as a completed bootstrap, and its
// diagnostic names the Job rather than the deployment as a whole.
func TestDisposableK3dFailedBootstrapJobBlocksBootstrappedCompletion(t *testing.T) {
	qualification := requireDisposableK3d(t)
	manifests := migrationJob(qualification.namespace, "schema-broken", "exit 3")

	qualification.apply(manifests)

	observed, err := qualification.await(StageBootstrapped, 120*time.Second, manifests)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Job "+qualification.namespace+"/schema-broken")
	require.Len(t, observed, 1)
	require.Equal(t, ResourceFailed, observed[0].State)
}

// A schema Job that succeeds is what lets a consumer roll out, and observation
// reports bootstrapped only once the Job is genuinely complete.
func TestDisposableK3dSchemaJobPrecedesConsumerRollout(t *testing.T) {
	qualification := requireDisposableK3d(t)
	schema := migrationJob(qualification.namespace, "schema-ready", "sleep 5")
	consumer := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: consumer
  namespace: ` + qualification.namespace + `
spec:
  replicas: 1
  selector:
    matchLabels:
      app: consumer
  template:
    metadata:
      labels:
        app: consumer
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "sleep 3600"]
`
	preparation, rollout, err := partitionDocuments([]string{consumer, schema})
	require.NoError(t, err)
	require.Len(t, preparation, 1)
	require.Len(t, rollout, 1)

	qualification.apply(strings.Join(preparation, "\n---\n"))
	observed, err := qualification.await(StageBootstrapped, 180*time.Second, schema)
	require.NoError(t, err)
	require.Equal(t, ResourceReady, observed[0].State)

	qualification.apply(strings.Join(rollout, "\n---\n"))
	observed, err = qualification.await(StageHealthy, 180*time.Second, schema+"---\n"+consumer)
	require.NoError(t, err)
	for _, resource := range observed {
		require.Equal(t, ResourceReady, resource.State, resource.String())
	}
}

// A Job that never finishes returns a bounded, actionable failure rather than
// hanging on the caller's budget.
func TestDisposableK3dJobTimeoutIsBoundedAndActionable(t *testing.T) {
	qualification := requireDisposableK3d(t)
	manifests := migrationJob(qualification.namespace, "schema-slow", "sleep 600")

	qualification.apply(manifests)

	start := time.Now()
	_, err := qualification.await(StageBootstrapped, 20*time.Second, manifests)
	require.Error(t, err)
	require.Contains(t, err.Error(), "did not reach bootstrapped within 20s")
	require.Contains(t, err.Error(), "Job "+qualification.namespace+"/schema-slow")
	require.Less(t, time.Since(start), 90*time.Second)
}
