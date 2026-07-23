package orchestration

import (
	"context"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func newSyncBuilder(info *agentv0.AgentInformation) *Builder {
	return &Builder{
		instance: &services.Instance{
			Identity: &resources.ServiceIdentity{Module: "saas-starter", Name: "redis"},
			Info:     info,
		},
		world: &World{SyncRequest: &builderv0.SyncRequest{DryRun: true}},
	}
}

func TestBuilderSyncSkipsAgentWithoutSyncCapability(t *testing.T) {
	b := newSyncBuilder(nil)

	output, err := b.Sync(context.Background())
	require.NoError(t, err)
	require.Nil(t, output)
	require.True(t, b.SyncSkipped())
}

func TestBuilderSyncHardErrorsWhenSyncAdvertisedButUnsupported(t *testing.T) {
	b := newSyncBuilder(&agentv0.AgentInformation{
		Validation: &agentv0.ValidationCapabilities{
			Sync: &agentv0.ValidationOperationCapability{Supported: false},
		},
	})

	output, err := b.Sync(context.Background())
	require.Error(t, err)
	require.Nil(t, output)
	require.False(t, b.SyncSkipped())
}
