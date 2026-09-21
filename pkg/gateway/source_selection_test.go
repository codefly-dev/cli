package gateway

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestGatewayDiscoveryHonorsCancellationWithoutHoldingServiceLock(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	root := t.TempDir()
	writeCodeUnitFixture(t, root, "go.mod", "module example.test/source\n")
	binary := filepath.Join(t.TempDir(), "peer")
	output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../sourceworkspace/testdata/agent").CombinedOutput()
	require.NoError(t, err, "%s", output)
	selected := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "slow", Version: "1.0.0"}
	path, err := selected.Path(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.Symlink(binary, path))
	marker := filepath.Join(t.TempDir(), "started")
	t.Setenv("TEST_SOURCE_STARTED", marker)
	t.Setenv("TEST_SOURCE_STARTUP_DELAY", "30s")
	server, err := NewServer(Config{WorkDir: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	response, err := server.Build(canceled, &gatewayv1.BuildRequest{})
	require.NoError(t, err)
	require.False(t, response.Success)
	require.Contains(t, response.Output, "context canceled")
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist, "already canceled calls must not start a peer")
	_, err = server.serviceBehaviorForCodeUnit(canceled, normalizedCodeUnitTarget{id: "root", path: ".", root: root}, "")
	require.ErrorIs(t, err, context.Canceled)

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan *gatewayv1.BuildResponse, 1)
	go func() {
		response, _ := server.Build(ctx, &gatewayv1.BuildRequest{})
		done <- response
	}()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(40 * time.Second):
			t.Error("discovery did not finish after cancellation")
		}
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, 15*time.Second, 10*time.Millisecond)
	// Explicit routing must be able to bind while another request probes a slow
	// peer. Binding a host service does not launch that selected executable.
	bound := make(chan error, 1)
	go func() {
		_, err := server.executionServiceBehaviorWithAgent(t.Context(), "example.test/ready:1.0.0")
		bound <- err
	}()
	select {
	case err := <-bound:
		require.NoError(t, err)
	case <-time.After(time.Second):
		stop()
		<-bound
		t.Fatal("agent discovery retained the shared service mutex")
	}
	stop()
	select {
	case response = <-done:
		require.NotNil(t, response)
		require.False(t, response.Success)
		require.Contains(t, response.Output, "context canceled")
		done <- response
	case <-time.After(5 * time.Second):
		t.Fatal("request cancellation did not interrupt active discovery")
	}
}

func TestCodeUnitSelectionUsesDeclaredIdentityWithoutDiscovery(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	root := t.TempDir()
	writeCodeUnitFixture(t, root, "mind.yaml", "source_agents:\n  .: example.test/unknown:0.0.1\n")
	server, err := NewServer(Config{WorkDir: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	_, err = server.serviceBehaviorForCodeUnit(t.Context(), normalizedCodeUnitTarget{id: "root", path: ".", root: root}, "")
	require.NoError(t, err)
	require.Contains(t, server.codeUnitServices, "root\x00.\x00example.test/unknown:0.0.1")
}

func TestCodeUnitSelectionRejectsUnreachableConfigurationKeys(t *testing.T) {
	for _, key := range []string{"../escape", "./nested", "nested/../other"} {
		t.Run(key, func(t *testing.T) {
			root := t.TempDir()
			writeCodeUnitFixture(t, root, "mind.yaml", "source_agents:\n  "+key+": example.test/unknown\n")
			_, err := NewServer(Config{WorkDir: root})
			require.ErrorContains(t, err, "canonical unit paths")
		})
	}
}
