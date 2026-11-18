package common

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stoppingCommandEnv makes this test binary stand in for a long-running
// command: it installs SignalContext, says when it is ready, and once it is
// asked to stop takes the time named here to stop in order before it exits 0.
const stoppingCommandEnv = "SIGNAL_TEST_STOPPING_COMMAND"

func TestStoppingCommandHelper(t *testing.T) {
	teardown := os.Getenv(stoppingCommandEnv)
	if teardown == "" {
		return
	}
	stopsIn, err := time.ParseDuration(teardown)
	if err != nil {
		os.Exit(3)
	}
	ctx, stop := SignalContext(context.Background())
	defer stop()
	_, _ = os.Stdout.WriteString("ready\n")
	<-ctx.Done()
	time.Sleep(stopsIn)
	_, _ = os.Stdout.WriteString("stopped in order\n")
	os.Exit(0)
}

// stoppingCommand starts the helper and waits until it handles signals.
func stoppingCommand(t *testing.T, stopsIn time.Duration) (*exec.Cmd, *strings.Builder) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestStoppingCommandHelper$")
	command.Env = append(os.Environ(), stoppingCommandEnv+"="+stopsIn.String())
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	output := &strings.Builder{}
	ready := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 256)
		announced := false
		for {
			n, err := stdout.Read(buffer)
			output.Write(buffer[:n])
			if !announced && strings.Contains(output.String(), "ready") {
				announced = true
				close(ready)
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(20 * time.Second):
		t.Fatal("the command never handled signals")
	}
	t.Cleanup(func() { <-done })
	return command, output
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// A signal that arrives twice in the same instant is one request to stop, not
// a person insisting: whatever stops a process group signals it, and a parent
// in that group (make, a shell) forwards the signal to its child again. The
// second delivery used to force the exit, 0.2 s into a stop that had barely
// begun, and left every agent, server and store of the run behind.
func TestASignalRepeatedAtOnceStopsInOrder(t *testing.T) {
	for _, repeat := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run("SIGTERM then "+repeat.String(), func(t *testing.T) {
			command, output := stoppingCommand(t, 700*time.Millisecond)
			if err := command.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			if err := command.Process.Signal(repeat); err != nil {
				t.Fatal(err)
			}
			err := command.Wait()
			if code := exitCode(err); code != 0 {
				t.Fatalf("the command exited %d before it had stopped (%v): %q", code, err, output.String())
			}
			if !strings.Contains(output.String(), "stopped in order") {
				t.Fatalf("the command did not stop in order: %q", output.String())
			}
		})
	}
}

// A second signal after the interval is a person insisting on a stop that
// does not end: it still forces the exit.
func TestASignalRepeatedAfterTheIntervalForcesTheExit(t *testing.T) {
	command, output := stoppingCommand(t, time.Minute)
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(ForceQuitInterval + 200*time.Millisecond)
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		if code := exitCode(err); code != 130 {
			t.Fatalf("the forced exit is %d (%v), want 130: %q", code, err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a deliberate second signal did not force the exit")
	}
	if strings.Contains(output.String(), "stopped in order") {
		t.Fatalf("the command was not forced: %q", output.String())
	}
}
