package executionruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/workcontext"
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
	runtime, err := Open(context.Background(), &Config{
		WorkDir:         t.TempDir(),
		StateDir:        stateDir,
		AuthorityIssuer: "https://accounts.example.test",
		Revisions:       workcontext.FixedRevision(1),
		Seals:           workcontext.NewMemorySealSource(),
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
	// An authority with no issuer, and one with an issuer but none of the
	// live sources core's authenticator verifies against: both refuse before
	// any durable state exists, because a gateway that cannot verify is not
	// a gateway with a weaker verifier.
	for name, config := range map[string]Config{
		"no issuer": {Release: "test"},
		"no live sources": {
			AuthorityJWKS:   "https://accounts.example.test/.well-known/work-context-jwks.json",
			AuthorityIssuer: "https://accounts.example.test",
			Release:         "test",
		},
		"no seal source": {
			AuthorityJWKS:   "https://accounts.example.test/.well-known/work-context-jwks.json",
			AuthorityIssuer: "https://accounts.example.test",
			Revisions:       workcontext.FixedRevision(1),
			Release:         "test",
		},
	} {
		stateDir := filepath.Join(t.TempDir(), "must-not-exist")
		config.StateDir = stateDir
		_, err := Open(context.Background(), &config)
		if err == nil {
			t.Fatalf("%s: expected invalid authority configuration", name)
		}
		if _, statErr := os.Stat(stateDir); !os.IsNotExist(statErr) {
			t.Fatalf("%s: invalid configuration created durable state: %v", name, statErr)
		}
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
