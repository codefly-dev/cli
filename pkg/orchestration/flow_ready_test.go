package orchestration

import (
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestFormatServiceRunPlanEntryIncludesServiceAndAgentVersions(t *testing.T) {
	service := &resources.Service{
		Name:    "api",
		Version: "0.0.7",
		Agent: &resources.Agent{
			Kind:      resources.ServiceAgent,
			Publisher: "codefly.dev",
			Name:      "go-grpc",
			Version:   "latest",
		},
	}

	got := formatServiceRunPlanEntry("users/api", service, nil)
	require.Equal(t, "users/api@0.0.7 via codefly.dev/go-grpc:latest", got)
}

func nativeTestInstance(host string) *basev0.NetworkInstance {
	return &basev0.NetworkInstance{
		Access: &basev0.NetworkAccess{Kind: resources.NetworkAccessNative},
		Host:   host,
	}
}
