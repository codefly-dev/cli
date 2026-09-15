package control

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
)

// A solution root: the workspace's own `path: .` module declares the
// service-entry the whole composition hangs off, and the manifest beside it
// declares the API it consumes from a composed module. The entry service
// declares no dependency, so an --exclude-root run starts nothing and needs no
// agent — the environment the root would have received is composed all the same.
const (
	solutionInputsWorkspaceYAML = `name: wiki
layout: modules
modules:
    - name: wiki
      path: .
    - name: documents
    - name: host
`
	solutionInputsRootModuleYAML = `kind: module
name: wiki
service-entry: backend
services:
    - name: backend
`
	solutionInputsBackendYAML = `kind: service
name: backend
version: 0.0.0
module: wiki
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
`
	solutionInputsDocumentsModuleYAML = `kind: module
name: documents
services:
    - name: api
`
	solutionInputsDocumentsServiceYAML = `kind: service
name: api
version: 0.0.0
module: documents
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
`
	// The registrar: federation credentials are provisioned only when some
	// service declares the group that carries their digests.
	solutionInputsHostModuleYAML = `kind: module
name: host
services:
    - name: accounts
`
	solutionInputsAccountsYAML = `kind: service
name: accounts
version: 0.0.0
module: host
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
workspace-configuration-dependencies:
    - federation
`
	solutionInputsManifestYAML = `schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
agent:
  kind: codefly:solution
  publisher: codefly.dev
  name: wiki
  version: 0.1.0
api:
  exposes:
    - id: gateway
      protocol: http
  consumes:
    - id: documents
      protocol: connect
      module: documents
      service: api
      endpoint: connect
      as: documents
lifecycle:
  create: true
`
)

func writeSolutionInputsWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                              solutionInputsWorkspaceYAML,
		"module.codefly.yaml":                                 solutionInputsRootModuleYAML,
		"services/backend/service.codefly.yaml":               solutionInputsBackendYAML,
		"modules/documents/module.codefly.yaml":               solutionInputsDocumentsModuleYAML,
		"modules/documents/services/api/service.codefly.yaml": solutionInputsDocumentsServiceYAML,
		"modules/host/module.codefly.yaml":                    solutionInputsHostModuleYAML,
		"modules/host/services/accounts/service.codefly.yaml": solutionInputsAccountsYAML,
		manifest.FileName:                                     solutionInputsManifestYAML,
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// startExcludedRootRun starts the fixture with the root excluded, returning the
// output-environment path.
//
// Nothing here starts a process, but composing the excluded root's environment
// still resolves a runtime context, and the plane leaves it unset unless the
// request names one.
func startExcludedRootRun(t *testing.T, root string) string {
	t.Helper()
	// Installed before anything that tears the run down is registered, so LIFO
	// cleanup order stops the flow first and restores os.Stdout last.
	capture := captureStdout(t)
	outputEnvironment := filepath.Join(t.TempDir(), "runtime.env")

	plane, err := NewAt(root)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	if _, runErr := plane.Run(ctx, RunRequest{
		Service:        "wiki/backend",
		RuntimeContext: resources.RuntimeContextNative,
		ExcludeRoot:    true,
		Wait:           true,
		OutputEnv:      outputEnvironment,
	}); runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	// The plane has no terminal, so starting the run must put nothing on the
	// descriptor an MCP server serves JSON-RPC over. Asserted for the window the
	// plane drives; what orchestration logs while unwinding is a separate,
	// pre-existing problem and not what this test pins.
	if written := capture.written(); written != "" {
		t.Errorf("the run wrote %q to stdout; the plane must not narrate", written)
	}
	t.Cleanup(func() {
		if _, err := plane.Stop(context.Background(), StopRequest{Destroy: true}); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return outputEnvironment
}

// waitForExportedEnvironment waits for the export to actually land.
//
// Readiness cannot be used as the signal: this root is excluded and declares no
// dependency, so the flow has no readiness requirements at all and reports ready
// on the first poll, while the export runs later at the root's RuntimeStart
// barrier. Reading the file the moment Run returns is therefore a race.
func waitForExportedEnvironment(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		body, err := os.ReadFile(path)
		// CODEFLY__SERVICE is written by the same export as everything else, so
		// its presence means the file is complete rather than half-written.
		if err == nil && strings.Contains(string(body), "CODEFLY__SERVICE=backend") {
			return string(body)
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("excluded-root environment was never written: %v", err)
			}
			t.Fatalf("excluded-root environment never completed; keys=%v", exportedEnvironmentKeys(string(body)))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// stdoutCapture collects everything written to os.Stdout while the redirect is
// installed. The plane may be driven by a process serving JSON-RPC on stdout —
// the MCP server does exactly that with this call — so anything a run writes
// there corrupts the protocol.
type stdoutCapture struct {
	mu        sync.Mutex
	collected bytes.Buffer
}

// Write collects a chunk read off the pipe. The reader goroutine and the test
// goroutine both touch the buffer, so it is guarded.
func (c *stdoutCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.collected.Write(p)
}

// written is everything that has reached the pipe so far.
func (c *stdoutCapture) written() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.collected.String()
}

// captureStdout redirects os.Stdout for the remainder of the test.
//
// The redirect is undone in a cleanup rather than inline, because the run
// outlives the call that starts it: plane.Run returns at readiness, not at
// termination, and the flow goes on logging until its goroutine is joined. A
// caller registers the run's teardown after this, so LIFO ordering runs that
// first and os.Stdout is written back only once the run is gone. Restoring it
// inline was an unsynchronized write racing those fmt.Print reads — the data
// race that reddened the race job.
func captureStdout(t *testing.T) *stdoutCapture {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	capture := &stdoutCapture{}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(capture, reader)
	}()
	t.Cleanup(func() {
		os.Stdout = original
		_ = writer.Close()
		<-drained
		_ = reader.Close()
	})
	return capture
}

// The redirect must outlive every cleanup registered after it. Restoring it
// inside the capturing call landed in the middle of a live run — plane.Run
// returns at readiness while the flow logs on until Stop — so the write to
// os.Stdout raced those reads and the detector failed the whole package.
func TestStdoutRedirectOutlivesLaterCleanups(t *testing.T) {
	t.Run("still redirected while a later cleanup runs", func(t *testing.T) {
		beforeRedirect := os.Stdout
		captureStdout(t)
		redirected := os.Stdout
		if redirected == beforeRedirect {
			t.Fatal("os.Stdout was not redirected")
		}
		// Registered after the redirect, so LIFO runs this first — the slot
		// where a run's Stop executes, with its goroutines still logging.
		t.Cleanup(func() {
			if os.Stdout != redirected {
				t.Error("os.Stdout was restored before a later cleanup ran; a flow still logging there would race the restore")
			}
		})
	})
}

// Stop must leave nothing from the run still running. Run's goroutine returns
// only once its context is done, which tearing the flow down does not do, so
// before it was joined it kept narrating after the caller had been told the
// flow had stopped — onto the JSON-RPC stream, for a plane serving one — and
// raced anything that touched os.Stdout afterwards.
func TestStopJoinsTheRunGoroutine(t *testing.T) {
	captureStdout(t)
	plane, err := NewAt(writeSolutionInputsWorkspace(t))
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	impl, ok := plane.(*planeImpl)
	if !ok {
		t.Fatalf("NewAt returned %T, not *planeImpl", plane)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	if _, runErr := plane.Run(ctx, RunRequest{
		Service:        "wiki/backend",
		RuntimeContext: resources.RuntimeContextNative,
		ExcludeRoot:    true,
		Wait:           true,
		OutputEnv:      filepath.Join(t.TempDir(), "runtime.env"),
	}); runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}

	if _, err := plane.Stop(context.Background(), StopRequest{Destroy: true}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if live := impl.activeRunCount(); live != 0 {
		t.Errorf("Stop returned with %d run goroutine(s) still live", live)
	}
}

// A test process standing in for a solution backend runs the composition with
// the root excluded and takes its environment from the export. The federation
// inputs are derived for the root, so excluding it is exactly the case where
// they have no running process to ride — and the hand-off must carry them, or
// the composition boots with every consumed route unrouted.
//
// The control plane derives them the same way the run command does: it drives
// the same lifecycle behind the MCP run tool, and an environment that depended
// on which entry point composed it would be two contracts, not one.
func TestRunExportsSolutionDerivedInputsForAnExcludedRoot(t *testing.T) {
	// The plane has no terminal. Narrating the derivation would put those lines
	// on the MCP server's JSON-RPC stream, which startExcludedRootRun fails the
	// test for — across the whole run, teardown included.
	outputEnvironment := startExcludedRootRun(t, writeSolutionInputsWorkspace(t))

	environment := waitForExportedEnvironment(t, outputEnvironment)

	consumes := outputEnvironmentValue(environment, manifest.APIConsumesEnvironmentVariable)
	if consumes == "" {
		t.Fatalf("excluded root received no %s; keys=%v",
			manifest.APIConsumesEnvironmentVariable, exportedEnvironmentKeys(environment))
	}
	decoded, err := manifest.ParseConsumedAPIs(consumes)
	if err != nil {
		t.Fatalf("exported %s does not decode: %v", manifest.APIConsumesEnvironmentVariable, err)
	}
	if len(decoded) != 1 || decoded[0].Module != "documents" || decoded[0].As != "documents" {
		t.Fatalf("exported consumes projection = %+v, want the documents binding", decoded)
	}

	// The registration secrets are the other half: without them the backend can
	// announce an upstream but never prove which module it is.
	secrets := outputEnvironmentValue(environment, "CODEFLY__MODULE_REGISTRATION_SECRETS")
	if !strings.HasPrefix(secrets, "documents:") {
		t.Fatalf("excluded root received %q for the registration secrets, want a documents entry; keys=%v",
			secrets, exportedEnvironmentKeys(environment))
	}
}

// A workspace with no solution manifest derives nothing, so its excluded-root
// environment must carry neither variable.
func TestRunExportsNoSolutionInputsWithoutAManifest(t *testing.T) {
	root := writeSolutionInputsWorkspace(t)
	if err := os.Remove(filepath.Join(root, manifest.FileName)); err != nil {
		t.Fatal(err)
	}

	outputEnvironment := startExcludedRootRun(t, root)
	environment := waitForExportedEnvironment(t, outputEnvironment)

	for _, key := range []string{manifest.APIConsumesEnvironmentVariable, "CODEFLY__MODULE_REGISTRATION_SECRETS"} {
		if got := outputEnvironmentValue(environment, key); got != "" {
			t.Errorf("a workspace with no solution manifest exported %s=%q", key, got)
		}
	}
}

// Deriving on this path means a manifest the run cannot encode now fails the
// run rather than booting a composition that federates nothing. Two entries
// claiming one facade prefix would hand two modules the same credential, so the
// run must refuse it here exactly as the run command does.
func TestRunRejectsAManifestItCannotFederate(t *testing.T) {
	root := writeSolutionInputsWorkspace(t)
	broken := strings.Replace(solutionInputsManifestYAML, "lifecycle:", `    - id: archives
      protocol: connect
      module: archives
      service: api
      endpoint: connect
      as: documents
lifecycle:`, 1)
	if err := os.WriteFile(filepath.Join(root, manifest.FileName), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	plane, err := NewAt(root)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err = plane.Run(ctx, RunRequest{
		Service:        "wiki/backend",
		RuntimeContext: resources.RuntimeContextNative,
		ExcludeRoot:    true,
		OutputEnv:      filepath.Join(t.TempDir(), "runtime.env"),
	})
	if err == nil {
		_, _ = plane.Stop(context.Background(), StopRequest{Destroy: true})
		t.Fatal("a manifest claiming one facade prefix twice was accepted")
	}
	if !strings.Contains(err.Error(), "documents") {
		t.Errorf("error %q does not name the duplicated prefix", err)
	}
}

// exportedEnvironmentKeys lists what the export did write, so a failure says
// which variables arrived instead of only which one did not.
func exportedEnvironmentKeys(body string) []string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	keys := make([]string, 0, len(lines))
	for _, line := range lines {
		key, _, _ := strings.Cut(line, "=")
		keys = append(keys, key)
	}
	return keys
}

// outputEnvironmentValue reads one key out of the exported KEY=VALUE file.
func outputEnvironmentValue(body string, key string) string {
	for _, line := range strings.Split(body, "\n") {
		name, value, found := strings.Cut(line, "=")
		if found && name == key {
			return value
		}
	}
	return ""
}
