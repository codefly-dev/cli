//go:build integration

package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/recoveryscope"
)

// disposableRunImage stands in for a store's image: what is under test is
// what becomes of a run's container when the run ends, not what runs in it.
const disposableRunImage = "mirror.gcr.io/library/alpine:3.22"

// storeContainerOf is the container a store's agent of flow's run leaves when
// the run stops it, made as an agent makes it: by core's Docker environment,
// under the recovery scope the run projects to its agents, with the data
// directory core mounts into it (what the Postgres agent keeps its cluster
// in), and stopped by the agent's own Stop, which keeps it. It answers the
// container's id and its data directory.
func storeContainerOf(t *testing.T, flow *orchestration.Flow) (string, string) {
	t.Helper()
	// The run's projection, as InitManagers makes it before any agent starts.
	_, err := flow.ContainerRecoveryScope()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Unsetenv(recoveryscope.EnvironmentVariable) })

	name := "disposable-run-test-" + flow.Environment().NamingScope + "-" + time.Now().Format("150405.000000000")
	store, err := dockerrun.NewDockerHeadlessEnvironment(t.Context(), resources.NewDockerImage(disposableRunImage), name)
	require.NoError(t, err)
	store.WithPause()
	data, err := store.WithPersistentCacheMount(t.Context(), "data", "/var/lib/store")
	require.NoError(t, err)
	require.NoError(t, store.Init(t.Context()))
	id, err := store.ContainerID()
	require.NoError(t, err)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", "--volumes", id).Run() })
	require.NoError(t, os.WriteFile(filepath.Join(data, "PG_VERSION"), []byte("17\n"), 0o600))
	require.NoError(t, store.Stop(t.Context()))
	return id, data
}

func containerPresent(id string) bool {
	return exec.Command("docker", "inspect", "--type", "container", id).Run() == nil
}

// endedRun is a run of the fixture workspace that ended the way end ends the
// run command's context, taken back exactly as the command takes it back: the
// deferred ReclaimRun, on a context that outlives the cancellation, told
// whether the run failed.
func endedRun(t *testing.T, disposable bool, end func(t *testing.T, ctx context.Context) (context.Context, func())) (string, string) {
	t.Helper()
	if output, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Fatalf("this test needs a reachable docker daemon: %v: %s", err, output)
	}
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(recoveryscope.EnvironmentVariable, "")
	workspace, env := writeHostedNativeWorkspace(t)
	module, err := workspace.LoadModuleFromName(t.Context(), "model")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "model")
	require.NoError(t, err)
	flow, err := orchestration.NewFlow(t.Context(), workspace, module, service, env, orchestration.RunMode)
	require.NoError(t, err)
	if disposable {
		require.NotEmpty(t, flow.WithIsolatedInvocation(), "the run generates its scope, as --temporary-ports does")
	}

	signalled, release := end(t, context.Background())
	defer release()
	started := time.Now().Add(-time.Second)
	ctx, cancelRun := context.WithCancel(signalled)
	defer cancelRun()
	id, data := storeContainerOf(t, flow)

	select {
	case <-signalled.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the run was not asked to stop")
	}
	require.NoError(t, ReclaimRun(context.WithoutCancel(ctx), flow, started, runFailed(signalled, nil)))
	return id, data
}

// The three ways a run is stopped on purpose, each ending the command's
// context as the command wires it.
var runEndings = map[string]func(t *testing.T, ctx context.Context) (context.Context, func()){
	"stop": func(_ *testing.T, ctx context.Context) (context.Context, func()) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		return ctx, cancel
	},
	"SIGTERM": func(t *testing.T, ctx context.Context) (context.Context, func()) {
		ctx, stop := common.SignalContext(ctx)
		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
		return ctx, stop
	},
	"owner gone": func(t *testing.T, ctx context.Context) (context.Context, func()) {
		owner, birth := startOwner(t)
		ctx, release := stopWithOwner(ctx, birth)
		require.NoError(t, owner.Process.Kill())
		_ = owner.Wait()
		return ctx, release
	},
}

// A disposable run's container, and the data directory mounted into it, are
// gone once the run has ended: after a stop, a SIGTERM and the loss of the
// process the run was started for alike. Each used to keep both, as a stop
// keeps a developer's.
func TestADisposableRunsContainerIsGoneHoweverTheRunWasStopped(t *testing.T) {
	for name, end := range runEndings {
		t.Run(name, func(t *testing.T) {
			id, data := endedRun(t, true, end)
			require.False(t, containerPresent(id), "the disposable run's container was kept")
			require.NoDirExists(t, data, "the disposable run's container data was kept")
		})
	}
}

// A persistent run's container and data survive the same three endings: the
// next run reuses the container, and the directory is a developer's database.
func TestAPersistentRunsContainerAndDataSurviveHoweverTheRunWasStopped(t *testing.T) {
	for name, end := range runEndings {
		t.Run(name, func(t *testing.T) {
			id, data := endedRun(t, false, end)
			require.True(t, containerPresent(id), "the persistent run's container was removed")
			require.FileExists(t, filepath.Join(data, "PG_VERSION"), "the persistent run's data was removed")
		})
	}
}
