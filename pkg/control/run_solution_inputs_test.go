package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	root := writeSolutionInputsWorkspace(t)
	outputEnvironment := filepath.Join(t.TempDir(), "runtime.env")

	plane, err := NewAt(root)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Nothing here starts a process, but composing the excluded root's
	// environment still resolves a runtime context, and the plane leaves it
	// unset unless the request names one.
	if _, err := plane.Run(ctx, RunRequest{
		Service:        "wiki/backend",
		RuntimeContext: resources.RuntimeContextNative,
		ExcludeRoot:    true,
		Wait:           true,
		OutputEnv:      outputEnvironment,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		if _, err := plane.Stop(context.Background(), StopRequest{Destroy: true}); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	body, err := os.ReadFile(outputEnvironment)
	if err != nil {
		t.Fatalf("read excluded-root output environment: %v", err)
	}
	environment := string(body)

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
	outputEnvironment := filepath.Join(t.TempDir(), "runtime.env")

	plane, err := NewAt(root)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Nothing here starts a process, but composing the excluded root's
	// environment still resolves a runtime context, and the plane leaves it
	// unset unless the request names one.
	if _, err := plane.Run(ctx, RunRequest{
		Service:        "wiki/backend",
		RuntimeContext: resources.RuntimeContextNative,
		ExcludeRoot:    true,
		Wait:           true,
		OutputEnv:      outputEnvironment,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		if _, err := plane.Stop(context.Background(), StopRequest{Destroy: true}); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	body, err := os.ReadFile(outputEnvironment)
	if err != nil {
		t.Fatalf("read excluded-root output environment: %v", err)
	}
	environment := string(body)
	for _, key := range []string{manifest.APIConsumesEnvironmentVariable, "CODEFLY__MODULE_REGISTRATION_SECRETS"} {
		if got := outputEnvironmentValue(environment, key); got != "" {
			t.Errorf("a workspace with no solution manifest exported %s=%q", key, got)
		}
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
