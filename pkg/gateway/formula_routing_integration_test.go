package gateway

import (
	"strings"
	"testing"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

// TestExplicitFormulaOutranksCachedGenericSource proves that typed runtime
// evidence controls dispatch even after a markerless root has bound the generic
// agent. The Gateway and both production agents are real; no runtime behavior
// is substituted by the test.
func TestExplicitFormulaOutranksCachedGenericSource(t *testing.T) {
	server, err := NewServer(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	generic, err := server.Test(t.Context(), &gatewayv1.TestRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if generic.GetSuccess() {
		t.Fatal("generic source runtime unexpectedly reported test success")
	}

	routed, err := server.Test(t.Context(), &gatewayv1.TestRequest{RuntimeRequest: &runtimev0.TestRequest{
		Formula: &runtimev0.TestFormula{
			Command:      []string{"python", "-c", "pass"},
			Output:       "unittest-text",
			Provisioning: map[string]string{"no_project": "true"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result := routed.GetRuntimeResponse().GetResult()
	if result.GetState() != runtimev0.TestRunResult_ERRORED {
		t.Fatalf("formula result = %s (%s), want Python runtime ERRORED", result.GetState(), result.GetMessage())
	}
	if !strings.Contains(result.GetMessage(), "no-tests-executed") {
		t.Fatalf("formula result message = %q, want Python no-tests-executed classification", result.GetMessage())
	}
}
