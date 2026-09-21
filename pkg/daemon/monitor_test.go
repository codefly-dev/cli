package daemon

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/codefly-dev/core/runners/base"
	"github.com/stretchr/testify/require"
)

func TestMonitorRequiresAuthenticatedMembershipNotExecutableName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	command := exec.Command("sleep", "60")
	group, err := base.StartTrackedProcessGroup(command)
	require.NoError(t, err)
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, group.Terminate(ctx))
		select {
		case <-waited:
		case <-ctx.Done():
			t.Error("managed process did not exit")
		}
		require.NoError(t, group.RemoveIfDead())
	})
	processes, err := checkProcesses(t.Context())
	require.NoError(t, err)
	found := false
	for _, process := range processes {
		require.NotEqual(t, os.Getpid(), process.PID, "unregistered caller is not managed by its executable name")
		if process.PID == command.Process.Pid {
			found = true
			require.True(t, process.OwnerAlive)
			require.Equal(t, group.PGID(), process.PGID)
		}
	}
	require.True(t, found, "a registered arbitrary executable must be monitored")
	result, err := monitor(t.Context(), MonitorConfig{CPUThreshold: 10000, MemoryMB: 10000, MaxOrphans: 0})
	require.NoError(t, err)
	require.Empty(t, result.Warnings, "live owned processes are not orphaned agents")
	require.Empty(t, result.Killed)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = checkProcesses(ctx)
	require.Error(t, err, "cancellation cannot report a successful empty observation")
}

func TestParseMonitorProcessColumns(t *testing.T) {
	info := parsePSLine("123 120 1.2 2048 /some path/new-tool")
	require.NotNil(t, info)
	require.Equal(t, 123, info.PID)
	require.Equal(t, 120, info.PGID)
	require.Equal(t, "new-tool", info.Name)
	for _, invalid := range []string{"", "pid pgid cpu rss name", "1 0 0 2 executable", "1 2 0 bad executable"} {
		require.Nil(t, parsePSLine(invalid))
	}
}

func TestRunMonitorLoopStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunMonitorLoop(ctx, MonitorConfig{CheckInterval: time.Hour})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunMonitorLoop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunMonitorLoop ignored cancellation")
	}
}
