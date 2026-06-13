package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestGoModRequires(t *testing.T) {
	dir := t.TempDir()
	goMod := `module example.com/foo

go 1.21

require (
	github.com/codefly-dev/core/wool v0.1.0
	github.com/codefly-dev/core v0.1.0
)
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	if !goModRequires(dir, "github.com/codefly-dev/core/wool") {
		t.Error("goModRequires(wool) = false, want true")
	}
	if !goModRequires(dir, "github.com/codefly-dev/core") {
		t.Error("goModRequires(core) = false, want true")
	}
	if goModRequires(dir, "github.com/other/module") {
		t.Error("goModRequires(other) = true, want false")
	}
	if goModRequires(dir, "nonexistent") {
		t.Error("goModRequires(nonexistent) = true, want false")
	}
}

func TestGoModRequires_NoGoMod(t *testing.T) {
	dir := t.TempDir()
	// No go.mod file
	if goModRequires(dir, "github.com/codefly-dev/core/wool") {
		t.Error("goModRequires with no go.mod = true, want false")
	}
}

func TestIsDir(t *testing.T) {
	dir := t.TempDir()
	if !isDir(dir) {
		t.Errorf("isDir(%q) = false, want true", dir)
	}

	filePath := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if isDir(filePath) {
		t.Errorf("isDir(%q) = true, want false (file)", filePath)
	}

	nonexistent := filepath.Join(dir, "nonexistent")
	if isDir(nonexistent) {
		t.Errorf("isDir(%q) = true, want false (nonexistent)", nonexistent)
	}
}

func TestBuildAgent_MissingYAML(t *testing.T) {
	dir := t.TempDir()
	err := buildAgent(dir, buildOptions{skipAudit: true})
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
	err := buildAgent(dir, buildOptions{skipAudit: true})
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
