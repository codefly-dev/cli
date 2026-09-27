package generate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/codefly-dev/core/standards"
)

func resetRunnableBindingsFlags(t *testing.T) {
	t.Helper()
	runnableBindingsEnv, runnableBindingsCheck = "local", false
	t.Cleanup(func() { runnableBindingsEnv, runnableBindingsCheck = "local", false })
}

// Every derived operation is prepared as the tiny binding a caller installs:
// the owner's own spelling of the operation, the Connect route and the address
// this environment resolves, the bounded contract with its digest, and the
// declared policy. --check holds the committed file to all of it.
func TestGenerateRunnableBindingsBindsEveryDerivedOperation(t *testing.T) {
	ctx := context.Background()
	second := markedMethod("ApplyTextAgain", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", conformingOperation())
	root, _ := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation(), second)), "0.1.0", connectEndpoint())
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env")
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "#") {
		t.Fatalf("want a header and two bindings, and no third value:\n%s", raw)
	}
	declared := conformingOperation()
	connectAddress := ""
	for _, line := range lines[1:] {
		key, value, _ := strings.Cut(line, "=")
		if !strings.HasPrefix(key, "DOCUMENTS__") {
			t.Fatalf("binding key %q is not scoped by its module", key)
		}
		// A value is read back through the rules a caller applies, so the test
		// asserts on what installs rather than on what was marshalled.
		prepared, err := corerunnable.DecodePrepared([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if got := len(value); got > 2048 {
			t.Fatalf("a prepared binding carries no descriptors: %d bytes", got)
		}
		operation := prepared.GetOperation()
		if operation.GetModule() != "documents" || operation.GetService() != "runtime-worker" || operation.GetEndpoint() != "grpc" {
			t.Fatalf("operation coordinates: %v", operation)
		}
		if !strings.HasPrefix(operation.GetSpelling(), "/documents.ingest.v1.IngestionService/ApplyText") {
			t.Fatalf("the operation is not the owner's own spelling: %q", operation.GetSpelling())
		}
		// A gRPC owner is called on its Connect endpoint, at the base URL a local
		// run resolves for it — allocated, never a port spelled here.
		call := prepared.GetCall()
		if call.GetConnect().GetProcedure() != operation.GetSpelling() {
			t.Fatalf("call route %v is not the operation %q", call.GetRoute(), operation.GetSpelling())
		}
		if !strings.HasPrefix(call.GetAddress(), "http://localhost:") {
			t.Fatalf("call address %q is not a resolved HTTP base URL", call.GetAddress())
		}
		if connectAddress == "" {
			connectAddress = call.GetAddress()
		} else if call.GetAddress() != connectAddress {
			t.Fatalf("two operations of one endpoint resolve different addresses: %q and %q", connectAddress, call.GetAddress())
		}
		// The contract is the bounded one, and the digest covers it.
		if len(prepared.GetContract().GetInput().GetFields()) == 0 || len(prepared.GetContract().GetOutput().GetFields()) == 0 {
			t.Fatalf("bounded contract: %v", prepared.GetContract())
		}
		digest, err := corerunnable.ContractDigest(prepared.GetContract())
		if err != nil || digest != prepared.GetContractDigest() {
			t.Fatalf("contract digest %q does not cover the contract (%v)", prepared.GetContractDigest(), err)
		}
		// The policy is the one the method declared, not a re-spelling of it.
		policy := prepared.GetPolicy()
		if policy.GetAudience() != declared.GetAudience() || policy.GetMaxAttempts() != declared.GetMaxAttempts() {
			t.Fatalf("policy: %v", policy)
		}
		if policy.GetAttemptTimeout().AsDuration() != declared.GetAttemptTimeout().AsDuration() ||
			policy.GetTotalTimeout().AsDuration() != declared.GetTotalTimeout().AsDuration() ||
			policy.GetBackoff().AsDuration() != declared.GetBackoff().AsDuration() {
			t.Fatalf("policy durations: %v", policy)
		}
		if policy.GetMaxInputBytes() != declared.GetMaxInputBytes() || policy.GetMaxOutputBytes() != declared.GetMaxOutputBytes() {
			t.Fatalf("the declared payload bounds are not installed: %v", policy)
		}
		if len(policy.GetInvokeScopes()) != len(declared.GetInvokeScopes()) || len(policy.GetLookupScopes()) != len(declared.GetLookupScopes()) {
			t.Fatalf("policy authority: %v", policy)
		}
	}

	// The committed file is current; a stale one is refused by --check.
	runnableBindingsCheck = true
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append(raw, []byte("STALE=1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err == nil {
		t.Fatal("--check accepted a stale file")
	}
}

// A Runnable owner is called with JSON, so a gRPC owner that serves no Connect
// endpoint cannot be called at all. It is refused here, naming the service,
// rather than at a call against an address that answers nothing.
func TestGenerateRunnableBindingsRefusesAGRPCOwnerWithNoConnectEndpoint(t *testing.T) {
	ctx := context.Background()
	root, _ := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
	if err == nil || !strings.Contains(err.Error(), standards.CONNECT) {
		t.Fatalf("an owner with no connect endpoint was not refused: %v", err)
	}
}

// A contract whose bytes are not the ones the catalog recorded is refused: the
// bounded contract a prepared value states the digest of was derived from
// something nobody published.
func TestGenerateRunnableBindingsRefusesAStaleContractCatalog(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0", connectEndpoint())
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	catalog, err := composition.LoadAPIContractCatalog(moduleDir)
	if err != nil {
		t.Fatal(err)
	}
	contract := filepath.Join(moduleDir, filepath.FromSlash(catalog.Endpoints[0].Path))
	changed := descriptorSet(t, ingestionFile(conformingOperation(), markedMethod("Extra", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", nil)))
	if err := os.WriteFile(contract, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	err = RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "codefly generate contracts") {
		t.Fatalf("a stale catalog was not refused: %v", err)
	}
}

func connectEndpoint() *resources.Endpoint {
	return &resources.Endpoint{Name: "connect", API: standards.CONNECT, Visibility: resources.VisibilityPublic}
}

func TestRunnableBindingKeyIsAnEnvironmentKey(t *testing.T) {
	if got := RunnableBindingKey("documents", "documents.ingest-document"); got != "DOCUMENTS__DOCUMENTS_INGEST_DOCUMENT" {
		t.Fatalf("key %q", got)
	}
}
