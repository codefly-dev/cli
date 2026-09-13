package show

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

const runnableWithStoreDependency = `kind: runnable
name: word-count
version: 0.1.0
description: Count words
agent:
  kind: codefly:runnable
  name: python
  version: 0.0.1
  publisher: codefly.dev
contract:
  protocol: codefly.runnable/v1
  input:
    fields:
      - name: text
        type: string
      - name: options
        type: object
        optional: true
        fields:
          - name: stop_words
            type: array
            nullable: true
            items:
              type: string
  output:
    fields:
      - name: count
        type: integer
entrypoint:
  handler: handler.py
  inputs: [pyproject.toml]
execution:
  facilities: [native, kubernetes]
  timeout: 2m
  cancellation: signal
  recovery: recompute
  payload:
    max-input-bytes: 65536
service-dependencies:
  - name: store
    kind: runtime
    endpoints:
      - name: tcp
workspace-configuration-dependencies: [openai]
`

const storeServiceWithTCP = `kind: service
name: store
version: 0.0.1
agent:
  kind: codefly:service
  name: postgres
  version: 0.0.1
  publisher: codefly.dev
endpoints:
  - name: tcp
    api: tcp
`

func writeShowRunnableWorkspace(t *testing.T, serviceDeclaration string) string {
	t.Helper()
	dir := t.TempDir()
	workspace := "name: test-ws\nlayout: flat\nrunnables:\n  - name: word-count\n"
	if serviceDeclaration != "" {
		workspace += "services:\n  - name: store\n"
		serviceDir := filepath.Join(dir, "services", "store")
		require.NoError(t, os.MkdirAll(serviceDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(serviceDir, "service.codefly.yaml"), []byte(serviceDeclaration), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte(workspace), 0o644))

	runnableDir := filepath.Join(dir, "runnables", "word-count")
	require.NoError(t, os.MkdirAll(runnableDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(runnableDir, "runnable.codefly.yaml"), []byte(runnableWithStoreDependency), 0o644))
	return dir
}

func newShowTestCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	return cmd, buf
}

func TestRunnableCommandRequiresExactlyOneName(t *testing.T) {
	if err := RunnableCmd.Args(RunnableCmd, nil); err == nil {
		t.Fatal("show runnable accepted no name")
	}
	if err := RunnableCmd.Args(RunnableCmd, []string{"one", "two"}); err == nil {
		t.Fatal("show runnable accepted two names")
	}
}

func TestShowRunnableRendersContractAndExecution(t *testing.T) {
	t.Chdir(writeShowRunnableWorkspace(t, storeServiceWithTCP))
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "word-count"))

	out := buf.String()
	require.Contains(t, out, "Runnable:   word-count @ 0.1.0")
	require.Contains(t, out, "Workspace:  test-ws")
	require.Contains(t, out, "Module:     test-ws")
	require.Contains(t, out, "codefly.dev/python:0.0.1")
	require.Contains(t, out, "codefly.runnable/v1")
	require.Contains(t, out, "options: object (optional)")
	require.Contains(t, out, "stop_words: array (nullable)")
	require.Contains(t, out, "items: string")
	require.Contains(t, out, "native, kubernetes")
	require.Contains(t, out, "65536 bytes in / 1048576 bytes out")
	require.Contains(t, out, "test-ws/store [runtime] tcp — resolved")
	require.Contains(t, out, "Configurations: openai")
}

func TestShowRunnableJSONReportsResolvedDependency(t *testing.T) {
	t.Chdir(writeShowRunnableWorkspace(t, storeServiceWithTCP))
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })
	showRunnableJSON = true

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "word-count"))

	var report runnableReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &report))
	require.Equal(t, "word-count", report.Name)
	require.Equal(t, "0.1.0", report.Version)
	require.Equal(t, uint64(65536), report.Execution.MaxInputBytes)
	require.Equal(t, uint64(1<<20), report.Execution.MaxOutputBytes)
	require.Len(t, report.Dependencies, 1)
	require.True(t, report.Dependencies[0].Resolved)
	require.Empty(t, report.Dependencies[0].Problem)
	require.Len(t, report.Output, 1)
	require.Equal(t, "integer", report.Output[0].Type)
}

// TestShowRunnableReportsMissingService keeps an unresolved dependency a
// reported fact rather than an error: the declaration itself is valid, and a
// caller needs to see which of several dependencies is the missing one.
func TestShowRunnableReportsMissingService(t *testing.T) {
	t.Chdir(writeShowRunnableWorkspace(t, ""))
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })
	showRunnableJSON = true

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "word-count"))

	var report runnableReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &report))
	require.Len(t, report.Dependencies, 1)
	require.False(t, report.Dependencies[0].Resolved)
	require.Contains(t, report.Dependencies[0].Problem, "cannot load service test-ws/store")
}

