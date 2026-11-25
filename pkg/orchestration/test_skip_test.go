package orchestration

import (
	"context"
	"testing"

	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v1"
	"google.golang.org/grpc"
)

func noTestSuitesInformation() *agentv1.AgentInformation {
	return &agentv1.AgentInformation{Validation: &agentv1.ValidationCapabilities{
		Test: &agentv1.TestValidationCapability{},
	}}
}

type countingTestRuntimeClient struct {
	runtimev1.RuntimeClient
	testCalls int
}

func (client *countingTestRuntimeClient) Test(context.Context, *runtimev1.TestRequest, ...grpc.CallOption) (*runtimev1.TestResponse, error) {
	client.testCalls++
	return &runtimev1.TestResponse{
		Result: &runtimev1.TestRunResult{State: runtimev1.TestRunResult_PASSED},
	}, nil
}

func TestResolveTestExecutionSkipsAgentWithoutSuites(t *testing.T) {
	execution, err := resolveTestExecution(noTestSuitesInformation(), &runtimev1.TestRequest{})
	if err != nil {
		t.Fatalf("unsupported test capability failed resolution: %v", err)
	}
	if !execution.skipped {
		t.Fatal("agent advertising no test capability did not resolve to a skip")
	}
	if execution.legacy {
		t.Fatal("an explicit unsupported advertisement was treated as a legacy agent")
	}
	if execution.DependencyMode != agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE {
		t.Fatalf("dependency mode = %s, want NONE so nothing is started for a skip", execution.DependencyMode)
	}
}

func TestRunnerTestSkipsWithoutDispatchingRPC(t *testing.T) {
	client := &countingTestRuntimeClient{}
	runner := runnerWithRuntimeClient(client)
	runner.instance.Info = noTestSuitesInformation()

	outputProperty, err := runner.Test(context.Background())
	if err != nil {
		t.Fatalf("agent without test suites failed the test phase: %v", err)
	}
	if outputProperty == nil {
		t.Fatal("skipped test returned no outputProperty, so the playbook cannot continue")
	}
	if client.testCalls != 0 {
		t.Fatalf("Test RPC dispatched %d time(s) for an agent that advertises no tests", client.testCalls)
	}
	if !runner.TestSkipped() {
		t.Fatal("skip was not recorded, so the CI report would show an unearned pass")
	}
	if runner.TestResponse() != nil {
		t.Fatal("skipped test recorded a response")
	}
}

func TestRunnerTestStillDispatchesWhenSuitesAreAdvertised(t *testing.T) {
	client := &countingTestRuntimeClient{}
	runner := runnerWithRuntimeClient(client)
	runner.outputPropertyForTest = NewRunnerTestManager(runner.instance.Unique())
	runner.instance.Info = testInformation(&agentv1.TestSuiteCapability{
		Name:           "unit",
		DependencyMode: agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE,
		DefaultSuite:   true,
	})

	if _, err := runner.Test(context.Background()); err != nil {
		t.Fatalf("advertised test suite failed: %v", err)
	}
	if client.testCalls != 1 {
		t.Fatalf("Test RPC dispatched %d time(s), want exactly 1", client.testCalls)
	}
	if runner.TestSkipped() {
		t.Fatal("a real test run was recorded as skipped")
	}
}
