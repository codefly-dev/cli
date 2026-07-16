package orchestration

import (
	"fmt"
	"strings"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"google.golang.org/protobuf/proto"
)

type testExecution struct {
	Request        *runtimev0.TestRequest
	DependencyMode agentv0.TestDependencyMode
	legacy         bool
}

func (execution testExecution) DisplaySuite() string {
	if execution.Request.GetSuite() == "" {
		return "default"
	}
	return execution.Request.GetSuite()
}

// resolveTestExecution turns the agent's suite advertisement into one binding
// lifecycle decision. A nil validation contract is the only compatibility
// path and retains the historical dependency-free test flow.
func resolveTestExecution(info *agentv0.AgentInformation, request *runtimev0.TestRequest) (testExecution, error) {
	if request == nil {
		request = &runtimev0.TestRequest{}
	} else {
		request = proto.Clone(request).(*runtimev0.TestRequest)
	}
	legacy := info == nil || info.GetValidation() == nil
	if legacy {
		return testExecution{
			Request:        request,
			DependencyMode: agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE,
			legacy:         true,
		}, nil
	}

	test := info.GetValidation().GetTest()
	if !test.GetSupported() {
		return testExecution{}, fmt.Errorf("test is explicitly unsupported by the agent validation contract")
	}
	requested := strings.TrimSpace(request.GetSuite())
	var selected *agentv0.TestSuiteCapability
	if requested != "" {
		for _, suite := range test.GetSuites() {
			if suite.GetName() == requested {
				selected = suite
				break
			}
		}
		if selected == nil {
			return testExecution{}, fmt.Errorf("suite %q is not advertised (available: %s)", requested, advertisedSuiteNames(test.GetSuites()))
		}
	} else {
		for _, suite := range test.GetSuites() {
			if !suite.GetDefaultSuite() {
				continue
			}
			if selected != nil {
				return testExecution{}, fmt.Errorf("validation contract advertises multiple default test suites")
			}
			selected = suite
		}
		if selected == nil {
			return testExecution{}, fmt.Errorf("validation contract does not advertise a default test suite")
		}
		request.Suite = selected.GetName()
	}

	if strings.TrimSpace(selected.GetName()) == "" {
		return testExecution{}, fmt.Errorf("validation contract advertises an empty test suite name")
	}
	switch selected.GetDependencyMode() {
	case agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE,
		agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES,
		agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK:
	default:
		return testExecution{}, fmt.Errorf("suite %q has incomplete dependency mode %s", selected.GetName(), selected.GetDependencyMode().String())
	}
	return testExecution{Request: request, DependencyMode: selected.GetDependencyMode()}, nil
}

func advertisedSuiteNames(suites []*agentv0.TestSuiteCapability) string {
	names := make([]string, 0, len(suites))
	for _, suite := range suites {
		if name := strings.TrimSpace(suite.GetName()); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
