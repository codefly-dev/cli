package cmd

import (
	"context"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestClearAndStopCommandsReturnErrors(t *testing.T) {
	for name, command := range map[string]struct {
		run  bool
		runE bool
	}{
		"clear": {run: ClearCmd.Run != nil, runE: ClearCmd.RunE != nil},
		"stop":  {run: StopCmd.Run != nil, runE: StopCmd.RunE != nil},
	} {
		if command.run || !command.runE {
			t.Fatalf("%s must return errors through RunE", name)
		}
	}
}

func TestStopOptionsDoNotInheritClearFlags(t *testing.T) {
	clearKeepProcesses = true
	clearKeepContainers = false
	clearDryRun = true
	t.Cleanup(func() {
		clearKeepProcesses = false
		clearKeepContainers = false
		clearDryRun = false
	})

	got := stopClearOptions()
	if got.verb != "stop" || got.keepProcesses || !got.keepContainers || got.dryRun {
		t.Fatalf("stop options inherited clear state: %+v", got)
	}
}

func TestParseCodeflyOwnedPIDs(t *testing.T) {
	out := []byte(`  10 /usr/local/bin/codefly run service api
  11 /Users/me/.codefly/agents/go/bin/agent serve
  12 /usr/bin/editor /repo/codefly.dev/file.go
  13 /usr/local/bin/codefly stop
bad /usr/local/bin/codefly
`)
	if got, want := parseCodeflyOwnedPIDs(out, 13), []int{10, 11}; !reflect.DeepEqual(got, want) {
		t.Fatalf("owned PIDs = %v, want %v", got, want)
	}
}

func TestCodeflyOwnedPIDsReturnsProcessErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := codeflyOwnedPIDs(ctx, -1); err == nil {
		t.Fatal("expected cancelled process enumeration to return an error")
	}
}

// TestWaitProcessesExitedBlocksUntilGone guards the reparent race: clear must not
// sweep for orphaned natives until the agents it SIGKILLed have actually exited.
// A no-op wait (the original bug) would return immediately while the process is
// still alive, so the first half asserts the wait genuinely blocks; the second
// asserts it returns promptly once the process is gone.
func TestWaitProcessesExitedBlocksUntilGone(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})

	start := time.Now()
	waitProcessesExited(context.Background(), []int{pid}, 300*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("waitProcessesExited returned after %s while the process was still alive; expected to wait to the timeout", elapsed)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-reaped

	start = time.Now()
	waitProcessesExited(context.Background(), []int{pid}, 5*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waitProcessesExited blocked %s after the process exited; expected a prompt return", elapsed)
	}
}

// TestWaitProcessesExitedHonorsCancelledContext ensures a wedged process cannot
// hang clear once the command's context is cancelled (e.g. a second Ctrl-C).
func TestWaitProcessesExitedHonorsCancelledContext(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	waitProcessesExited(ctx, []int{pid}, time.Minute)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waitProcessesExited ignored the cancelled context and blocked %s", elapsed)
	}
}
