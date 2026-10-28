//go:build unix

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/codefly-dev/core/runners/base"
	"github.com/stretchr/testify/require"
)

// These fixtures are independently selected test children, not registered
// Codefly resources. All production delivery goes through Core.SignalProcess.
func startProcessExitChild(t *testing.T, command *exec.Cmd) (base.ProcessIdentity, <-chan error) {
	t.Helper()
	require.NoError(t, command.Start())
	identity, err := base.InspectProcess(command.Process.Pid)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		// The fixture's original birth remains the cleanup expectation.
		_ = base.SignalProcess(context.Background(), identity, syscall.SIGKILL)
	})
	return identity, done
}

func assertProcessExitRealReadback(t *testing.T, result processExitResult, err error) {
	t.Helper()
	require.True(t, result.SignalAttempted)
	require.True(t, result.SignalAccepted)
	if err != nil {
		// Linux can observe the exited child before its parent reaps it. Core
		// refuses incomplete inspection; the CLI must preserve that uncertainty.
		require.Equal(t, "unknown", result.Observation)
		require.False(t, result.ExpectedBirthGone)
		t.Logf("accepted delivery with uncertain readback (not successful absence): %v", err)
		return
	}
	require.Equal(t, "absent", result.Observation)
	require.True(t, result.ExpectedBirthGone)
}

func TestProcessExitRealTargetAndSameGroupSibling(t *testing.T) {
	target := exec.Command("sleep", "60")
	target.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	identity, done := startProcessExitChild(t, target)
	sibling := exec.Command("sleep", "60")
	sibling.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: identity.PGID}
	siblingIdentity, _ := startProcessExitChild(t, sibling)
	require.Equal(t, identity.PGID, siblingIdentity.PGID)

	var preview bytes.Buffer
	command := newRecoverProcessCommand(base.InspectProcess, base.SignalProcess)
	command.SetOut(&preview)
	command.SetArgs([]string{strconv.Itoa(identity.PID), "--preview"})
	require.NoError(t, command.Execute())
	path := filepath.Join(t.TempDir(), "receipt.json")
	require.NoError(t, os.WriteFile(path, preview.Bytes(), 0o600))
	var output bytes.Buffer
	command = newRecoverProcessCommand(base.InspectProcess, base.SignalProcess)
	command.SilenceErrors, command.SilenceUsage = true, true
	command.SetOut(&output)
	command.SetArgs([]string{strconv.Itoa(identity.PID), "--expect", path, "--force"})
	exitErr := command.Execute()
	var result processExitResult
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	assertProcessExitRealReadback(t, result, exitErr)
	select {
	case err := <-done:
		require.Error(t, err, "force-exited fixture should have a signal exit status")
	case <-time.After(time.Second):
		t.Fatal("fixture parent did not reap selected process")
	}
	// A separate invocation after the fixture's parent has reaped it must
	// confirm absence without another delivery. Production never auto-retries.
	output.Reset()
	command = newRecoverProcessCommand(base.InspectProcess, base.SignalProcess)
	command.SetOut(&output)
	command.SetArgs([]string{strconv.Itoa(identity.PID), "--expect", path, "--force"})
	require.NoError(t, command.Execute())
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	require.False(t, result.SignalAttempted)
	require.Equal(t, "absent", result.Observation)
	require.True(t, result.ExpectedBirthGone)
	actual, err := base.InspectProcess(siblingIdentity.PID)
	require.NoError(t, err)
	require.Equal(t, siblingIdentity, actual, "same-group sibling must survive exact-process delivery")

	// A stale birth expectation aimed at a live PID must not select its current
	// occupant. Actual OS PID reuse is nondeterministic; owner handles own that
	// delivery race, while this tests the CLI admission boundary with a live PID.
	stale := siblingIdentity
	stale.StartID++
	result, err = requestProcessExit(t.Context(), stale, syscall.SIGKILL, "force", base.InspectProcess, base.SignalProcess)
	require.Error(t, err)
	require.False(t, result.SignalAttempted)
	require.Equal(t, "changed_birth", result.Observation)
	actual, err = base.InspectProcess(siblingIdentity.PID)
	require.NoError(t, err)
	require.Equal(t, siblingIdentity, actual)
}

func TestProcessExitParentReactionHelper(t *testing.T) {
	switch os.Getenv("CLI934_PROCESS_EXIT_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestProcessExitParentReactionHelper$")
		child.Env = append(os.Environ(), "CLI934_PROCESS_EXIT_HELPER=watcher")
		require.NoError(t, child.Start())
		// The watcher writes readiness after capturing this parent's identity.
		for {
			time.Sleep(time.Second)
		}
	case "watcher":
		parent := os.Getppid()
		require.NoError(t, os.WriteFile(os.Getenv("CLI934_PROCESS_EXIT_READY"), []byte(strconv.Itoa(os.Getpid())), 0o600))
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if os.Getppid() != parent {
				require.NoError(t, os.WriteFile(os.Getenv("CLI934_PROCESS_EXIT_REACTION"), []byte("parent loss triggered child behavior\n"), 0o600))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("fixture parent never exited")
	}
}

