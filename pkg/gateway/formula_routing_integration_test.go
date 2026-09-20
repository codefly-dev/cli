package gateway

import (
	"strings"
	"testing"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"github.com/codefly-dev/core/resources"
)

// A formula describes work, not an agent identity or a compatibility exception.
func TestFormulaDoesNotSelectConcreteAgentOnMarkerlessSource(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	server, err := NewServer(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	for _, command := range [][]string{nil, {"python", "-c", "pass"}, {"go", "test", "./..."}} {
		response, err := server.Test(t.Context(), &gatewayv1.TestRequest{RuntimeRequest: &runtimev0.TestRequest{
			Formula: &runtimev0.TestFormula{Command: command},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if response.GetSuccess() || !strings.Contains(response.GetOutput(), "select an agent explicitly") {
			t.Fatalf("formula %v selected an agent without evidence: %+v", command, response)
		}
	}
}