// TestShowRunnableDistinguishesAnUnloadableServiceFromAnAbsentOne covers the
// case that made this report lie: the service file is present, so "not found"
// sends the reader hunting for a resource that is sitting right there.
func TestShowRunnableDistinguishesAnUnloadableServiceFromAnAbsentOne(t *testing.T) {
	dir := writeShowRunnableWorkspace(t, storeServiceWithTCP)
	declaration := filepath.Join(dir, "services", "store", "service.codefly.yaml")
	require.NoError(t, os.WriteFile(declaration, []byte("kind: service\nname: store\nendpoints:\n  - name: tcp\n\tapi: tcp\n"), 0o644))
	t.Chdir(dir)
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })
	showRunnableJSON = true

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "word-count"))

	var report runnableReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &report))
	require.False(t, report.Dependencies[0].Resolved)
	require.Contains(t, report.Dependencies[0].Problem, "cannot load service test-ws/store")
	require.NotContains(t, report.Dependencies[0].Problem, "not found in workspace",
		"a malformed declaration must not be reported as an absent service")
}

// TestShowRunnableRejectsARuntimeDependencyOnAnEndpointlessService mirrors
// core's binding rule (runnable/package.go: a runtime edge needs at least one
// mapping, and an empty selection resolves to every exported endpoint). A
// service exporting none can never satisfy one, so reporting it resolved
// promises a binding VerifyBinding refuses.
func TestShowRunnableRejectsARuntimeDependencyOnAnEndpointlessService(t *testing.T) {
	endpointlessStore := `kind: service
name: store
version: 0.0.1
endpoints: []
`
	dir := writeShowRunnableWorkspace(t, endpointlessStore)
	declaration := filepath.Join(dir, "runnables", "word-count", "runnable.codefly.yaml")
	content, err := os.ReadFile(declaration)
	require.NoError(t, err)
	withoutSelection := strings.Replace(string(content), "    endpoints:\n      - name: tcp\n", "", 1)
	require.NotEqual(t, string(content), withoutSelection, "fixture must drop the explicit endpoint selection")
	require.NoError(t, os.WriteFile(declaration, []byte(withoutSelection), 0o644))
	t.Chdir(dir)
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })
	showRunnableJSON = true

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "word-count"))

	var report runnableReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &report))
	require.Len(t, report.Dependencies, 1)
	require.Empty(t, report.Dependencies[0].Endpoints, "the selection is empty in this fixture")
	require.False(t, report.Dependencies[0].Resolved)
	require.Contains(t, report.Dependencies[0].Problem, "exports no endpoint")
}

// TestShowRunnableResolvesARuntimeDependencyWithAnEmptySelection is the other
// half of the rule: an empty selection is satisfied as soon as the service
// exports anything at all.
func TestShowRunnableResolvesARuntimeDependencyWithAnEmptySelection(t *testing.T) {
	dir := writeShowRunnableWorkspace(t, storeServiceWithTCP)
	declaration := filepath.Join(dir, "runnables", "word-count", "runnable.codefly.yaml")
	content, err := os.ReadFile(declaration)
	require.NoError(t, err)
	withoutSelection := strings.Replace(string(content), "    endpoints:\n      - name: tcp\n", "", 1)
	require.NoError(t, os.WriteFile(declaration, []byte(withoutSelection), 0o644))
	t.Chdir(dir)
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })
	showRunnableJSON = true

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "word-count"))

	var report runnableReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &report))
	require.True(t, report.Dependencies[0].Resolved)
	require.Empty(t, report.Dependencies[0].Problem)
}

// TestShowRunnableRendersNestedArrayElementTypes locks the human rendering of
// array<array<T>>: printing only the immediate element type silently dropped
// the innermost type, so the text and --json described different contracts.
func TestShowRunnableRendersNestedArrayElementTypes(t *testing.T) {
	nested := `kind: runnable
name: matrix
version: 0.1.0
agent:
  kind: codefly:runnable
  name: python
  version: 0.0.1
  publisher: codefly.dev
contract:
  protocol: codefly.runnable/v1
  input:
    fields:
      - name: grid
        type: array
        items:
          type: array
          items:
            type: integer
      - name: rows
        type: array
        items:
          type: object
          fields:
            - name: label
              type: string
  output:
    fields:
      - name: total
        type: integer
entrypoint:
  handler: handler.py
execution:
  facilities: [native]
  timeout: 2m
  cancellation: signal
  recovery: recompute
`
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"),
		[]byte("name: test-ws\nlayout: flat\nrunnables:\n  - name: matrix\n"), 0o644))
	runnableDir := filepath.Join(dir, "runnables", "matrix")
	require.NoError(t, os.MkdirAll(runnableDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(runnableDir, "runnable.codefly.yaml"), []byte(nested), 0o644))
	t.Chdir(dir)
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "matrix"))

	out := buf.String()
	require.Contains(t, out, "    grid: array\n      items: array\n        items: integer\n",
		"the innermost array element type must survive:\n%s", out)
	require.Contains(t, out, "    rows: array\n      items: object\n        label: string\n",
		"an object element's fields must still render:\n%s", out)
}

