package gateway

import (
	"os"
	"path/filepath"
	"testing"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

// TestGatewayRunsEveryDeclaredCodeUnitThroughItsProductionPlugin is the
// regression for heterogeneous attached projects being collapsed into the
// first root-detected agent. It uses the real Python and generic Node agents;
// neither runtime, filesystem, nor gateway behavior is mocked.
func TestGatewayRunsEveryDeclaredCodeUnitThroughItsProductionPlugin(t *testing.T) {
	root := t.TempDir()
	writeCodeUnitFixture(t, root, "backend/pyproject.toml", `[project]
name = "gateway-polyglot-backend"
version = "0.0.0"
`)
	writeCodeUnitFixture(t, root, "backend/test_backend.py", `def test_backend_runtime():
    assert "backend".upper() == "BACKEND"
`)
	writeCodeUnitFixture(t, root, "frontend/package.json", `{
  "name": "gateway-polyglot-frontend",
  "version": "0.0.0",
  "private": true,
  "scripts": {"test": "node --test"}
}
`)
	writeCodeUnitFixture(t, root, "frontend/package-lock.json", `{
  "name": "gateway-polyglot-frontend",
  "version": "0.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {"": {"name": "gateway-polyglot-frontend", "version": "0.0.0"}}
}
`)
	writeCodeUnitFixture(t, root, "frontend/frontend.test.js", `const test = require("node:test");
const assert = require("node:assert/strict");

test("frontend runtime", () => {
  assert.equal("frontend".toUpperCase(), "FRONTEND");
});
`)

	server, err := NewServer(Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	response, err := server.Test(t.Context(), &gatewayv1.TestRequest{
		RuntimeRequest: &runtimev0.TestRequest{},
		CodeUnits: []*gatewayv1.CodeUnitTarget{
			{Id: "polyglot/backend", Path: "backend"},
			{Id: "polyglot/frontend", Path: "frontend"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeResponse := response.GetRuntimeResponse()
	if state := runtimeResponse.GetResult().GetState(); state != runtimev0.TestRunResult_PASSED {
		t.Fatalf("multi-unit state = %s (%s)\noutput:\n%s", state, runtimeResponse.GetResult().GetMessage(), response.GetOutput())
	}
	if got := runtimeResponse.GetRun().GetRunner(); got != "codefly-multi-unit" {
		t.Fatalf("runner = %q, want codefly-multi-unit", got)
	}
	if counts := runtimeResponse.GetCounts(); counts.GetTotal() != 2 || counts.GetPassed() != 2 {
		t.Fatalf("counts = %+v, want two passing tests", counts)
	}
	if suites := runtimeResponse.GetSuites(); len(suites) != 2 || suites[0].GetName() != "code-unit:polyglot/backend" || suites[1].GetName() != "code-unit:polyglot/frontend" {
		t.Fatalf("code-unit suites = %+v", suites)
	}

	selected, err := server.Test(t.Context(), &gatewayv1.TestRequest{
		RuntimeRequest: &runtimev0.TestRequest{
			Selection: &runtimev0.TestSelection{Scope: &runtimev0.TestSelection_File{
				File: &runtimev0.TestFileSelection{Path: "backend/test_backend.py"},
			}},
			SelectionId: "backend-file-selection",
		},
		CodeUnits: []*gatewayv1.CodeUnitTarget{
			{Id: "polyglot/backend", Path: "backend"},
			{Id: "polyglot/frontend", Path: "frontend"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	selectedRuntime := selected.GetRuntimeResponse()
	if state := selectedRuntime.GetResult().GetState(); state != runtimev0.TestRunResult_PASSED {
		t.Fatalf("selected state = %s (%s)\noutput:\n%s", state, selectedRuntime.GetResult().GetMessage(), selected.GetOutput())
	}
	if got := selectedRuntime.GetRun().GetSelectionId(); got != "backend-file-selection" {
		t.Fatalf("selection acknowledgement = %q", got)
	}
	if got := selectedRuntime.GetRun().GetRequestedSelection().GetFile().GetPath(); got != "backend/test_backend.py" {
		t.Fatalf("reported selection path = %q, want repository-relative path", got)
	}
	if counts := selectedRuntime.GetCounts(); counts.GetTotal() != 1 || counts.GetPassed() != 1 {
		t.Fatalf("selected counts = %+v, want one passing backend test", counts)
	}
}

func writeCodeUnitFixture(t *testing.T, root, relative, body string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
