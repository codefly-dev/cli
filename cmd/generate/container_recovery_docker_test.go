//go:build integration

package generate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/dockerrun"
)

// qualificationImage stands in for the proto companion image. What is under
// qualification is the ownership a container carries and the sweep that
// collects it, neither of which depends on what runs inside it — and pulling
// the companion image here would make a recovery proof fail on a buf or
// registry problem. Keep the tag in step with the image the workflow pulls.
const qualificationImage = "alpine:3.22"

// interruptedGenerateEnv marks the re-exec'd child below as the interrupted
// generate. The owner recorded on a container is the PID that created it, and
// the sweep keeps anything whose owner is still alive, so the leftover has to
// be created by a process that really dies — not by this test pretending one
// did.
const interruptedGenerateEnv = "CODEFLY_TEST_INTERRUPTED_GENERATE"

// generateContainerNameEnv carries the container name to the child, so the
// parent can find what it left behind.
const generateContainerNameEnv = "CODEFLY_TEST_INTERRUPTED_GENERATE_CONTAINER"

// interruptedGenerateMarker is how the parent knows the child really ran. A
// `-test.run` regex that matches nothing exits 0, so without this a renamed or
// build-tag-excluded helper would be read as a successful build.
const interruptedGenerateMarker = "container built; exiting without shutdown"

// TestInterruptedGenerateHelper is the child process: a `generate` that builds
// its container and is killed before the defer that shuts it down. It creates
// the container the way the real call sites do — the marker projected by
// generate's own projectContainerRecovery, the environment ephemeral and
// paused — and then exits without Shutdown.
//
// It is a no-op in an ordinary run, so this file adds no container to a normal
// `go test -tags=integration ./cmd/generate`.
func TestInterruptedGenerateHelper(t *testing.T) {
	if os.Getenv(interruptedGenerateEnv) == "" {
		return
	}
	ctx := context.Background()
	projectContainerRecovery(ctx)
	// The real call sites hand the environment an absolute directory (the proto
	// directory, or the temporary copy of it); Docker refuses a relative one.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	runner, err := runners.NewDockerEnvironment(ctx, resources.NewDockerImage(qualificationImage), dir, os.Getenv(generateContainerNameEnv))
	if err != nil {
		t.Fatalf("create docker environment: %v", err)
	}
	runner.WithEphemeral()
	runner.WithPause()
	if err = runner.Init(ctx); err != nil {
		t.Fatalf("init docker environment: %v", err)
	}
	fmt.Fprintln(os.Stdout, interruptedGenerateMarker)
}

