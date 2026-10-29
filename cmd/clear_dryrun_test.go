package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/cli/pkg/processgroup"
)

// capturedLogs keeps every line a command writes, with its fields.
type capturedLogs struct {
	mu   sync.Mutex
	logs []*wool.Log
}

func (c *capturedLogs) Process(msg *wool.Log) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, msg)
}

// line is the captured log whose message is msg, with its fields as text.
func (c *capturedLogs) line(msg string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.logs {
		if l.Message != msg {
			continue
		}
		var fields []string
		for _, f := range l.Fields {
			fields = append(fields, fmt.Sprintf("%s=%v", f.Key, f.Value))
		}
		return strings.Join(fields, " "), true
	}
	return "", false
}

// trackedSleep starts a real tracked process group, killed with the test.
func trackedSleep(t *testing.T) int {
	t.Helper()
	group, err := runners.StartTrackedProcessGroup(exec.Command("sleep", "60"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-group.PGID(), syscall.SIGKILL) })
	return group.PGID()
}

// recordedByAnOlderAgent rewrites a group's record as an older Darwin writer
// left it: one clock-derived boot id for leader and owner, which Core refuses
// to decide while the leader's pid and start are unchanged.
func recordedByAnOlderAgent(t *testing.T, home string, pgid int) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, ".codefly", "*", "authenticated-v2", strconv.Itoa(pgid)+".pgid"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("record for %d: %v %v", pgid, paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"leader", "owner"} {
		identity, ok := record[field].(map[string]any)
		if !ok {
			t.Fatalf("record has no %s: %s", field, raw)
		}
		identity["boot_id"] = "1793000000:123456"
	}
	out, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[0], out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A dry run with one group Core cannot decide still says everything it can:
// the healthy group it would stop, a warning naming the kept group and its
// recovery, and the stale sockets it would remove; and it returns the
// refusal rather than hiding it (cli#1233).
func TestClearDryRunReportsPastAnUndecidableGroup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	healthy, undecidable := trackedSleep(t), trackedSleep(t)
	recordedByAnOlderAgent(t, home, undecidable)

	// One stale agent socket: its owner's pid is dead.
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(os.TempDir(), fmt.Sprintf("codefly-uds-%d-x", gone.Process.Pid)), 0o700); err != nil {
		t.Fatal(err)
	}

	logs := &capturedLogs{}
	w := wool.Get(context.Background()).WithLogger(logs)
	failures := sweepStaleState(context.Background(), w, clearOptions{dryRun: true, scope: processgroup.AllWorkspaces()})

	groups, ok := logs.line("would stop registered process groups")
	if !ok || !strings.Contains(groups, strconv.Itoa(healthy)) || strings.Contains(groups, strconv.Itoa(undecidable)) {
		t.Fatalf("would stop %q (logged %v), want only the healthy group %d", groups, ok, healthy)
	}
	warning, ok := logs.line("would keep process groups whose ownership cannot be decided")
	if !ok || !strings.Contains(warning, "codefly recover process-group "+strconv.Itoa(undecidable)) {
		t.Fatalf("ownership warning %q (logged %v) does not name group %d's recovery", warning, ok, undecidable)
	}
	if sockets, ok := logs.line("would remove stale agent sockets"); !ok || !strings.Contains(sockets, "count=1") {
		t.Fatalf("stale sockets %q (logged %v), want count 1", sockets, ok)
	}
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "inspect registered group "+strconv.Itoa(undecidable)) {
		t.Fatalf("returned %v, want the undecidable group's refusal", failures)
	}
	if syscall.Kill(-healthy, 0) != nil || syscall.Kill(-undecidable, 0) != nil {
		t.Fatal("the dry run signaled a group")
	}
}
