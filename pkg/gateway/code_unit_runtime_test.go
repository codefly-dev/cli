package gateway

import (
	"strings"
	"testing"
	"unicode/utf8"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"google.golang.org/protobuf/proto"
)

func TestRebaseCodeUnitSemanticIndexPreservesFactsAndRebasesEveryPath(t *testing.T) {
	original := &basev0.SemanticIndex{
		State: basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_DEGRADED,
		Files: []*basev0.SemanticFile{{Path: "src/main.go", ContentSha256: "abc"}},
		Symbols: []*basev0.SemanticSymbol{{
			QualifiedName: "demo.Main", Location: &basev0.SemanticLocation{Path: "src/main.go", StartLine: 3},
			Calls:      []*basev0.SemanticUse{{Name: "Run", Location: &basev0.SemanticLocation{Path: "src/main.go", StartLine: 4}}},
			References: []*basev0.SemanticUse{{Name: "Config", Location: &basev0.SemanticLocation{Path: "src/config.go", StartLine: 5}}},
		}},
		Issues: []*basev0.SemanticIssue{{Code: "parse_failed", Path: "src/broken.go"}, {Code: "unsupported_source"}},
	}

	rebased := rebaseCodeUnitSemanticIndex("services/api", original)
	if got := rebased.GetFiles()[0].GetPath(); got != "services/api/src/main.go" {
		t.Fatalf("file path = %q", got)
	}
	symbol := rebased.GetSymbols()[0]
	if symbol.GetLocation().GetPath() != "services/api/src/main.go" ||
		symbol.GetCalls()[0].GetLocation().GetPath() != "services/api/src/main.go" ||
		symbol.GetReferences()[0].GetLocation().GetPath() != "services/api/src/config.go" {
		t.Fatalf("semantic locations were not rebased: %#v", symbol)
	}
	if rebased.GetIssues()[0].GetPath() != "services/api/src/broken.go" || rebased.GetIssues()[1].GetPath() != "" {
		t.Fatalf("issue paths = %#v", rebased.GetIssues())
	}
	if original.GetFiles()[0].GetPath() != "src/main.go" || original.GetSymbols()[0].GetLocation().GetPath() != "src/main.go" {
		t.Fatal("rebasing mutated the agent response")
	}
	if rebased.GetState() != original.GetState() || rebased.GetFiles()[0].GetContentSha256() != "abc" {
		t.Fatal("rebasing changed semantic facts")
	}
}

// TestPackageSelectionForwardsPluginPackageIdentityUnchanged pins the contract
// that a package selection routed to a unit is forwarded verbatim.
// TestPackageSelection.package is the plugin's project-level package identity
// (an import path / module / crate), already expressed in the owning unit's
// namespace — NOT a workspace-relative path. File and test-case PATHS are
// rebased to the unit root; a package identity must not be, or import paths
// would be corrupted.
func TestPackageSelectionForwardsPluginPackageIdentityUnchanged(t *testing.T) {
	targets := []normalizedCodeUnitTarget{
		{id: "backend", path: "backend", root: "/repo/backend"},
		{id: "frontend", path: "frontend", root: "/repo/frontend"},
	}
	request := &runtimev0.TestRequest{
		Selection: &runtimev0.TestSelection{Scope: &runtimev0.TestSelection_Package{
			Package: &runtimev0.TestPackageSelection{Package: "backend/internal/handlers"},
		}},
	}

	runs, err := routeCodeUnitTestRequest(request, targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].target.id != "backend" {
		t.Fatalf("routed runs = %+v, want a single backend run", runs)
	}
	if got := runs[0].request.GetSelection().GetPackage().GetPackage(); got != "backend/internal/handlers" {
		t.Fatalf("forwarded package = %q, want it forwarded unchanged", got)
	}
}

// TestAggregateOutputStaysValidUTF8WhenTruncated proves the bounded aggregate
// Output never splits a multibyte rune. A unit whose output overflows the cap
// with 3-byte runes would, under a byte-boundary cut, leave a partial rune —
// invalid UTF-8 for a proto3 string, which makes the whole TestResponse fail to
// marshal. The id/path lengths below are chosen so the cut lands mid-rune.
func TestAggregateOutputStaysValidUTF8WhenTruncated(t *testing.T) {
	// "[u @ vv]\n" is 9 bytes; with 3-byte runes the byte cut at the cap lands 2
	// bytes into a rune (524288-9 ≡ 2 mod 3), the worst case for truncation.
	target := normalizedCodeUnitTarget{id: "u", path: "vv"}
	runes := maxCodeUnitAggregateOutputSize/utf8.RuneLen('世') + 1
	overflowing := &runtimev0.TestResponse{
		Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_PASSED},
		Counts: &runtimev0.TestCounts{Total: 1, Passed: 1},
		Output: strings.Repeat("世", runes),
	}

	aggregate := aggregateCodeUnitTestResponses(&runtimev0.TestRequest{}, []codeUnitTestResult{
		{target: target, response: overflowing},
		{target: normalizedCodeUnitTarget{id: "other", path: "o"}, response: &runtimev0.TestResponse{
			Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_PASSED},
			Counts: &runtimev0.TestCounts{Total: 1, Passed: 1},
			Output: "ok",
		}},
	})

	if got := aggregate.GetOutput(); !utf8.ValidString(got) {
		t.Fatalf("aggregate output is not valid UTF-8 (len=%d)", len(got))
	}
	if len(aggregate.GetOutput()) > maxCodeUnitAggregateOutputSize {
		t.Fatalf("aggregate output %d exceeds cap %d", len(aggregate.GetOutput()), maxCodeUnitAggregateOutputSize)
	}
	// The invalid-UTF-8 regression surfaces as a marshal failure on the wire.
	if _, err := proto.Marshal(aggregate); err != nil {
		t.Fatalf("aggregate TestResponse failed to marshal: %v", err)
	}
}
