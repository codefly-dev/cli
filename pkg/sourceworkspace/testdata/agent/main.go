package main

import (
	"context"
	"os"
	"time"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/contract"
	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type discoveryAgent struct {
	agentv1.UnimplementedAgentServer
}

func (*discoveryAgent) GetAgentInformation(context.Context, *agentv1.AgentInformationRequest) (*agentv1.AgentInformation, error) {
	advertised := contract.Current()
	switch os.Getenv("TEST_SOURCE_CONTRACT") {
	case "future":
		advertised.ProtocolVersion++
	case "undeclared":
		advertised.ProtocolVersion = 0
	case "unavailable":
		return nil, status.Error(codes.Unavailable, "discovery unavailable")
	}
	var capabilities []*agentv1.Capability
	if os.Getenv("TEST_SOURCE_NO_BUILDER") == "" {
		capabilities = []*agentv1.Capability{{Type: agentv1.Capability_BUILDER}}
	}
	validation := &agentv1.ValidationCapabilities{
		SourcePackage: &agentv1.ValidationOperationCapability{Supported: true},
	}
	if os.Getenv("TEST_SOURCE_CI_RECORD") != "" {
		capabilities = append(capabilities, &agentv1.Capability{Type: agentv1.Capability_RUNTIME})
		validation.Test = &agentv1.TestValidationCapability{Supported: true, Suites: []*agentv1.TestSuiteCapability{{Name: "source", DefaultSuite: true, DependencyMode: agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE}}}
	}
	return &agentv1.AgentInformation{
		Capabilities: capabilities,
		Contract:     advertised,
		Languages:    []*agentv1.Language{{Type: agentv1.Language_GO}},
		Validation:   validation,
	}, nil
}

type discoveryBuilder struct {
	builderv1.UnimplementedBuilderServer
}

func (*discoveryBuilder) Load(context.Context, *builderv1.LoadRequest) (*builderv1.LoadResponse, error) {
	if marker := os.Getenv("TEST_SOURCE_BUILDER_LOADED"); marker != "" {
		if err := os.WriteFile(marker, []byte("loaded"), 0o600); err != nil {
			return nil, err
		}
	}
	return &builderv1.LoadResponse{State: &builderv1.LoadStatus{State: builderv1.LoadStatus_READY}}, nil
}

func main() {
	if marker := os.Getenv("TEST_SOURCE_STARTED"); marker != "" {
		if err := os.WriteFile(marker, []byte("started"), 0o600); err != nil {
			panic(err)
		}
	}
	if delay := os.Getenv("TEST_SOURCE_STARTUP_DELAY"); delay != "" {
		duration, err := time.ParseDuration(delay)
		if err != nil {
			panic(err)
		}
		time.Sleep(duration)
	}
	agents.Serve(agents.PluginRegistration{Agent: &discoveryAgent{}, Builder: &discoveryBuilder{}, Runtime: &ciRuntime{}})
}
