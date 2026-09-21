package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
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