// TestAnInterruptedGenerateLeavesARecoverableContainer qualifies `codefly
// generate` against a real Docker daemon: that the containers it builds in the
// CLI process carry `codefly.recovery-scope`, and that a later run in the same
// workspace collects one an interrupted generate left behind.
//
// The second half is the one that needed a daemon. `generate` resolves the
// identity a run resolves, but a run is free to pick a different naming scope
// (--naming-scope, a non-local --env, the invocation id --temporary-ports
// generates), and the exact-scope sweep compares a hash that includes it — so
// that sweep is asserted here to walk past the leftover. What collects it is
// the disposable sweep, keyed on the naming-scope-independent namespace, which
// only reaches it because the call sites mark these containers ephemeral. Both
// sweeps run, in the order `initRunService` runs them.
func TestAnInterruptedGenerateLeavesARecoverableContainer(t *testing.T) {
	requireDockerDaemon(t)
	root := newRecoveryWorkspace(t, "from-yaml")
	home := os.Getenv(resources.CodeflyHomeEnv)

	// The identity a `codefly run` in this workspace resolves, read through the
	// marker because the scope's fields are Core's own.
	if _, err := containerRecoveryScope(context.Background()); err != nil {
		t.Fatalf("containerRecoveryScope: %v", err)
	}
	projectContainerRecovery(context.Background())
	generatedScope, generatedNamespace := markerFields(t)
	if generatedScope == "" {
		t.Fatal("the workspace resolved no container recovery ownership")
	}
	// A durable namespace is a precondition of this qualification, not an
	// optional extra. The disposable sweep is keyed on it and returns without
	// doing anything when it is empty, so skipping here would leave the gate
	// green having checked the labels and neither sweep — the silent pass this
	// proof exists to close. Fail instead, and say what the host is missing.
	if generatedNamespace == "" {
		t.Fatal("this host proves no durable identity (no usable /etc/machine-id, /var/lib/dbus/machine-id or product UUID), so ReapDisposableContainers cannot be qualified here; run this gate on a host that has a machine identity")
	}

	name := fmt.Sprintf("generate-recovery-%d", time.Now().UnixMilli())
	container := runners.ContainerName(name)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "--force", container).Run()
	})

	child := exec.Command(os.Args[0], "-test.run=^TestInterruptedGenerateHelper$", "-test.v")
	child.Dir = root
	child.Env = append(os.Environ(),
		interruptedGenerateEnv+"=1",
		generateContainerNameEnv+"="+name,
		resources.CodeflyHomeEnv+"="+home,
		runners.ContainerRecoveryScopeEnvironment+"=",
	)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("the interrupted generate did not build its container: %v: %s", err, output)
	}
	// Exit 0 is not evidence the child ran: `go test` exits 0 when its -test.run
	// matches nothing, which is what a renamed or build-tag-excluded helper
	// produces. Require the helper's own marker instead.
	if !strings.Contains(string(output), interruptedGenerateMarker) {
		t.Fatalf("the interrupted generate never ran (no %q in its output); the helper is missing or its name no longer matches\n%s",
			interruptedGenerateMarker, output)
	}
	if !containerExists(t, container) {
		t.Fatal("the interrupted generate left no container behind; there is nothing to recover")
	}

	// Ownership. A container labeled with anything else is collected by no
	// sweep this workspace ever runs, which is the failure this qualification
	// exists to catch — and it is silent.
	if got := containerLabel(t, container, runners.LabelCodeflyRecoveryScope); got != generatedScope {
		t.Errorf("container %s = %q, want the scope a run resolves %q", runners.LabelCodeflyRecoveryScope, got, generatedScope)
	}
	if got := containerLabel(t, container, runners.LabelCodeflyRecoveryNamespace); got != generatedNamespace {
		t.Errorf("container %s = %q, want the durable namespace %q", runners.LabelCodeflyRecoveryNamespace, got, generatedNamespace)
	}
	if got := containerLabel(t, container, runners.LabelCodeflyEphemeral); got != "true" {
		t.Errorf("container %s = %q, want \"true\": without it the disposable sweep never reaches this container", runners.LabelCodeflyEphemeral, got)
	}

	// A later run in the same workspace, having chosen a different naming
	// scope.
	renamed, err := runners.NewContainerRecoveryScope(home, root, "pr-123")
	if err != nil {
		t.Fatalf("NewContainerRecoveryScope: %v", err)
	}
	ctx := context.Background()
	if err = runners.ReapStaleContainers(ctx, renamed); err != nil {
		t.Fatalf("ReapStaleContainers: %v", err)
	}
	if !containerExists(t, container) {
		t.Fatal("the exact-scope sweep collected a leftover from another naming scope; the disposable sweep below is then proving nothing")
	}
	if err = runners.ReapDisposableContainers(ctx, renamed); err != nil {
		t.Fatalf("ReapDisposableContainers: %v", err)
	}
	if containerExists(t, container) {
		t.Fatal("a run that renamed the naming scope left the interrupted generate's container behind")
	}
}

func requireDockerDaemon(t *testing.T) {
	t.Helper()
	if output, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Fatalf("this qualification needs a reachable docker daemon: %v: %s", err, output)
	}
}

func containerExists(t *testing.T, container string) bool {
	t.Helper()
	return exec.Command("docker", "inspect", container).Run() == nil
}

func containerLabel(t *testing.T, container, label string) string {
	t.Helper()
	output, err := exec.Command("docker", "inspect", "--format", fmt.Sprintf("{{index .Config.Labels %q}}", label), container).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect %s: %v: %s", container, err, output)
	}
	return strings.TrimSpace(string(output))
}