func TestShowRunnableReportsMissingEndpoint(t *testing.T) {
	serviceWithoutTCP := `kind: service
name: store
version: 0.0.1
agent:
  kind: codefly:service
  name: postgres
  version: 0.0.1
  publisher: codefly.dev
endpoints:
  - name: http
    api: http
`
	t.Chdir(writeShowRunnableWorkspace(t, serviceWithoutTCP))
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })
	showRunnableJSON = true

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "word-count"))

	var report runnableReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &report))
	require.False(t, report.Dependencies[0].Resolved)
	require.Contains(t, report.Dependencies[0].Problem, "declares no endpoint tcp")
}

func TestShowRunnableUnknownNameReturnsError(t *testing.T) {
	t.Chdir(writeShowRunnableWorkspace(t, storeServiceWithTCP))
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })

	cmd, _ := newShowTestCmd()
	require.Error(t, showRunnable(cmd, "absent"))
}

func TestShowRunnableMissingWorkspaceReturnsError(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })

	cmd, _ := newShowTestCmd()
	require.Error(t, showRunnable(cmd, "word-count"))
}

// TestShowRunnableNamesAnApiOnlySelector covers the selector that projected to
// nothing: "- api: grpc" names no endpoint, so reporting it by Name alone
// printed "declares no endpoint " and put "" in the JSON, telling the reader
// neither which selector failed nor why.
//
// The verdict itself is correct and must stay correct: a runnable's wire form
// flattens every selector to endpoint.Name (core resources/runnable.go), so an
// api-only selector flattens to "" and binds nothing.
func TestShowRunnableNamesAnApiOnlySelector(t *testing.T) {
	dir := writeShowRunnableWorkspace(t, storeServiceWithTCP)
	declaration := filepath.Join(dir, "runnables", "word-count", "runnable.codefly.yaml")
	content, err := os.ReadFile(declaration)
	require.NoError(t, err)
	apiOnly := strings.Replace(string(content), "      - name: tcp\n", "      - api: tcp\n", 1)
	require.NotEqual(t, string(content), apiOnly, "fixture must replace the named selector")
	require.NoError(t, os.WriteFile(declaration, []byte(apiOnly), 0o644))
	t.Chdir(dir)
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })
	showRunnableJSON = true

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "word-count"))

	var report runnableReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &report))
	require.False(t, report.Dependencies[0].Resolved)
	require.Equal(t, []string{"::tcp"}, report.Dependencies[0].Endpoints,
		"an api-only selector must be reported as declared, not as an empty name")
	require.Contains(t, report.Dependencies[0].Problem, "declares no endpoint ::tcp")
	require.NotContains(t, report.Dependencies[0].Problem, "endpoint \n")
}

// TestShowRunnableKeepsALegacyEndpointlessDependencyResolved pins a rule that
// looks like a bug and is not. core's validateBindingMappings requires a
// mapping for a RUNTIME edge unconditionally, but for a legacy or external
// edge only when it explicitly names endpoints; ValidateDependencyPrerequisite
// spells out why ("Endpointless producers under an undeclared kind are how
// existing workspaces express one-shot work, and failing them would reject
// configurations that work today").
//
// Widening the endpointless check to every edge that participates in the run
// stage would therefore report working workspaces as broken.
func TestShowRunnableKeepsALegacyEndpointlessDependencyResolved(t *testing.T) {
	endpointlessStore := `kind: service
name: store
version: 0.0.1
endpoints: []
`
	dir := writeShowRunnableWorkspace(t, endpointlessStore)
	declaration := filepath.Join(dir, "runnables", "word-count", "runnable.codefly.yaml")
	content, err := os.ReadFile(declaration)
	require.NoError(t, err)
	legacy := strings.Replace(string(content), "    kind: runtime\n", "", 1)
	legacy = strings.Replace(legacy, "    endpoints:\n      - name: tcp\n", "", 1)
	require.NotContains(t, legacy, "kind: runtime", "fixture must drop the declared kind")
	require.NoError(t, os.WriteFile(declaration, []byte(legacy), 0o644))
	t.Chdir(dir)
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })
	showRunnableJSON = true

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "word-count"))

	var report runnableReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &report))
	require.Equal(t, "legacy", report.Dependencies[0].Kind)
	require.True(t, report.Dependencies[0].Resolved,
		"a legacy edge naming no endpoint is exempt from the endpointless rule")
	require.Empty(t, report.Dependencies[0].Problem)
}

