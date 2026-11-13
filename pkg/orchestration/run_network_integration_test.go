//go:build integration

package orchestration

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/stretchr/testify/require"
)

const runNetworkImage = "mirror.gcr.io/library/alpine:3.21"

func createTestNetwork(t *testing.T, owned bool) string {
	t.Helper()
	name := fmt.Sprintf("codefly-run-network-test-%d", time.Now().UnixNano())
	args := []string{"network", "create"}
	if owned {
		args = append(args, "--label", dockerrun.LabelCodeflyOwner+"=true")
	}
	output, err := exec.Command("docker", append(args, name)...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", name).Run() })
	return name
}

func networkExists(name string) bool {
	return exec.Command("docker", "network", "inspect", name).Run() == nil
}

// Against the real engine: a failed run removes the network its removed
// containers left empty, and only one codefly created — never one another
// container still uses, nor one it does not own.
func TestRemoveRunNetworkAgainstTheEngine(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()

	empty := createTestNetwork(t, true)
	removed, err := removeRunNetwork(ctx, empty)
	require.NoError(t, err)
	require.True(t, removed)
	require.False(t, networkExists(empty), "the empty codefly network is removed")

	foreign := createTestNetwork(t, false)
	removed, err = removeRunNetwork(ctx, foreign)
	require.NoError(t, err)
	require.False(t, removed)
	require.True(t, networkExists(foreign), "a network codefly did not create is kept")

	shared := createTestNetwork(t, true)
	output, err := exec.Command("docker", "run", "--detach", "--rm", "--network", shared, runNetworkImage, "sleep", "60").CombinedOutput()
	require.NoError(t, err, "%s", output)
	id := strings.TrimSpace(string(output))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", id).Run() })
	removed, err = removeRunNetwork(ctx, shared)
	require.NoError(t, err)
	require.False(t, removed)
	require.True(t, networkExists(shared), "a network another container still uses is kept")

	// The engine's inspect lists only running endpoints; a stopped container
	// still needs its network to start again.
	stopped := createTestNetwork(t, true)
	output, err = exec.Command("docker", "create", "--network", stopped, runNetworkImage, "true").CombinedOutput()
	require.NoError(t, err, "%s", output)
	created := strings.TrimSpace(string(output))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", created).Run() })
	removed, err = removeRunNetwork(ctx, stopped)
	require.NoError(t, err)
	require.False(t, removed)
	require.True(t, networkExists(stopped), "a network a stopped container is attached to is kept")

	removed, err = removeRunNetwork(ctx, "codefly-run-network-test-absent")
	require.NoError(t, err)
	require.False(t, removed)
}
