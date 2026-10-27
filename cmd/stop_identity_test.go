package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/processgroup"
	runners "github.com/codefly-dev/core/runners/base"
)

func TestStopReportsIncompatibleBootIdentityWithoutDiscardingEvidence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := psWorkspace(t)
	t.Chdir(workspace)
	child := exec.Command(os.Args[0], "-test.run=^TestPsHelper$")
	child.Env = append(os.Environ(), psHelperEnv+"=1", processgroup.LaunchWorkspaceEnv+"="+workspace)
	child.Dir = workspace
	group, err := runners.StartTrackedProcessGroup(child)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if found, ok := processgroup.LaunchWorkspaceOfProcess(child.Process.Pid); ok && found == workspace {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never inherited workspace")
		}
		time.Sleep(time.Millisecond)
	}
	path := filepath.Join(home, ".codefly", "runs", "authenticated-v1", fmt.Sprintf("%d.pgid", group.PGID()))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"leader", "owner"} {
		record[key].(map[string]any)["boot_id"] = "incompatible-recorded-boot"
	}
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	err = clearCommand(context.Background(), nil, stopClearOptions())
	if err == nil || !strings.Contains(err.Error(), "incompatible boot identity") {
		t.Fatalf("stop reported %v", err)
	}
	if retained, err := os.ReadFile(path); err != nil || !bytes.Equal(retained, data) {
		t.Fatalf("stop changed recovery evidence: %v", err)
	}
	if err := syscall.Kill(child.Process.Pid, 0); err != nil {
		t.Fatalf("stop signaled unauthenticated process: %v", err)
	}
}
