package agents

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"gopkg.in/yaml.v3"
)

func TestFindMonorepoRoot(t *testing.T) {
	root := t.TempDir()

	// Root is detected by core/ being the codefly core module (its go.mod
	// declares module github.com/codefly-dev/core). The old top-level wool/
	// dir is gone, so detection keys off core/go.mod, not bare dirs.
	if err := os.MkdirAll(filepath.Join(root, "core"), 0o755); err != nil {
		t.Fatalf("mkdir core: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "core", "go.mod"),
		[]byte("module github.com/codefly-dev/core\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("write core/go.mod: %v", err)
	}
	myagent := filepath.Join(root, "agents", "services", "myagent")
	if err := os.MkdirAll(myagent, 0o755); err != nil {
		t.Fatalf("mkdir myagent: %v", err)
	}

	got := findMonorepoRoot(myagent)
	if got != root {
		t.Errorf("findMonorepoRoot(%q) = %q, want %q", myagent, got, root)
	}

	// From root itself
	gotFromRoot := findMonorepoRoot(root)
	if gotFromRoot != root {
		t.Errorf("findMonorepoRoot(%q) = %q, want %q", root, gotFromRoot, root)
	}
}

func TestFindMonorepoRoot_NoMonorepo(t *testing.T) {
	dir := t.TempDir()
	// No wool/ or core/ subdirs
	got := findMonorepoRoot(dir)
	if got != "" {
		t.Errorf("findMonorepoRoot(%q) = %q, want \"\"", dir, got)
	}

	// Nested dir with no monorepo above
	nested := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	got = findMonorepoRoot(nested)
	if got != "" {
		t.Errorf("findMonorepoRoot(%q) = %q, want \"\"", nested, got)
	}
}

func TestAgentBuildResultSummary(t *testing.T) {
	r := &agentBuildResult{
		ag:     agentYAML{Name: "go", Version: "0.0.7"},
		native: 8700 * time.Millisecond,
		linux:  3600 * time.Millisecond,
	}
	want := "go:0.0.7 ✓ " + runtime.GOOS + " 8.7s · linux 3.6s"
	if got := r.summary(); got != want {
		t.Errorf("summary() = %q, want %q", got, want)
	}

	r.linuxFailed = true
	if got := r.summary(); !strings.Contains(got, "linux ✗") {
		t.Errorf("summary() with failed linux = %q, want it to contain \"linux ✗\"", got)
	}

	r2 := &agentBuildResult{
		ag:           agentYAML{Name: "go", Version: "0.0.7"},
		native:       8700 * time.Millisecond,
		linuxSkipped: true,
	}
	if got := r2.summary(); !strings.Contains(got, "linux skipped") {
		t.Errorf("summary() with skipped linux = %q, want it to contain \"linux skipped\"", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBuildAgent_MissingYAML(t *testing.T) {
	dir := t.TempDir()
	err := buildAgent(context.Background(), dir, buildOptions{skipAudit: true})
	if err == nil {
		t.Fatal("buildAgent: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "agent.codefly.yaml") {
		t.Errorf("error %q does not contain \"agent.codefly.yaml\"", err.Error())
	}
}

func TestBuildAgent_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	// YAML with missing required fields (publisher, name, version)
	yamlContent := `kind: codefly:service
`
	if err := os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write agent.codefly.yaml: %v", err)
	}
	err := buildAgent(context.Background(), dir, buildOptions{skipAudit: true})
	if err == nil {
		t.Fatal("buildAgent: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "publisher") && !strings.Contains(err.Error(), "name") && !strings.Contains(err.Error(), "version") {
		t.Errorf("error %q should mention missing required fields", err.Error())
	}
}

func TestAgentYAMLParsing(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `publisher: codefly
kind: codefly:service
name: my-agent
version: 1.2.3
`
	yamlPath := filepath.Join(dir, "agent.codefly.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write agent.codefly.yaml: %v", err)
	}

	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}

	var ag agentYAML
	if err := yaml.Unmarshal(data, &ag); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	if ag.Publisher != "codefly" {
		t.Errorf("Publisher = %q, want \"codefly\"", ag.Publisher)
	}
	if ag.Kind != "codefly:service" {
		t.Errorf("Kind = %q, want \"codefly:service\"", ag.Kind)
	}
	if ag.Name != "my-agent" {
		t.Errorf("Name = %q, want \"my-agent\"", ag.Name)
	}
	if ag.Version != "1.2.3" {
		t.Errorf("Version = %q, want \"1.2.3\"", ag.Version)
	}
}

func TestBuildCommandReturnsErrors(t *testing.T) {
	if BuildCmd.RunE == nil || BuildCmd.Run != nil {
		t.Fatal("agent build must return errors through RunE")
	}
	if err := BuildCmd.Args(BuildCmd, []string{"extra"}); err == nil {
		t.Fatal("agent build accepted positional arguments")
	}
}

func TestBuildAllAgentsReturnsMalformedManifestError(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "broken")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(agentDir, "agent.codefly.yaml"), "name: [\n")
	if err := buildAllAgents(context.Background(), root, buildOptions{skipAudit: true}); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("malformed manifest error = %v", err)
	}
}

func TestApplyAgentAuditPolicyGatesOnlyActionableFindings(t *testing.T) {
	agent := agentYAML{Name: "test", Version: "0.0.1"}
	response := &builderv0.AuditResponse{
		Tool: "plugin-scanner",
		Findings: []*builderv0.AuditFinding{
			{Id: "FIXED", Severity: builderv0.AuditFinding_HIGH, FixedVersion: "v1.2.3"},
			{Id: "UNPATCHED", Severity: builderv0.AuditFinding_CRITICAL},
		},
	}
	if err := applyAgentAuditPolicy(t.TempDir(), agent, response, false); err != nil {
		t.Fatalf("informational audit unexpectedly failed: %v", err)
	}
	if err := applyAgentAuditPolicy(t.TempDir(), agent, response, true); err == nil || !strings.Contains(err.Error(), "1 high/critical") {
		t.Fatalf("actionable audit gate error = %v", err)
	}
	response.Findings = response.Findings[1:]
	if err := applyAgentAuditPolicy(t.TempDir(), agent, response, true); err != nil {
		t.Fatalf("unpatched finding should not fail release policy: %v", err)
	}
}
