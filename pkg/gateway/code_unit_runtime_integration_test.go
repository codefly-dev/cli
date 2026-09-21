package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

// Routing and aggregation use controlled wire responses, not language runtimes.
func TestGatewayRoutesEveryDeclaredCodeUnitToItsSelectedPeer(t *testing.T) {
	root := t.TempDir()
	peers := protocoltest.Install(t, "backend-peer", "frontend-peer")
	writeCodeUnitFixture(t, root, "mind.yaml", "source_agents:\n  backend: "+peers[0]+"\n  frontend: "+peers[1]+"\n")
	writeCodeUnitFixture(t, root, "backend/test_backend.txt", "opaque backend input\n")
	writeCodeUnitFixture(t, root, "frontend/test_frontend.txt", "opaque frontend input\n")

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
				File: &runtimev0.TestFileSelection{Path: "backend/test_backend.txt"},
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
	if got := selectedRuntime.GetRun().GetRequestedSelection().GetFile().GetPath(); got != "backend/test_backend.txt" {
		t.Fatalf("reported selection path = %q, want repository-relative path", got)
	}
	if counts := selectedRuntime.GetCounts(); counts.GetTotal() != 1 || counts.GetPassed() != 1 {
		t.Fatalf("selected counts = %+v, want one passing backend test", counts)
	}
	for _, unit := range []string{"backend", "frontend"} {
		var requests []*runtimev0.TestRequest
		for _, call := range protocoltest.Calls(t, filepath.Join(root, unit)) {
			if call.Method == "Runtime.Test" {
				request := &runtimev0.TestRequest{}
				require.NoError(t, protojson.Unmarshal(call.Request, request))
				requests = append(requests, request)
			}
		}
		if unit == "backend" {
			require.Len(t, requests, 2)
			require.Equal(t, "test_backend.txt", requests[1].GetSelection().GetFile().GetPath())
			require.Equal(t, "backend-file-selection", requests[1].GetSelectionId())
		} else {
			require.Len(t, requests, 1, "selection must not dispatch to the unrelated unit")
		}
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
