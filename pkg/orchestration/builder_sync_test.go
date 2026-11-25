package orchestration

import (
	"context"
	"testing"

	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func newSyncBuilder(info *agentv1.AgentInformation) *Builder {
	return &Builder{
		instance: &services.Instance{
			Identity: &resources.ServiceIdentity{Module: "starterkit", Name: "redis"},
			Info:     info,
		},
		world: &World{SyncRequest: &builderv1.SyncRequest{DryRun: true}},
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
	b := newSyncBuilder(&agentv1.AgentInformation{
		Validation: &agentv1.ValidationCapabilities{
			Sync: &agentv1.ValidationOperationCapability{Supported: false},
		},
	})

	output, err := b.Sync(context.Background())
	require.Error(t, err)
	require.Nil(t, output)
	require.False(t, b.SyncSkipped())
}
