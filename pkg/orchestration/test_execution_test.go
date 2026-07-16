package orchestration

import (
	"context"
	"strings"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
)

func testInformation(suites ...*agentv0.TestSuiteCapability) *agentv0.AgentInformation {
	return &agentv0.AgentInformation{Validation: &agentv0.ValidationCapabilities{
		Test: &agentv0.TestValidationCapability{Supported: true, Suites: suites},
	}}
}

func TestResolveTestExecutionLegacyPreservesRequestAndUsesNoDependencies(t *testing.T) {
	original := &runtimev0.TestRequest{Suite: "custom", Filters: []string{"Auth"}}
	execution, err := resolveTestExecution(&agentv0.AgentInformation{}, original)
	if err != nil {
		t.Fatal(err)
	}
	if execution.DependencyMode != agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE {
		t.Fatalf("dependency mode = %s, want NONE", execution.DependencyMode)
	}
	if !execution.legacy || execution.Request.GetSuite() != "custom" {
		t.Fatalf("legacy execution = %#v", execution)
	}
	execution.Request.Filters[0] = "Changed"
	if original.Filters[0] != "Auth" {
		t.Fatal("resolveTestExecution mutated the caller's request")
	}
}

func TestResolveTestExecutionSelectsAdvertisedDefaultAndExplicitSuite(t *testing.T) {
	info := testInformation(
		&agentv0.TestSuiteCapability{
			Name:           "unit",
			DependencyMode: agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE,
			DefaultSuite:   true,
		},
		&agentv0.TestSuiteCapability{
			Name:           "integration",
			DependencyMode: agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES,
		},
	)

	defaultExecution, err := resolveTestExecution(info, nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaultExecution.Request.GetSuite() != "unit" || defaultExecution.DependencyMode != agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE {
		t.Fatalf("default execution = %#v", defaultExecution)
	}

	explicitExecution, err := resolveTestExecution(info, &runtimev0.TestRequest{Suite: "integration"})
	if err != nil {
		t.Fatal(err)
	}
	if explicitExecution.DependencyMode != agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES {
		t.Fatalf("explicit dependency mode = %s", explicitExecution.DependencyMode)
	}
}

func TestResolveTestExecutionRejectsInvalidAuthoritativeContracts(t *testing.T) {
	valid := &agentv0.TestSuiteCapability{
		Name:           "unit",
		DependencyMode: agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE,
		DefaultSuite:   true,
	}
	tests := []struct {
		name    string
		info    *agentv0.AgentInformation
		request *runtimev0.TestRequest
		want    string
	}{
		{name: "unsupported suite", info: testInformation(valid), request: &runtimev0.TestRequest{Suite: "e2e"}, want: "not advertised"},
		{name: "no default", info: testInformation(&agentv0.TestSuiteCapability{Name: "unit", DependencyMode: agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE}), want: "does not advertise a default"},
		{name: "multiple defaults", info: testInformation(valid, valid), want: "multiple default"},
		{name: "unspecified mode", info: testInformation(&agentv0.TestSuiteCapability{Name: "unit", DefaultSuite: true}), want: "incomplete dependency mode"},
		{name: "test unsupported", info: &agentv0.AgentInformation{Validation: &agentv0.ValidationCapabilities{Test: &agentv0.TestValidationCapability{}}}, want: "explicitly unsupported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveTestExecution(tt.info, tt.request)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

type recordingTestExecutor struct {
	actions []Action
}

func (executor *recordingTestExecutor) GetExecutor(_ context.Context, action Action) (OutputProcessorFunc, error) {
	executor.actions = append(executor.actions, action)
	return func(context.Context) (*OutputProperty, error) { return OnInit(), nil }, nil
}

func TestRuntimeTestPolicyUsesTargetStartAsDependencyBarrier(t *testing.T) {
	origin := "app/frontend"
	recorder := &recordingTestExecutor{}
	policy, err := NewRuntimeTestPolicy(context.Background(), nil, recorder, origin, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES)
	if err != nil {
		t.Fatal(err)
	}
	next, err := policy.Execute(context.Background(), Action{Type: RuntimeStart, Service: origin})
	if err != nil {
		t.Fatal(err)
	}
	if len(recorder.actions) != 0 {
		t.Fatalf("target RuntimeStart reached the executor: %v", recorder.actions)
	}
	if len(next) != 1 || next[0].Type != RuntimeTest || next[0].Service != origin {
		t.Fatalf("next = %v, want target RuntimeTest", next)
	}
}

func TestRuntimeTestPolicyStartsTargetForStackSuite(t *testing.T) {
	origin := "app/frontend"
	recorder := &recordingTestExecutor{}
	policy, err := NewRuntimeTestPolicy(context.Background(), nil, recorder, origin, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK)
	if err != nil {
		t.Fatal(err)
	}
	next, err := policy.Execute(context.Background(), Action{Type: RuntimeStart, Service: origin})
	if err != nil {
		t.Fatal(err)
	}
	if len(recorder.actions) != 1 || recorder.actions[0].Type != RuntimeStart {
		t.Fatalf("executed actions = %v, want target RuntimeStart", recorder.actions)
	}
	if len(next) != 1 || next[0].Type != RuntimeTest {
		t.Fatalf("next = %v, want target RuntimeTest", next)
	}
}

func TestRuntimeTestPolicyRejectsUnspecifiedDependencyMode(t *testing.T) {
	if _, err := NewRuntimeTestPolicy(context.Background(), nil, &recordingTestExecutor{}, "app/frontend", agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_UNSPECIFIED); err == nil {
		t.Fatal("unspecified dependency mode was accepted")
	}
}
