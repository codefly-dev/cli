package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/codefly-dev/cli/pkg/processgroup"
	runners "github.com/codefly-dev/core/runners/base"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStopIdentityFallbackOwner(t *testing.T) {
	target := os.Getenv("CODEFLY_TEST_STOP_IDENTITY_CHILD_PID")
	if target == "" {
		return
	}
	child := exec.Command(os.Getenv("CODEFLY_TEST_STOP_IDENTITY_CHILD_BINARY"), "-test.run=^TestPsHelper$")
	child.Env = append(os.Environ(), psHelperEnv+"=1")
	child.Stdout = io.Discard
	child.Stderr = io.Discard
	group, err := runners.StartTrackedProcessGroup(child)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, []byte(strconv.Itoa(group.PGID())), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestStopDoesNotFallbackAfterBootIdentityRefusal(t *testing.T) { testStopIdentityRefusal(t, false) }
func TestStopDoesNotKillAgentBeforeBootIdentityRefusal(t *testing.T) {
	testStopIdentityRefusal(t, true)
}
func testStopIdentityRefusal(t *testing.T, agentShape bool) {
	t.Setenv("HOME", t.TempDir())
	workspace := psWorkspace(t)
	t.Chdir(workspace)
	t.Setenv(processgroup.LaunchWorkspaceEnv, workspace)
	binary := filepath.Join(t.TempDir(), ".cache", "native", "stop-child")
	if agentShape {
		binary = filepath.Join(t.TempDir(), ".codefly", "agents", "stop-child")
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(binary, executable, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEFLY_TEST_STOP_IDENTITY_CHILD_BINARY", binary)
	pidFile := filepath.Join(t.TempDir(), "pid")
	owner := exec.Command(os.Args[0], "-test.run=^TestStopIdentityFallbackOwner$")
	owner.Env = append(os.Environ(), "CODEFLY_TEST_STOP_IDENTITY_CHILD_PID="+pidFile)
	if out, err := owner.CombinedOutput(); err != nil {
		t.Fatalf("owner: %v %s", err, out)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	path := filepath.Join(os.Getenv("HOME"), ".codefly", "runs", "authenticated-v2", fmt.Sprintf("%d.pgid", pid))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"leader", "owner"} {
		record[k].(map[string]any)["boot_id"] = "incompatible-recorded-boot"
	}
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if !agentShape {
		deadline := time.Now().Add(5 * time.Second)
		for {
			rows, scanErr := scanRunProcesses(context.Background(), processgroup.InWorkspace(workspace))
			if scanErr != nil {
				t.Fatal(scanErr)
			}
			found := false
			for _, row := range rows {
				found = found || (row.PID == pid && row.Kind == "native" && row.Status == "orphaned")
			}
			if found {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("child never became eligible for orphan fallback")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	err = clearCommand(context.Background(), nil, stopClearOptions())
	if err == nil || !strings.Contains(err.Error(), "incompatible boot identity") {
		t.Fatalf("expected boot refusal: %v", err)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("stop killed incompatible registered group %d through fallback after refusal", pid)
	}
	if retained, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(retained, data) {
		t.Fatalf("stop changed incompatible record: %v", readErr)
	}
}
