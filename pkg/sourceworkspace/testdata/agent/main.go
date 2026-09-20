package main

import (
	"context"
	"os"
	"time"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type discoveryAgent struct {
	agentv0.UnimplementedAgentServer
}

func (*discoveryAgent) GetAgentInformation(context.Context, *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	advertised := contract.Current()
	switch os.Getenv("TEST_SOURCE_CONTRACT") {
	case "future":
		advertised.ProtocolVersion++
	case "undeclared":
		advertised.ProtocolVersion = 0
	case "unavailable":
		return nil, status.Error(codes.Unavailable, "discovery unavailable")
	}
	var capabilities []*agentv0.Capability
	if os.Getenv("TEST_SOURCE_NO_BUILDER") == "" {
		capabilities = []*agentv0.Capability{{Type: agentv0.Capability_BUILDER}}
	}
	return &agentv0.AgentInformation{
		Capabilities: capabilities,
		Contract:     advertised,
		Languages:    []*agentv0.Language{{Type: agentv0.Language_GO}},
		Validation: &agentv0.ValidationCapabilities{
			SourcePackage: &agentv0.ValidationOperationCapability{Supported: true},
		},
	}, nil
}

type discoveryBuilder struct {
	builderv0.UnimplementedBuilderServer
}

func (*discoveryBuilder) Load(context.Context, *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	if marker := os.Getenv("TEST_SOURCE_BUILDER_LOADED"); marker != "" {
		if err := os.WriteFile(marker, []byte("loaded"), 0o600); err != nil {
			return nil, err
		}
	}
	return &builderv0.LoadResponse{State: &builderv0.LoadStatus{State: builderv0.LoadStatus_READY}}, nil
}

func main() {
	if delay := os.Getenv("TEST_SOURCE_STARTUP_DELAY"); delay != "" {
		duration, err := time.ParseDuration(delay)
		if err != nil {
			panic(err)
		}
		time.Sleep(duration)
	}
	agents.Serve(agents.PluginRegistration{Agent: &discoveryAgent{}, Builder: &discoveryBuilder{}})
}
