package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/engine"
	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"github.com/google/uuid"
)

// newSourceSemanticsGateway binds a gateway to one peer-backed service so the
// parser-derived surfaces can be observed against a scripted agent.
func newSourceSemanticsGateway(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	selected := protocoltest.Install(t, "semantics-peer")[0]
	writeCodeUnitFixture(t, root, "mind.yaml", "service: app\nplugin: "+selected+"\n")
	server, err := NewServer(Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server, root
}

// agentSawOperation reports whether the peer was asked to serve one Code
// operation, named as the peer records it on the wire.
func agentSawOperation(t *testing.T, root, operation string) bool {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".protocol-test", "calls.jsonl")); os.IsNotExist(err) {
		return false
	}
	for _, call := range protocoltest.Calls(t, root) {
		if strings.Contains(string(call.Request), operation) {
			return true
		}
	}
	return false
}

// An analyzer-free agent answers project info with dependencies, packages and
// hashes intact and the inventory declined. The engine owns the parser, so it
// takes that inventory rather than passing the gap on.
func TestGatewayTakesTheInventoryAnAgentDeclined(t *testing.T) {
	server, root := newSourceSemanticsGateway(t)
	writeCodeUnitFixture(t, root, "pyproject.toml", "[project]\nname = \"shop\"\nversion = \"1.0.0\"\n")
	writeCodeUnitFixture(t, root, "checkout.py", "import decimal\n\n\ndef total():\n    return decimal.Decimal(0)\n")

	protocoltest.Response(t, root, "project", &codev0.CodeResponse{Result: &codev0.CodeResponse_GetProjectInfo{
		GetProjectInfo: &codev0.GetProjectInfoResponse{
			Module: "shop", Language: "python", SourceFilesOmitted: true,
			FileHashes: map[string]string{"checkout.py": "unused"},
		},
	}})

	response, err := server.GetProjectInfo(t.Context(), &gatewayv1.GetProjectInfoRequest{Service: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFailure() != nil {
		t.Fatalf("project-info failure = %+v", response.GetFailure())
	}
	files := response.GetSourceFiles()
	if len(files) != 1 || files[0].GetPath() != "checkout.py" {
		t.Fatalf("source files = %+v, want the engine-taken inventory for checkout.py", files)
	}
	if !reflect.DeepEqual(files[0].GetImports(), []string{"decimal"}) {
		t.Fatalf("imports = %#v, want decimal parsed from source", files[0].GetImports())
	}
	if response.GetModule() != "shop" || response.GetLanguage() != "python" {
		t.Fatalf("agent-owned identity was not preserved: module=%q language=%q", response.GetModule(), response.GetLanguage())
	}
}

// An empty inventory without the declined flag is the agent's authoritative
// answer: the unit genuinely has no source files. Refilling it here would
// replace real evidence with a second opinion.
func TestGatewayKeepsAnAuthoritativeEmptyInventory(t *testing.T) {
	server, root := newSourceSemanticsGateway(t)
	writeCodeUnitFixture(t, root, "pyproject.toml", "[project]\nname = \"shop\"\nversion = \"1.0.0\"\n")
	writeCodeUnitFixture(t, root, "checkout.py", "import decimal\n")

	protocoltest.Response(t, root, "project", &codev0.CodeResponse{Result: &codev0.CodeResponse_GetProjectInfo{
		GetProjectInfo: &codev0.GetProjectInfoResponse{Module: "shop", Language: "python"},
	}})

	response, err := server.GetProjectInfo(t.Context(), &gatewayv1.GetProjectInfoRequest{Service: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFailure() != nil {
		t.Fatalf("project-info failure = %+v", response.GetFailure())
	}
	if len(response.GetSourceFiles()) != 0 {
		t.Fatalf("source files = %+v, want the agent's empty inventory preserved", response.GetSourceFiles())
	}
}

// Import extraction fails loudly on malformed source rather than certifying a
// partial inventory as whole. The dependencies, packages and hashes the agent
// did produce are real evidence and must survive that failure.
func TestGatewayReportsAnUntakeableInventoryWithoutLosingAgentEvidence(t *testing.T) {
	server, root := newSourceSemanticsGateway(t)
	writeCodeUnitFixture(t, root, "pyproject.toml", "[project]\nname = \"shop\"\nversion = \"1.0.0\"\n")
	writeCodeUnitFixture(t, root, "broken.py", "def oops(:\n    return\n")

	protocoltest.Response(t, root, "project", &codev0.CodeResponse{Result: &codev0.CodeResponse_GetProjectInfo{
		GetProjectInfo: &codev0.GetProjectInfoResponse{
			Module: "shop", Language: "python", SourceFilesOmitted: true,
			Dependencies: []*codev0.Dependency{{Name: "httpx", Version: "0.28.0", Direct: true}},
		},
	}})

	response, err := server.GetProjectInfo(t.Context(), &gatewayv1.GetProjectInfoRequest{Service: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFailure() == nil {
		t.Fatalf("malformed source must not be reported as a complete inventory: %+v", response)
	}
	if len(response.GetSourceFiles()) != 0 {
		t.Fatalf("source files = %+v, want none alongside the failure", response.GetSourceFiles())
	}
	deps := response.GetDependencies()
	if len(deps) != 1 || deps[0].GetName() != "httpx" {
		t.Fatalf("dependencies = %+v, want the agent's evidence preserved", deps)
	}
	if response.GetModule() != "shop" {
		t.Fatalf("module = %q, want the agent's evidence preserved", response.GetModule())
	}
}

// The semantic projection is computed against the bound root in this process.
// The agent is never asked, which is what lets every language agent drop the
// tree-sitter CGO stack.
func TestGatewaySemanticIndexNeverReachesTheAgent(t *testing.T) {
	server, root := newSourceSemanticsGateway(t)
	writeCodeUnitFixture(t, root, "go.mod", "module shop\n\ngo 1.25\n")
	writeCodeUnitFixture(t, root, "total.go", "package shop\n\nfunc Total() int { return 1 }\n")

	response, err := server.GetSemanticIndex(t.Context(), &gatewayv1.GetSemanticIndexRequest{
		Service: "app", CodeUnit: &gatewayv1.CodeUnitTarget{Id: "shop", Path: "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFailure() != nil {
		t.Fatalf("semantic-index failure = %+v", response.GetFailure())
	}
	index := response.GetIndex()
	if index.GetState() != basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_COMPLETE {
		t.Fatalf("semantic state = %s issues %+v, want a complete engine-computed index", index.GetState(), index.GetIssues())
	}
	found := false
	for _, symbol := range index.GetSymbols() {
		if symbol.GetName() == "Total" && symbol.GetLocation().GetPath() == "total.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("semantic symbols = %+v, want Total in total.go", index.GetSymbols())
	}
	if agentSawOperation(t, root, "getSemanticIndex") {
		t.Fatalf("semantic index was routed to the agent: %+v", protocoltest.Calls(t, root))
	}
}

// Typed symbol mutation is the one Code operation that genuinely needs a
// parser, so the engine resolves the declaration and writes the file itself.
func TestGatewaySymbolPatchNeverReachesTheAgent(t *testing.T) {
	server, root := newSourceSemanticsGateway(t)
	writeCodeUnitFixture(t, root, "go.mod", "module shop\n\ngo 1.25\n")
	declaration := "func Total() int { return 1 }"
	writeCodeUnitFixture(t, root, "total.go", "package shop\n\n"+declaration+"\n")

	index, err := server.GetSemanticIndex(t.Context(), &gatewayv1.GetSemanticIndexRequest{
		Service: "app", CodeUnit: &gatewayv1.CodeUnitTarget{Id: "shop", Path: "."},
	})
	if err != nil || index.GetFailure() != nil {
		t.Fatalf("semantic index: response=%+v err=%v", index.GetFailure(), err)
	}
	qualified := ""
	for _, symbol := range index.GetIndex().GetSymbols() {
		if symbol.GetName() == "Total" {
			qualified = symbol.GetQualifiedName()
		}
	}
	if qualified == "" {
		t.Fatalf("no qualified name for Total in %+v", index.GetIndex().GetSymbols())
	}
	digest := sha256.Sum256([]byte(declaration))

	response, err := server.ApplySymbolPatch(t.Context(), &gatewayv1.ApplySymbolPatchRequest{
		Service: "app", File: "total.go", QualifiedName: qualified,
		ExpectedDeclarationSha256: hex.EncodeToString(digest[:]),
		NewSource:                 "func Total() int { return 2 }",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.GetSuccess() {
		t.Fatalf("symbol patch failed: %+v", response)
	}
	patched, err := os.ReadFile(filepath.Join(root, "total.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patched), "return 2") {
		t.Fatalf("patched file = %q, want the new declaration", patched)
	}
	if agentSawOperation(t, root, "applySymbolPatch") {
		t.Fatalf("symbol patch was routed to the agent: %+v", protocoltest.Calls(t, root))
	}
}

// A prepared mutation is previewed, hashed and re-verified against the tree
// under serviceRoot(). The write must land in that same tree: routing it to the
// agent instead targets whatever root the agent resolved, so a declared
// source-dir would put bytes derived from one file on top of a different one —
// and the preview/authoritative hash check at prepare time can no longer catch
// it, because both sides of that comparison now read the gateway's tree.
func TestPreparedMutationWritesToTheVerifiedTreeNotTheAgent(t *testing.T) {
	server, privateKey, root := newPreparedMutationGateway(t)
	path := filepath.Join(root, "pkg", "service.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	before := "package service\n\nfunc Value() int { return 1 }\n"
	declaration := "func Value() int { return 1 }"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	preparedResponse, err := server.PrepareMutation(t.Context(), &gatewayv1.PrepareMutationRequest{
		Service: "app", WorkspaceVersion: "workspace-verified-tree-v1",
		Mutation: &gatewayv1.PrepareMutationRequest_SymbolPatch{SymbolPatch: &gatewayv1.PrepareSymbolPatchMutation{
			File: "pkg/service.go", SymbolId: "symbol-service-value", QualifiedName: "service.Value",
			ExpectedDeclarationSha256: contentSHA256([]byte(declaration)),
			NewSource:                 "func Value() int { return 2 }", FixMode: basev0.FixMode_FIX_MODE_NONE,
		}},
	})
	if err != nil || !preparedResponse.GetSuccess() {
		t.Fatalf("prepare symbol mutation: response=%+v err=%v", preparedResponse, err)
	}
	prepared := preparedResponse.GetPrepared()

	permit := signPreparedMutationPermit(t, privateKey, prepared, time.Now().UTC().Add(-time.Second), time.Minute)
	applied, err := server.ApplyPreparedMutation(t.Context(), &gatewayv1.ApplyPreparedMutationRequest{
		Service: "app", PreparationId: prepared.GetPreparationId(),
		MutationDigest: prepared.GetMutationDigest(), MutationPermit: permit,
	})
	if err != nil || !applied.GetSuccess() {
		t.Fatalf("apply prepared symbol mutation: response=%+v err=%v", applied, err)
	}

	after, err := os.ReadFile(path)
	if err != nil || string(after) != "package service\n\nfunc Value() int { return 2 }\n" {
		t.Fatalf("prepared bytes did not land in the verified tree: content=%q err=%v", after, err)
	}
	if agentSawOperation(t, root, "writeFile") {
		t.Fatalf("prepared write was routed to the agent, whose root need not be the verified tree: %+v", protocoltest.Calls(t, root))
	}
}

// The engine resolves and writes a typed symbol patch, so its receipt must not
// claim a plugin returned the effect. GATEWAY_EXECUTED and PLUGIN_EXECUTED are
// distinct provenance claims and the receipt is persisted and signed.
func TestSymbolPatchReceiptRecordsGatewayExecution(t *testing.T) {
	server, _, root := newPreparedMutationGateway(t)
	path := filepath.Join(root, "pkg", "service.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	declaration := "func Value() int { return 1 }"
	if err := os.WriteFile(path, []byte("package service\n\n"+declaration+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture := enableGovernedGateway(t, server, "operation-symbol-patch-1")

	response, err := server.ApplySymbolPatch(fixture.ctx, &gatewayv1.ApplySymbolPatchRequest{
		Service: "app", File: "pkg/service.go", QualifiedName: "service.Value",
		ExpectedDeclarationSha256: contentSHA256([]byte(declaration)),
		NewSource:                 "func Value() int { return 2 }",
	})
	if err != nil || !response.GetSuccess() {
		t.Fatalf("apply symbol patch: response=%+v err=%v", response, err)
	}
	pending, err := fixture.journal.Pending(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) == 0 {
		t.Fatal("symbol patch recorded no execution receipt")
	}
	started := pending[0].Attestation.GetReceipt()
	if started.GetOperationKind() != "code.apply-symbol-patch" {
		t.Fatalf("receipt operation kind = %q", started.GetOperationKind())
	}
	if started.GetAssurance() != executionv1.ExecutionAssurance_EXECUTION_ASSURANCE_GATEWAY_EXECUTED {
		t.Fatalf("receipt assurance = %s, want GATEWAY_EXECUTED for an engine-performed effect", started.GetAssurance())
	}
}

// Malformed source must degrade the engine-computed index rather than fail it:
// the parseable files still yield symbols and the unparseable one is reported
// as an issue. This is the coverage the agent-routed path used to carry.
func TestGatewaySemanticIndexDegradesOnMalformedSource(t *testing.T) {
	server, root := newSourceSemanticsGateway(t)
	writeCodeUnitFixture(t, root, "go.mod", "module shop\n\ngo 1.25\n")
	writeCodeUnitFixture(t, root, "total.go", "package shop\n\nfunc Total() int { return 1 }\n")
	writeCodeUnitFixture(t, root, "broken.go", "package shop\n\nfunc Broken( {\n")

	response, err := server.GetSemanticIndex(t.Context(), &gatewayv1.GetSemanticIndexRequest{
		Service: "app", CodeUnit: &gatewayv1.CodeUnitTarget{Id: "shop", Path: "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFailure() != nil {
		t.Fatalf("malformed source failed the whole index instead of degrading it: %+v", response.GetFailure())
	}
	index := response.GetIndex()
	if index.GetState() != basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_DEGRADED {
		t.Fatalf("semantic state = %s, want DEGRADED (issues=%+v)", index.GetState(), index.GetIssues())
	}
	brokenReported := false
	for _, issue := range index.GetIssues() {
		if issue.GetPath() == "broken.go" {
			brokenReported = true
		}
	}
	if !brokenReported {
		t.Fatalf("degraded index did not report broken.go: %+v", index.GetIssues())
	}
	healthy := false
	for _, symbol := range index.GetSymbols() {
		if symbol.GetName() == "Total" {
			healthy = true
		}
	}
	if !healthy {
		t.Fatalf("degraded index dropped the parseable file's symbols: %+v", index.GetSymbols())
	}
}

// newSourceDirGateway builds a gateway whose service declares source-dir, with
// a byte-identical shadow of the target file at the service root. The shadow is
// what makes the bug silent: hash-and-size checks cannot tell the two apart, so
// only the path that is actually written distinguishes right from wrong.
func newSourceDirGateway(t *testing.T, relative, content string) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	writeCodeUnitFixture(t, root, "workspace.codefly.yaml", "name: shop\nlayout: flat\n")
	writeCodeUnitFixture(t, root, "service.codefly.yaml", "name: app\nversion: 0.0.1\nspec:\n  source-dir: src\n")
	writeCodeUnitFixture(t, root, "mind.yaml", "service: app\nplugin: generic-go\n")
	writeCodeUnitFixture(t, root, filepath.Join("src", relative), content)
	writeCodeUnitFixture(t, root, relative, content)

	server, err := NewServer(Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server, root
}

// The gateway resolves symbol patches itself, so it must address the tree the
// agent calls its source. With source-dir set, patching the service-root copy
// leaves the compiled source untouched while reporting success.
func TestSymbolPatchTargetsTheAgentSourceRootNotTheServiceRoot(t *testing.T) {
	declaration := "func Value() int { return 1 }"
	body := "package service\n\n" + declaration + "\n"
	server, root := newSourceDirGateway(t, filepath.Join("pkg", "service.go"), body)

	response, err := server.ApplySymbolPatch(t.Context(), &gatewayv1.ApplySymbolPatchRequest{
		Service: "app", File: "pkg/service.go", QualifiedName: "service.Value",
		ExpectedDeclarationSha256: contentSHA256([]byte(declaration)),
		NewSource:                 "func Value() int { return 2 }",
	})
	if err != nil || !response.GetSuccess() {
		t.Fatalf("apply symbol patch: response=%+v err=%v", response, err)
	}

	patched, err := os.ReadFile(filepath.Join(root, "src", "pkg", "service.go"))
	if err != nil || !strings.Contains(string(patched), "return 2") {
		t.Fatalf("agent source tree was not patched: content=%q err=%v", patched, err)
	}
	shadow, err := os.ReadFile(filepath.Join(root, "pkg", "service.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(shadow) != body {
		t.Fatalf("the service-root shadow was patched instead of the agent's source: %q", shadow)
	}
}

// The same root has to govern prepared mutations end to end: preview, the
// authoritative re-read, and the write. A prepared symbol patch must move the
// agent's source and leave the identical shadow alone.
func TestPreparedMutationTargetsTheAgentSourceRoot(t *testing.T) {
	declaration := "func Value() int { return 1 }"
	body := "package service\n\n" + declaration + "\n"
	server, root := newSourceDirGateway(t, filepath.Join("pkg", "service.go"), body)

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.ConfigureMutationAuthority(t.Context(), &gatewayv1.ConfigureMutationAuthorityRequest{
		AuthorityId: "coordinator-key-v1", WorkspaceId: "workspace-" + uuid.NewString(), Ed25519PublicKey: publicKey,
	}); err != nil {
		t.Fatal(err)
	}

	preparedResponse, err := server.PrepareMutation(t.Context(), &gatewayv1.PrepareMutationRequest{
		Service: "app", WorkspaceVersion: "workspace-source-dir-v1",
		Mutation: &gatewayv1.PrepareMutationRequest_SymbolPatch{SymbolPatch: &gatewayv1.PrepareSymbolPatchMutation{
			File: "pkg/service.go", SymbolId: "symbol-service-value", QualifiedName: "service.Value",
			ExpectedDeclarationSha256: contentSHA256([]byte(declaration)),
			NewSource:                 "func Value() int { return 2 }", FixMode: basev0.FixMode_FIX_MODE_NONE,
		}},
	})
	if err != nil || !preparedResponse.GetSuccess() {
		t.Fatalf("prepare symbol mutation: response=%+v err=%v", preparedResponse, err)
	}
	prepared := preparedResponse.GetPrepared()
	permit := signPreparedMutationPermit(t, privateKey, prepared, time.Now().UTC().Add(-time.Second), time.Minute)
	applied, err := server.ApplyPreparedMutation(t.Context(), &gatewayv1.ApplyPreparedMutationRequest{
		Service: "app", PreparationId: prepared.GetPreparationId(),
		MutationDigest: prepared.GetMutationDigest(), MutationPermit: permit,
	})
	if err != nil || !applied.GetSuccess() {
		t.Fatalf("apply prepared mutation: response=%+v err=%v", applied, err)
	}

	patched, err := os.ReadFile(filepath.Join(root, "src", "pkg", "service.go"))
	if err != nil || !strings.Contains(string(patched), "return 2") {
		t.Fatalf("agent source tree was not written: content=%q err=%v", patched, err)
	}
	shadow, err := os.ReadFile(filepath.Join(root, "pkg", "service.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(shadow) != body {
		t.Fatalf("prepared bytes landed on the service-root shadow: %q", shadow)
	}
}

// An ephemeral source workspace is bound when no workspace configuration
// encloses the root, and its attachment resolves back to that root — so a
// source-dir beside it is not the declaration the agent reads, and honouring it
// would move the gateway off the agent's tree instead of onto it.
func TestAgentSourceRootIgnoresSourceDirWithoutAnEnclosingWorkspace(t *testing.T) {
	root := t.TempDir()
	writeCodeUnitFixture(t, root, "service.codefly.yaml", "name: app\nversion: 0.0.1\nspec:\n  source-dir: src\n")
	writeCodeUnitFixture(t, root, filepath.Join("src", "keep.go"), "package keep\n")

	resolved := engine.AgentSourceRoot(t.Context(), root)
	physical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != physical && resolved != root {
		t.Fatalf("source root = %q, want the service root %q when no workspace encloses it", resolved, root)
	}
}
