package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/contract"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	"github.com/codefly-dev/core/resources"
)

type usageAgent struct {
	agentv0.UnimplementedAgentServer
	usage string
}

func (a *usageAgent) GetAgentInformation(context.Context, *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	return &agentv0.AgentInformation{Contract: contract.Current(), UsageContract: a.usage}, nil
}

type usageCode struct {
	codev0.UnimplementedCodeServer
}

func (*usageCode) Execute(_ context.Context, _ *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	if _, err := os.Stat(filepath.Join(os.Getenv(agents.WorkDirEnvironment), resources.ServiceConfigurationName)); err != nil {
		return nil, err
	}
	return &codev0.CodeResponse{Result: &codev0.CodeResponse_GetSemanticIndex{
		GetSemanticIndex: &basev0.SemanticIndex{State: basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_COMPLETE},
	}}, nil
}
