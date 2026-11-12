package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/workingrender"
)

func TestDoctorWorkspaceWarnsAboutAnActiveDevDeployment(t *testing.T) {
	clean := singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, nil, nil)
	requireNoCode(t, runReadiness(t, workspaceReadinessOptions{dir: clean}), codeGitOpsDevDeploymentActive)

	inventory := gitops.Inventory{
		SchemaVersion: gitops.SchemaVersion, Module: "backend", Environment: "staging",
		Units: []gitops.InventoryUnit{}, Files: []gitops.InventoryFile{},
		Dev: []gitops.InventoryDevDeployment{{
			Service: "api", Origin: gitops.DevSourceFlag, Source: "/src/api", Commit: "0123456789ab", Dirty: true,
			Image:      "registry.example.com/acme/api@sha256:" + strings.Repeat("b", 64),
			Digest:     "sha256:" + strings.Repeat("b", 64),
			DeployedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC),
		}},
	}
	data, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Staging delivers from deployments/modules on main of this checkout's
	// repository: a dev deployment is found in its working render, in its
	// delivered tree as another clone published it, and, in both, reported
	// once.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	working := filepath.Join(workingrender.Dir, "staging", "modules", "backend", gitops.InventoryFilename)
	delivered := filepath.Join("deployments", "modules", "backend", gitops.InventoryFilename)
	yaml := testWorkspaceYAMLLocalDeclared + "    - name: staging\n      gitops:\n        repo-url: https://github.com/acme/platform.git\n"
	for name, files := range map[string][]string{"working": {working}, "delivered": {delivered}, "both": {working, delivered}} {
		extra := map[string]string{}
		for _, file := range files {
			extra[file] = string(data) + "\n"
		}
		checkout := singleServiceWorkspace(t, yaml, nil, extra)
		for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"remote", "add", "origin", "git@github.com:acme/platform.git"}} {
			if out, err := exec.Command("git", append([]string{"-C", checkout}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
		found := findDiagnostics(runReadiness(t, workspaceReadinessOptions{dir: checkout}), codeGitOpsDevDeploymentActive)
		if len(found) != 1 {
			t.Fatalf("%s: %d dev deployment diagnostics, want exactly one", name, len(found))
		}
	}
	dir := singleServiceWorkspace(t, yaml, nil, map[string]string{working: string(data) + "\n"})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	diagnostic := requireCode(t, report, codeGitOpsDevDeploymentActive, "warn")
	for _, want := range []string{"staging", "backend/api", "/src/api", "(dirty)", "no release describes"} {
		if !strings.Contains(diagnostic.Message, want) {
			t.Fatalf("message %q does not mention %q", diagnostic.Message, want)
		}
	}
	if !strings.Contains(diagnostic.Remediation, "codefly deploy gitops render backend --env staging") {
		t.Fatalf("remediation %q does not name the full render", diagnostic.Remediation)
	}
	if report.Status != readinessStatusReady {
		t.Fatalf("a dev deployment is a warning, not a failure: status %s", report.Status)
	}
}
