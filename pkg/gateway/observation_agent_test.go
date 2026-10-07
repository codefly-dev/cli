package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"

	"github.com/codefly-dev/cli/pkg/engine"

	"github.com/codefly-dev/cli/pkg/gateway/effect"
)

// No observation starts a process.
//
// r16's hole: ListAllCommands, ListDependencies and GetProjectInfo were classed
// as observations and reached AgentSupervisor.acquire, so a governed caller had
// Build refused at the transport boundary while ListAllCommands spawned a
// language agent. Starting a process is an effect, and the refusal now lives at
// that funnel — which makes the funnel a DETECTOR here: an observation that
// tries to start an agent surfaces effect.RefuseGoverned, and this walk fails
// it by name.
//
// The walk is over the generated descriptor's observations, so an RPC added as
// an observation that needs an agent fails without anyone remembering to list
// it. The assertion is implementation-independent: it does not inspect the OS
// process table, it asks whether the one code path that starts a process was
// reached.
func TestNoObservationStartsAnAgent(t *testing.T) {
	root := t.TempDir()
	// A workspace whose service declares a real agent, so acquire would have
	// something to start. Without this the test could pass by having nothing
	// to spawn.
	writeAgentRequiringWorkspace(t, root)

	server, err := NewServer(&Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	governed := incomingExecutionContext(t, "operation-observation-walk")
	descriptor := gatewayv1.Gateway_ServiceDesc
	walked := 0
	var offenders []string

	for _, method := range descriptor.Methods {
		class, known := effect.ClassOf(method.MethodName)
		if !known || class != effect.Observation {
			continue
		}
		walked++
		// The ATTEMPT is what is measured, not the refusal. Callers swallow
		// the refusal — ListAllCommands proceeds `if err == nil`, so a
		// governed refusal there becomes a response quietly missing its agent
		// commands — and an earlier version of this test looked for the
		// refusal in the result and reported no offender at all, while r16 had
		// executed three. Counting entries into the funnel cannot be swallowed.
		before := engine.AgentStartAttempts()
		_, _ = method.Handler(server, governed, leaveRequestZero, effect.UnaryInterceptor())
		if engine.AgentStartAttempts() != before {
			offenders = append(offenders, method.MethodName)
		}
	}

	if walked == 0 {
		t.Fatal("no observation was walked, so this test proves nothing")
	}
	if len(offenders) > 0 {
		t.Fatalf("these observations tried to start a process for a governed caller: %s\n"+
			"starting a process is an effect: class them Effect in pkg/gateway/effect",
			strings.Join(offenders, ", "))
	}
}

// The funnel's own refusal is tested where it cannot be skipped, in
// pkg/engine: TestAcquireRefusesAGovernedRequestBeforeStartingAnything. A
// gateway-level version had to skip in an environment with no installed agent,
// which is most environments, so it proved nothing here.

// writeAgentRequiringWorkspace is a workspace whose single service names a
// language agent, so the supervisor has an agent to start.
func writeAgentRequiringWorkspace(t *testing.T, root string) {
	t.Helper()
	for name, content := range map[string]string{
		"workspace.codefly.yaml": "name: observation\nlayout: modules\nmodules:\n    - name: backend\n",
		"modules/backend/module.codefly.yaml": "kind: module\nname: backend\n" +
			"services:\n    - name: api\n",
		"modules/backend/services/api/service.codefly.yaml": "kind: service\nname: api\nversion: 0.0.0\nmodule: backend\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"main.go": "package main\n\nfunc main() {}\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// reachedTheProcessStart reports whether the governed refusal appears anywhere
// in a result, error or response alike.
func reachedTheProcessStart(result any) bool {
	switch value := result.(type) {
	case nil:
		return false
	case error:
		return strings.Contains(value.Error(), "requires a verified Work Context")
	case fmt.Stringer:
		return strings.Contains(value.String(), "requires a verified Work Context")
	default:
		return strings.Contains(fmt.Sprintf("%v", value), "requires a verified Work Context")
	}
}
