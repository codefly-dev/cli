// Package killedrun leaves on the machine, for a test, what a disposable run
// that was killed leaves: a native state directory claimed by an agent that is
// gone, launched by a run that is gone, and the server that agent started
// there, still running.
//
// The processes are real, because the records core's runners/runstate keeps
// are its own: the test binary re-executes itself as the run, which starts
// itself again as the agent, which claims the directory, starts a server and
// tracks it. Both are then killed.
//
// A test binary that uses Leave declares the entry the helper processes run:
//
//	func TestKilledRunHelper(t *testing.T) { killedrun.Helper() }
package killedrun

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codefly-dev/core/runners/runstate"
)

const (
	// Entry is the test the helper processes run.
	Entry = "TestKilledRunHelper"

	roleEnv       = "KILLEDRUN_ROLE"
	dirEnv        = "KILLEDRUN_DIR"
	workspaceEnv  = "KILLEDRUN_WORKSPACE"
	scopeEnv      = "KILLEDRUN_NAMING_SCOPE"
	disposableEnv = "KILLEDRUN_DISPOSABLE"
	reportEnv     = "KILLEDRUN_REPORT"

	// serverLife bounds the server, so a failed test cannot leave it for good.
	serverLife = "120"
)

// Helper is the run or the agent when the binary was started as one, and
// returns at once otherwise.
func Helper() {
	switch os.Getenv(roleEnv) {
	case "run":
		agent := exec.Command(os.Args[0], "-test.run=^"+Entry+"$") //nolint:gosec // the test binary itself
		agent.Env = append(os.Environ(), roleEnv+"=agent")
		if err := agent.Start(); err != nil {
			fail(err)
		}
		_ = agent.Wait()
		os.Exit(0)
	case "agent":
		run := runstate.Run{
			Workspace:   os.Getenv(workspaceEnv),
			NamingScope: os.Getenv(scopeEnv),
			Disposable:  os.Getenv(disposableEnv) == "true",
		}
		hold, err := runstate.Claim(os.Getenv(dirEnv), run)
		if err != nil {
			fail(err)
		}
		server := exec.Command("sleep", serverLife)
		// Its own group: the server of a store outlives what started it.
		server.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := server.Start(); err != nil {
			fail(err)
		}
		if err := hold.Track(server.Process.Pid); err != nil {
			_ = server.Process.Kill()
			fail(err)
		}
		report := fmt.Sprintf("%d %d", os.Getpid(), server.Process.Pid)
		if err := writeReport(os.Getenv(reportEnv), report); err != nil {
			_ = server.Process.Kill()
			fail(err)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

// writeReport confines the helper's two writes to the test-owned directory.
func writeReport(path, report string) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	if filepath.Base(path) != "report" {
		return fmt.Errorf("unexpected helper report name")
	}
	if err := root.WriteFile("report.tmp", []byte(report), 0o600); err != nil {
		return err
	}
	return root.Rename("report.tmp", "report")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "killedrun:", err)
	os.Exit(1)
}

// Server is the server a killed run left running.
type Server struct{ PID int }

// Running reports whether the server still runs.
func (s Server) Running() bool { return running(s.PID) }

// Leave creates dir as the native state of run, claimed by an agent that
// started a server in it, kills the run and the agent, and answers the server
// that survived them. The server is killed when the test ends.
func Leave(t *testing.T, dir string, run runstate.Run) Server {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(t.TempDir(), "report")
	launcher := exec.Command(os.Args[0], "-test.run=^"+Entry+"$") //nolint:gosec // the test binary itself
	launcher.Env = append(os.Environ(),
		roleEnv+"=run", dirEnv+"="+dir, reportEnv+"="+report,
		workspaceEnv+"="+run.Workspace, scopeEnv+"="+run.NamingScope, disposableEnv+"="+strconv.FormatBool(run.Disposable))
	launcher.Stderr = os.Stderr
	if err := launcher.Start(); err != nil {
		t.Fatal(err)
	}
	var agent, server int
	deadline := time.Now().Add(20 * time.Second)
	for {
		if data, err := os.ReadFile(report); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 2 {
				agent, _ = strconv.Atoi(fields[0])
				server, _ = strconv.Atoi(fields[1])
				break
			}
		}
		if time.Now().After(deadline) {
			_ = launcher.Process.Kill()
			_ = launcher.Wait()
			t.Fatal("the run's agent never reported its server")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The server is this test's own: started by the agent it started, whose
	// report it just read. It was started as its own group leader.
	t.Cleanup(func() { _ = syscall.Kill(-server, syscall.SIGKILL) })

	// `kill -9` of the run, then of the agent it orphaned.
	_ = launcher.Process.Kill()
	_ = launcher.Wait()
	_ = syscall.Kill(agent, syscall.SIGKILL)
	for running(agent) {
		if time.Now().After(deadline) {
			t.Fatalf("agent %d did not die", agent)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !running(server) {
		t.Fatalf("server %d did not survive its run", server)
	}
	return Server{PID: server}
}

// running reports whether pid names a process that is not a zombie waiting to
// be reaped.
func running(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// RequireStopped fails the test unless the server ends shortly.
func (s Server) RequireStopped(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.Running() {
		if time.Now().After(deadline) {
			t.Fatalf("server %d still runs", s.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
