package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/codefly-dev/core/runners/base"
)

// Use the existing registered-owner subprocess fixture. No registry format or
// compatibility interpretation lives in this CLI test; all observations and
// effects go through Core's public API in a private home.
func recoveryCommandOrphan(t *testing.T) int {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "pid")
	owner := exec.Command(os.Args[0], "-test.run=^TestStopIdentityFallbackOwner$")
	owner.Env = append(os.Environ(), "CODEFLY_TEST_STOP_IDENTITY_CHILD_PID="+pidFile,
		"CODEFLY_TEST_STOP_IDENTITY_CHILD_BINARY="+os.Args[0])
	if out, err := owner.CombinedOutput(); err != nil {
		t.Fatalf("start isolated owner: %v: %s", err, out)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	group, err := base.LookupProcessGroup(pgid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := group.Terminate(context.Background()); err != nil {
			t.Errorf("fixture cleanup: %v", err)
		}
		if err := group.RemoveIfDead(); err != nil {
			t.Errorf("fixture record cleanup: %v", err)
		}
	})
	return pgid
}

func TestRecoveryCommandRealOwnerBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEFLY_HOME", t.TempDir())
	target, sibling := recoveryCommandOrphan(t), recoveryCommandOrphan(t)
	foreign := exec.Command(os.Args[0], "-test.run=^TestPsHelper$")
	foreign.Env = append(os.Environ(), psHelperEnv+"=1")
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Process.Kill(); _ = foreign.Wait() })
	foreignBefore, err := base.InspectProcess(foreign.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	siblingBefore, err := base.PreviewProcessGroupRecovery(t.Context(), sibling)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) ([]byte, error) {
		var out bytes.Buffer
		command := newRecoverCommand(base.PreviewProcessGroupRecovery, base.RecoverProcessGroup)
		command.SetOut(&out)
		command.SetErr(&bytes.Buffer{})
		command.SilenceErrors, command.SilenceUsage = true, true
		command.SetArgs(append([]string{"process-group"}, args...))
		err := command.Execute()
		return out.Bytes(), err
	}
	pgid := strconv.Itoa(target)
	raw, err := run(pgid, "--preview")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := base.ParseProcessGroupRecoveryExpectation(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Preview is observational; repeat produces the same exact receipt.
	again, err := base.PreviewProcessGroupRecovery(t.Context(), target)
	if err != nil || again == nil || !reflect.DeepEqual(expected, *again) {
		t.Fatalf("preview changed target: %v", err)
	}
	path := filepath.Join(t.TempDir(), "receipt.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(strconv.Itoa(sibling), "--expect", path); err == nil {
		t.Fatal("target mismatch succeeded")
	}
	changed := expected
	changed.Members = append([]base.ProcessIdentity(nil), expected.Members...)
	changed.Members[0].StartID++
	bad, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(t.TempDir(), "changed.json")
	if err := os.WriteFile(badPath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(pgid, "--expect", badPath); err == nil {
		t.Fatal("changed birth succeeded")
	}
	again, err = base.PreviewProcessGroupRecovery(t.Context(), target)
	if err != nil || again == nil || !reflect.DeepEqual(expected, *again) {
		t.Fatalf("refused operation changed target: %v", err)
	}
	out, err := run(pgid, "--expect", path)
	if err != nil {
		t.Fatalf("recover: %v, result %s", err, out)
	}
	var result base.ProcessGroupRecoveryResult
	if err := json.Unmarshal(out, &result); err != nil || !result.GroupDead || !result.RecordRemoved {
		t.Fatalf("recovery postconditions: %s, %v", out, err)
	}
	if _, err := base.LookupProcessGroup(target); !errors.Is(err, base.ErrProcessGroupNotRegistered) {
		t.Fatalf("target registration survived: %v", err)
	}
	for _, member := range expected.Members {
		if _, err := base.InspectProcess(member.PID); !errors.Is(err, base.ErrProcessNotFound) {
			t.Fatalf("target member %d survived: %v", member.PID, err)
		}
	}
	siblingAfter, err := base.PreviewProcessGroupRecovery(t.Context(), sibling)
	if err != nil || !reflect.DeepEqual(siblingBefore, siblingAfter) {
		t.Fatalf("sibling evidence changed: %v", err)
	}
	foreignAfter, err := base.InspectProcess(foreign.Process.Pid)
	if err != nil || foreignBefore != foreignAfter {
		t.Fatalf("unregistered control changed: %v", err)
	}
	t.Logf("isolated target %d recovered; registered sibling %d and unregistered control %d preserved", target, sibling, foreign.Process.Pid)
}
