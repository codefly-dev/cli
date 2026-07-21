package integrity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestVerifyBaseReportsOnlyUnallowedComposedDrift(t *testing.T) {
	root, workspace := baseFixture(t)
	moduleDir := filepath.Join(root, "modules", "app")

	writeTestFile(t, filepath.Join(moduleDir, "root.txt"), "canonical root")
	writeTestFile(t, filepath.Join(moduleDir, "allowed.txt"), "canonical allowed")
	writeTestFile(t, filepath.Join(moduleDir, "missing.txt"), "canonical missing")
	writeTestFile(t, filepath.Join(moduleDir, "services", "kept", "generated.txt"), "canonical kept")
	manifest := baseManifest{Files: map[string]string{}}
	for _, relative := range []string{"root.txt", "allowed.txt", "missing.txt", "services/kept/generated.txt"} {
		digest, err := sha256File(filepath.Join(moduleDir, relative))
		if err != nil {
			t.Fatal(err)
		}
		manifest.Files[relative] = digest
	}
	// This service is deliberately not composed in module.codefly.yaml. Its
	// canonical file may be absent without becoming local drift.
	manifest.Files["services/omitted/generated.txt"] = "unused digest"
	writeTestJSON(t, filepath.Join(moduleDir, "tools", "base-manifest.json"), manifest)
	writeTestJSON(t, filepath.Join(moduleDir, "tools", "base-integrity-allow.json"), map[string]any{
		"allowed.txt": "starter-owned customization",
		"requiredAdditions": map[string]string{
			"product/plugin.json": "product plugin installation",
		},
	})

	writeTestFile(t, filepath.Join(moduleDir, "root.txt"), "locally modified")
	writeTestFile(t, filepath.Join(moduleDir, "allowed.txt"), "allowed divergence")
	if err := os.Remove(filepath.Join(moduleDir, "missing.txt")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(moduleDir, "product", "plugin.json"), "{}")

	report, err := VerifyBase(context.Background(), workspace)
	if err == nil {
		t.Fatal("base verification unexpectedly passed")
	}
	if report.Failed() != 1 || len(report.Modules) != 1 {
		t.Fatalf("report = %#v", report)
	}
	module := report.Modules[0]
	if !reflect.DeepEqual(module.Modified, []string{"root.txt"}) {
		t.Fatalf("modified = %v", module.Modified)
	}
	if !reflect.DeepEqual(module.Missing, []string{"missing.txt"}) {
		t.Fatalf("missing = %v", module.Missing)
	}
	if !reflect.DeepEqual(module.Allowed, []AllowedDivergence{{Path: "allowed.txt", Reason: "starter-owned customization"}}) {
		t.Fatalf("allowed = %#v", module.Allowed)
	}
	if !reflect.DeepEqual(module.Omitted, map[string]int{"omitted": 1}) {
		t.Fatalf("omitted = %#v", module.Omitted)
	}
}

func TestVerifyBaseFailsWhenRequiredConsumerAdditionIsMissing(t *testing.T) {
	root, workspace := baseFixture(t)
	moduleDir := filepath.Join(root, "modules", "app")
	writeTestJSON(t, filepath.Join(moduleDir, "tools", "base-manifest.json"), baseManifest{Files: map[string]string{}})
	writeTestJSON(t, filepath.Join(moduleDir, "tools", "base-integrity-allow.json"), map[string]any{
		"requiredAdditions": map[string]string{
			"product/plugin.json": "product plugin installation",
		},
	})

	report, err := VerifyBase(context.Background(), workspace)
	if err == nil || report.Failed() != 1 {
		t.Fatalf("missing required addition passed: report=%#v err=%v", report, err)
	}
	if !reflect.DeepEqual(report.Modules[0].MissingRequiredAdditions, []string{"product/plugin.json"}) {
		t.Fatalf("missing required additions = %v", report.Modules[0].MissingRequiredAdditions)
	}
}

func TestVerifyBaseReportsMalformedManifest(t *testing.T) {
	root, workspace := baseFixture(t)
	writeTestFile(t, filepath.Join(root, "modules", "app", "tools", "base-manifest.json"), "{")
	report, err := VerifyBase(context.Background(), workspace)
	if err == nil || report.Failed() != 1 || report.Modules[0].Error == "" {
		t.Fatalf("malformed manifest result: report=%#v err=%v", report, err)
	}
}

func TestVerifyBaseRejectsMalformedIntegrityPolicy(t *testing.T) {
	root, workspace := baseFixture(t)
	moduleDir := filepath.Join(root, "modules", "app")
	writeTestJSON(t, filepath.Join(moduleDir, "tools", "base-manifest.json"), baseManifest{Files: map[string]string{}})
	writeTestFile(t, filepath.Join(moduleDir, "tools", "base-integrity-allow.json"), `{"requiredAdditions":[]}`)

	report, err := VerifyBase(context.Background(), workspace)
	if err == nil || report.Failed() != 1 || !strings.Contains(report.Modules[0].Error, "requiredAdditions") {
		t.Fatalf("malformed policy passed: report=%#v err=%v", report, err)
	}
}

func baseFixture(t *testing.T) (string, *resources.Workspace) {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "workspace.codefly.yaml"), `name: integrity-test
layout: modules
modules:
  - name: app
`)
	writeTestFile(t, filepath.Join(root, "modules", "app", "module.codefly.yaml"), `kind: module
name: app
services:
  - name: kept
`)
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return root, workspace
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, string(payload))
}
