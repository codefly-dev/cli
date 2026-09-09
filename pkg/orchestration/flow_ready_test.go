package orchestration

import (
	"testing"

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