func TestProcessExitCanTriggerIndirectChildReaction(t *testing.T) {
	ready, reaction := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "reaction")
	parent := exec.Command(os.Args[0], "-test.run=^TestProcessExitParentReactionHelper$")
	parent.Env = append(os.Environ(), "CLI934_PROCESS_EXIT_HELPER=parent", "CLI934_PROCESS_EXIT_READY="+ready, "CLI934_PROCESS_EXIT_REACTION="+reaction)
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	identity, done := startProcessExitChild(t, parent)
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 5*time.Second, 10*time.Millisecond)
	result, err := requestProcessExit(t.Context(), identity, syscall.SIGKILL, "force", base.InspectProcess, base.SignalProcess)
	assertProcessExitRealReadback(t, result, err)
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("fixture parent was not reaped")
	}
	_, err = base.InspectProcess(identity.PID)
	require.ErrorIs(t, err, base.ErrProcessNotFound)
	require.Eventually(t, func() bool { _, err := os.Stat(reaction); return err == nil }, 5*time.Second, 10*time.Millisecond)
	// This is intentional contrary evidence: one-PID delivery does not prove
	// zero child effects. It qualifies no historical agent cleanup implementation.
}

// TestProcessExitGracefulHelper is the real SIGTERM target. It installs a
// handler that writes a durable marker and only then exits, which is the whole
// point of --graceful: a store holding authoritative state must get to finish
// writing. "ignore" instead refuses to die, so the timeout can be observed.
func TestProcessExitGracefulHelper(t *testing.T) {
	switch os.Getenv("CLI934_PROCESS_EXIT_GRACEFUL") {
	case "handler":
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		// Readiness AFTER the handler is installed: a SIGTERM delivered before
		// that point takes the default disposition and the target dies without
		// running its shutdown, which would make this test prove nothing.
		require.NoError(t, os.WriteFile(os.Getenv("CLI934_PROCESS_EXIT_READY"), []byte("ready"), 0o600))
		<-signals
		require.NoError(t, os.WriteFile(os.Getenv("CLI934_PROCESS_EXIT_MARKER"), []byte("shutdown written before exit\n"), 0o600))
		os.Exit(0)
	case "ignore":
		signal.Ignore(syscall.SIGTERM)
		require.NoError(t, os.WriteFile(os.Getenv("CLI934_PROCESS_EXIT_READY"), []byte("ready"), 0o600))
		for {
			time.Sleep(time.Second)
		}
	}
}

// TestProcessExitGracefulLetsTheTargetWriteBeforeExit is the positive: one
// SIGTERM, the target's own shutdown runs to completion, and the result names
// the mode and signal that were requested.
func TestProcessExitGracefulLetsTheTargetWriteBeforeExit(t *testing.T) {
	dir := t.TempDir()
	marker, ready := filepath.Join(dir, "marker"), filepath.Join(dir, "ready")
	target := exec.Command(os.Args[0], "-test.run=^TestProcessExitGracefulHelper$")
	target.Env = append(os.Environ(), "CLI934_PROCESS_EXIT_GRACEFUL=handler",
		"CLI934_PROCESS_EXIT_MARKER="+marker, "CLI934_PROCESS_EXIT_READY="+ready)
	target.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	identity, done := startProcessExitChild(t, target)
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second, 10*time.Millisecond,
		"the target must have installed its SIGTERM handler first")

	result, err := requestProcessExit(t.Context(), identity, syscall.SIGTERM, "graceful", base.InspectProcess, base.SignalProcess)
	require.NoError(t, err)
	require.True(t, result.SignalAttempted)
	require.True(t, result.SignalAccepted)
	require.Equal(t, "absent", result.Observation)
	require.True(t, result.ExpectedBirthGone)
	require.Equal(t, "graceful", result.Mode)
	require.Equal(t, syscall.SIGTERM.String(), result.Signal)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("graceful target never exited")
	}
	body, readErr := os.ReadFile(marker)
	require.NoError(t, readErr, "the target's own shutdown must have run")
	require.Contains(t, string(body), "shutdown written before exit")
}

// TestProcessExitGracefulNeverEscalates is the protection the disposition is
// really about: a target that ignores SIGTERM stays ALIVE when the polling
// budget expires. The command reports the exit unconfirmed and sends nothing
// else — no SIGKILL fallback — so a store that did not finish writing is never
// taken down by this command.
func TestProcessExitGracefulNeverEscalates(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	target := exec.Command(os.Args[0], "-test.run=^TestProcessExitGracefulHelper$")
	target.Env = append(os.Environ(), "CLI934_PROCESS_EXIT_GRACEFUL=ignore", "CLI934_PROCESS_EXIT_READY="+ready)
	target.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	identity, _ := startProcessExitChild(t, target)
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second, 10*time.Millisecond,
		"the target must be ignoring SIGTERM before it is sent")

	var sent []syscall.Signal
	result, err := requestProcessExit(t.Context(), identity, syscall.SIGTERM, "graceful", base.InspectProcess,
		func(ctx context.Context, id base.ProcessIdentity, sig syscall.Signal) error {
			sent = append(sent, sig)
			return base.SignalProcess(ctx, id, sig)
		})
	require.ErrorContains(t, err, "unconfirmed")
	require.Equal(t, []syscall.Signal{syscall.SIGTERM}, sent, "exactly one signal, and never SIGKILL")
	require.Equal(t, "same_birth", result.Observation)
	require.False(t, result.ExpectedBirthGone)
	require.Equal(t, "graceful", result.Mode)

	// And it is still running, at its original birth.
	alive, inspectErr := base.InspectProcess(identity.PID)
	require.NoError(t, inspectErr)
	require.Equal(t, identity, alive, "an unconfirmed graceful exit leaves the target untouched")
}
