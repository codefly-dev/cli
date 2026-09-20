package main

import (
	"context"
	"os"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
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
	}
	return &agentv0.AgentInformation{
		Contract:  advertised,
		Languages: []*agentv0.Language{{Type: agentv0.Language_GO}},
		Validation: &agentv0.ValidationCapabilities{
			SourcePackage: &agentv0.ValidationOperationCapability{Supported: true},
		},
	}, nil
}

func main() {
	agents.Serve(agents.PluginRegistration{Agent: &discoveryAgent{}})
}
