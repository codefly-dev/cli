package executionruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

func TestDefaultStateDirIsStableAndWorkspaceIsolated(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	firstWorkspace := t.TempDir()
	secondWorkspace := t.TempDir()

	first, err := DefaultStateDir(firstWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DefaultStateDir(filepath.Join(firstWorkspace, "."))
	if err != nil {
		t.Fatal(err)
	}
	second, err := DefaultStateDir(secondWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("same workspace produced different state directories: %q != %q", first, again)
	}
	if first == second {
		t.Fatalf("different workspaces shared state directory %q", first)
	}
}

func TestOpenWithoutExportersCreatesPrivateProductNeutralRuntime(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "execution")
	runtime, err := Open(context.Background(), Config{
		WorkDir:         t.TempDir(),
		StateDir:        stateDir,
		AuthorityJWKS:   "https://accounts.example.test/.well-known/work-context-jwks.json",
		AuthorityIssuer: "https://accounts.example.test",
		Release:         "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Recorder == nil || runtime.Dispatcher == nil {
		t.Fatal("runtime did not assemble recorder and no-op dispatcher")
	}
	if runtime.Identity.SignerID == "" || runtime.Identity.KeyID == "" || len(runtime.Identity.PublicKey) == 0 {
		t.Fatal("runtime did not expose public attestor enrollment identity")
	}
	for _, name := range []string{attestorFileName, journalFileName} {
		info, err := os.Stat(filepath.Join(stateDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s permissions are too broad: %o", name, info.Mode().Perm())
		}
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("second close must be idempotent: %v", err)
	}
}

func TestOpenRejectsIncompleteAuthorityBeforeState(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "must-not-exist")
	_, err := Open(context.Background(), Config{
		StateDir: stateDir,
		Release:  "test",
	})
	if err == nil {
		t.Fatal("expected invalid authority configuration")
	}
	if _, statErr := os.Stat(stateDir); !os.IsNotExist(statErr) {
		t.Fatalf("invalid configuration created durable state: %v", statErr)
	}
}

func TestAdvertisesExecutionExporter(t *testing.T) {
	if advertisesExecutionExporter(nil) {
		t.Fatal("nil advertisement accepted")
	}
	if advertisesExecutionExporter(&agentv0.AgentInformation{}) {
		t.Fatal("empty advertisement accepted")
	}
	if !advertisesExecutionExporter(&agentv0.AgentInformation{
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_EXECUTION_EXPORTER},
		},
	}) {
		t.Fatal("execution exporter capability not detected")
	}
}
