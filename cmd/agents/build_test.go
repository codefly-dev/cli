package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestMonorepoModulesIncludeSDK(t *testing.T) {
	for _, module := range monorepoModules {
		if module.Module == "github.com/codefly-dev/sdk-go" && module.SubDir == "sdk-go" {
			return
		}
	}
	t.Fatal("monorepoModules does not include the local sdk-go module")
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

func TestCompileAgentNativeOnly(t *testing.T) {
	// A real build (no mocks): a self-contained agent module with no external
	// deps compiles offline. With nativeOnly the host binary is produced and
	// the Linux/amd64 container binary is never written.
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agent.codefly.yaml"),
		"publisher: codefly\nkind: codefly:service\nname: nativeonly\nversion: 0.0.1\n")
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module example.com/nativeonly\n\ngo 1.25\n")
	writeFile(t, filepath.Join(dir, "main.go"),
		"package main\n\nfunc main() {}\n")

	res := compileAgent(context.Background(), dir, &agentLogger{}, &sync.Mutex{}, true)
	if res.err != nil {
		t.Fatalf("compileAgent native-only: %v", res.err)
	}
	if !res.linuxSkipped {
		t.Error("res.linuxSkipped = false, want true")
	}

	nativePath := filepath.Join(home, ".codefly", "agents", "services", "codefly", "nativeonly__0.0.1")
	if _, err := os.Stat(nativePath); err != nil {
		t.Errorf("native binary not produced at %s: %v", nativePath, err)
	}
	containerPath := filepath.Join(home, ".codefly", "containers", "agents", "services", "codefly", "nativeonly__0.0.1")
	if _, err := os.Stat(containerPath); err == nil {
		t.Errorf("container binary should not exist with --native-only, but found %s", containerPath)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestAgentLoggerBufferedAndFlush(t *testing.T) {
	// Buffered mode holds lines and command output instead of streaming them,
	// so parallel builds can flush each agent's block without interleaving.
	log := &agentLogger{}
	log.Info("hello %s", "world")
	log.Header(1, "building %s", "go")
	if err := log.run(exec.Command("printf", "compiler error\n")); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(log.lines) != 3 {
		t.Fatalf("buffered lines = %d, want 3: %v", len(log.lines), log.lines)
	}
	if log.lines[0] != "hello world" {
		t.Errorf("line 0 = %q, want %q", log.lines[0], "hello world")
	}
	if log.lines[2] != "compiler error" {
		t.Errorf("line 2 = %q, want %q (trailing newline trimmed)", log.lines[2], "compiler error")
	}
}

func TestAgentLoggerRunReturnsCommandError(t *testing.T) {
	log := &agentLogger{}
	if err := log.run(exec.Command("false")); err == nil {
		t.Error("run(false): expected error, got nil")
	}
}

func TestCompileAgentSerializesTidy(t *testing.T) {
	// compileAgent must hold tidyMu while it shells out to `go mod tidy`. A
	// missing manifest fails before that point, so use a real manifest whose
	// build will fail later — the tidy step still runs under the lock. We can't
	// observe the lock directly, but a nil tidyMu would panic, which guards the
	// contract that callers always pass one.
	dir := t.TempDir()
	manifest := "publisher: codefly\nkind: codefly:service\nname: x\nversion: 0.0.1\n"
	if err := os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	// No go.mod, so `go mod tidy` fails; we only assert it doesn't panic and
	// surfaces the failure as res.err rather than crashing.
	res := compileAgent(context.Background(), dir, &agentLogger{}, &sync.Mutex{}, false)
	if res.err == nil {
		t.Fatal("compileAgent: expected error (no go.mod), got nil")
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

func TestCompileAgentLinuxFailureFailsAndPreservesArtifact(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := t.TempDir()
	fakeGo := filepath.Join(bin, "go")
	script := `#!/bin/sh
if [ "$1" = "mod" ]; then
  exit 0
fi
out=""
next_is_out=""
for arg in "$@"; do
  if [ -n "$next_is_out" ]; then
    out="$arg"
    next_is_out=""
  elif [ "$arg" = "-o" ]; then
    next_is_out=1
  fi
done
if [ "${GOOS:-}" = "linux" ]; then
  exit 1
fi
printf built > "$out"
`
	if err := os.WriteFile(fakeGo, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agent.codefly.yaml"),
		"publisher: codefly\nkind: codefly:service\nname: atomic\nversion: 0.0.1\n")
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/atomic\n\ngo 1.25\n")

	containerPath := filepath.Join(home, ".codefly", "containers", "agents", "services", "codefly", "atomic__0.0.1")
	if err := os.MkdirAll(filepath.Dir(containerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(containerPath, []byte("previous"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := compileAgent(context.Background(), dir, &agentLogger{}, &sync.Mutex{}, false)
	if res.err == nil || !res.linuxFailed {
		t.Fatalf("Linux failure was reported as success: %+v", res)
	}
	contents, err := os.ReadFile(containerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "previous" {
		t.Fatalf("failed Linux build replaced working artifact: %q", contents)
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

func TestRunAuditUsesManagedGovulncheck(t *testing.T) {
	bin := t.TempDir()
	goPath := filepath.Join(bin, "go")
	if err := os.WriteFile(goPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	agent := agentYAML{Name: "test", Version: "0.0.1"}
	if err := runAudit(context.Background(), t.TempDir(), agent, true); err != nil {
		t.Fatalf("managed audit unexpectedly failed: %v", err)
	}
	if err := runAudit(context.Background(), t.TempDir(), agent, false); err != nil {
		t.Fatalf("informational audit unexpectedly failed: %v", err)
	}
}

func TestRunAuditFailsWhenNoScannerCanRun(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	agent := agentYAML{Name: "test", Version: "0.0.1"}
	if err := runAudit(context.Background(), t.TempDir(), agent, false); err == nil || !strings.Contains(err.Error(), "govulncheck") {
		t.Fatalf("incomplete audit error = %v", err)
	}
}
