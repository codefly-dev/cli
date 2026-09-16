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
	"github.com/codefly-dev/core/wool"
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
	captureStdout(t)
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
	// Host construction reaps stale process groups with context.Background()
	// (pkg/engine/host.go), so those logs carry no provider and resolve to
	// wool's fallback, which prints to stdout. They are not the plane's
	// narration and the plane cannot reach them; a process that owns stdout
	// redirects the fallback itself, which is what pkg/mcp's Serve does. The
	// same guard is installed here so this asserts about the plane.
	wool.SetFallbackLogger(discardNarration{})
	capture := &stdoutCapture{}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(capture, reader)
	}()
	t.Cleanup(func() {
		// os.Stdout goes back FIRST. Clearing the fallback first put the
		// default Console — which prints to os.Stdout — back in play while the
		// pipe was still installed, so any straggling orphan log landed in the
		// capture and failed the test with narration it does not assert about.
		os.Stdout = original
		// wool exposes no way to read the previous fallback, and nothing in
		// this package installs one, so nil is the restore.
		wool.SetFallbackLogger(nil)
		_ = writer.Close()
		<-drained
		_ = reader.Close()
		if written := capture.written(); written != "" {
			t.Errorf("the run wrote %q to stdout; the plane must not narrate", written)
		}
	})
	return capture
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
	if live := activeRunCount(impl); live != 0 {
		t.Errorf("Stop returned with %d run goroutine(s) still live", live)
	}
}

// activeRunCount reports how many run goroutines have not returned yet. Test
// scaffolding rather than a method on the plane: nothing in production asks,
// and the tests are in-package, so they can read the tracking directly.
func activeRunCount(p *planeImpl) int {
	p.runsMu.Lock()
	defer p.runsMu.Unlock()
	live := 0
	for _, runs := range p.runs {
		live += len(runs)
	}
	return live
}

// trackFakeRun tracks a run whose goroutine returns once its context is
// cancelled — the same and only release condition Run's real goroutine has.
// Standing in for the flow lets these tests pin the tracking itself, including
// states a real flow reaches only by crashing partway through a teardown.
func trackFakeRun(p *planeImpl, flowID string) *activeRun {
	runCtx, runCancel := context.WithCancel(context.Background())
	run := p.trackRun(flowID, runCancel)
	go func() {
		<-runCtx.Done()
		p.finishRun(flowID, run)
	}()
	return run
}

// joined reports whether a run's goroutine has returned.
func joined(run *activeRun) bool {
	select {
	case <-run.done:
		return true
	default:
		return false
	}
}

// Stop with no FlowID must still join. A flow that exits on its own releases
// the registry entry from inside Run's goroutine, so Active() reports nothing
// while that goroutine is still tearing down and still narrating. Returning
// "nothing running" there told the caller the stream was quiet while the run
// was writing to it — on the MCP path, straight onto the JSON-RPC stream, which
// is the shape stop_flow takes whenever the caller omits flow_id.
func TestStopWithoutAFlowIDJoinsRunsTheRegistryHasForgotten(t *testing.T) {
	plane, err := NewAt(writeSolutionInputsWorkspace(t))
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	impl, ok := plane.(*planeImpl)
	if !ok {
		t.Fatalf("NewAt returned %T, not *planeImpl", plane)
	}

	// Nothing is registered — exactly what Stop sees once a flow has exited by
	// itself — while the run it started is still unwinding.
	run := trackFakeRun(impl, "wiki/backend")
	if id, _ := impl.host.Flows().Active(); id != "" {
		t.Fatalf("no flow should be registered, got active id %q", id)
	}

	stopped, err := plane.Stop(context.Background(), StopRequest{})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped {
		t.Error("Stop reported it stopped a flow, but none was registered")
	}
	if !joined(run) {
		t.Error("Stop returned without joining the unwinding run; it is still free to write to stdout")
	}
	if live := activeRunCount(impl); live != 0 {
		t.Errorf("Stop returned with %d run goroutine(s) still live", live)
	}
}

// A flow id can hold two runs at once: the registry frees the id when the flow
// exits, so a fresh Run claims it while the previous goroutine is still
// unwinding. Keying one run per id dropped the older handle, and Close then had
// nothing left to join it with — the very leak the tracking exists to close.
func TestCloseJoinsEveryRunSharingAFlowID(t *testing.T) {
	impl := &planeImpl{}

	first := trackFakeRun(impl, "wiki/backend")
	second := trackFakeRun(impl, "wiki/backend")
	if live := activeRunCount(impl); live != 2 {
		t.Fatalf("both runs under one flow id must stay tracked, got %d", live)
	}

	impl.stopAllRuns()

	if !joined(first) {
		t.Error("the superseded run was never joined; a second Run under its flow id dropped the handle")
	}
	if !joined(second) {
		t.Error("the newer run was not joined")
	}
	if live := activeRunCount(impl); live != 0 {
		t.Errorf("stopAllRuns returned with %d run goroutine(s) still live", live)
	}
}

// Every run must be cancelled before any of them is waited on. Cancelling
// inside the waiting loop left each run running until the one before it had
// finished unwinding, so closing a plane with several live flows cost the sum
// of their teardowns. Both runs here return only once the other has been
// cancelled, so a one-at-a-time loop cannot finish either of them.
func TestStopAllRunsCancelsEveryRunBeforeWaiting(t *testing.T) {
	impl := &planeImpl{}

	cancelled := map[string]chan struct{}{
		"first":  make(chan struct{}),
		"second": make(chan struct{}),
	}
	for _, name := range []string{"first", "second"} {
		runCtx, runCancel := context.WithCancel(context.Background())
		run := impl.trackRun(name, runCancel)
		go func() {
			<-runCtx.Done()
			close(cancelled[name])
			// Returns only once the other run has been cancelled too.
			for other, signal := range cancelled {
				if other != name {
					<-signal
				}
			}
			impl.finishRun(name, run)
		}()
	}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		impl.stopAllRuns()
	}()

	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("stopAllRuns cancelled runs one at a time: a run that returns only after every run is cancelled was left waiting")
	}
	if live := activeRunCount(impl); live != 0 {
		t.Errorf("stopAllRuns returned with %d run goroutine(s) still live", live)
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