// writeSecondRelease adds a second reference to the same runnable name at
// another version, which is what "several versions of one name coexist" means
// on disk and what `codefly list runnables` already lists as two rows.
func writeSecondRelease(t *testing.T, dir, version string) {
	t.Helper()
	workspaceFile := filepath.Join(dir, "workspace.codefly.yaml")
	content, err := os.ReadFile(workspaceFile)
	require.NoError(t, err)
	updated := strings.Replace(string(content),
		"runnables:\n  - name: word-count\n",
		"runnables:\n  - name: word-count\n  - name: word-count\n    path: runnables/word-count-next\n", 1)
	require.NotEqual(t, string(content), updated, "fixture must add the second reference")
	require.NoError(t, os.WriteFile(workspaceFile, []byte(updated), 0o644))

	next := filepath.Join(dir, "runnables", "word-count-next")
	require.NoError(t, os.MkdirAll(next, 0o755))
	first, err := os.ReadFile(filepath.Join(dir, "runnables", "word-count", "runnable.codefly.yaml"))
	require.NoError(t, err)
	bumped := strings.Replace(string(first), "version: 0.1.0\n", "version: "+version+"\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(next, "runnable.codefly.yaml"), []byte(bumped), 0o644))
}

// TestShowRunnableRefusesAnAmbiguousNameInsteadOfPickingOne is the silent wrong
// answer: core's LoadRunnableFromName returns the first matching reference and
// stops, and FindRunnableByName only detects ambiguity across modules, so a
// second release of a name was reported as the first with nothing saying so.
func TestShowRunnableRefusesAnAmbiguousNameInsteadOfPickingOne(t *testing.T) {
	dir := writeShowRunnableWorkspace(t, storeServiceWithTCP)
	writeSecondRelease(t, dir, "0.2.0")
	t.Chdir(dir)
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })

	cmd, _ := newShowTestCmd()
	err := showRunnable(cmd, "word-count")
	require.Error(t, err, "an ambiguous name must not silently resolve to one release")
	require.ErrorContains(t, err, "ambiguous")
	require.ErrorContains(t, err, "0.1.0")
	require.ErrorContains(t, err, "0.2.0")
}

// TestShowRunnableVersionSelectsOneRelease is the other half: the release the
// listing shows must be reachable.
func TestShowRunnableVersionSelectsOneRelease(t *testing.T) {
	dir := writeShowRunnableWorkspace(t, storeServiceWithTCP)
	writeSecondRelease(t, dir, "0.2.0")
	t.Chdir(dir)
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })

	for _, version := range []string{"0.1.0", "0.2.0"} {
		showRunnableVersion = version
		cmd, buf := newShowTestCmd()
		require.NoError(t, showRunnable(cmd, "word-count"))
		require.Contains(t, buf.String(), "Runnable:   word-count @ "+version)
	}

	showRunnableVersion = "9.9.9"
	cmd, _ := newShowTestCmd()
	err := showRunnable(cmd, "word-count")
	require.Error(t, err, "an undeclared version must not fall back to another release")
	require.ErrorContains(t, err, "0.1.0")
}

// TestShowRunnableStillFailsOnlyOnTheNamedRunnable guards the property the
// ambiguity fix could have cost: resolving one name must not load, and so must
// not fail on, an unrelated broken declaration elsewhere in the workspace.
func TestShowRunnableStillFailsOnlyOnTheNamedRunnable(t *testing.T) {
	dir := writeShowRunnableWorkspace(t, storeServiceWithTCP)
	workspaceFile := filepath.Join(dir, "workspace.codefly.yaml")
	content, err := os.ReadFile(workspaceFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(workspaceFile, []byte(strings.Replace(string(content),
		"runnables:\n  - name: word-count\n",
		"runnables:\n  - name: word-count\n  - name: broken\n", 1)), 0o644))
	brokenDir := filepath.Join(dir, "runnables", "broken")
	require.NoError(t, os.MkdirAll(brokenDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(brokenDir, "runnable.codefly.yaml"),
		[]byte("kind: runnable\nname: broken\noptionnal: true\n"), 0o644))
	t.Chdir(dir)
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "word-count"),
		"a broken sibling declaration must not fail an unrelated lookup")
	require.Contains(t, buf.String(), "Runnable:   word-count @ 0.1.0")
}
