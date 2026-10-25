package cmd

import (
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestDoctorReportsNativeFederationWithoutRequestingAFile(t *testing.T) {
	workspace := testWorkspaceYAML + `environments:
    - name: local
      host:
          coordinate: owner/local/host
          component: backend
          domain: owner
          audience: host
          trust_domain: host.local
          delivery: backend/api/grpc
`
	for _, delivery := range []string{"backend/api/grpc", "other/api/grpc"} {
		t.Run(delivery, func(t *testing.T) {
			dir := singleServiceWorkspace(t, strings.ReplaceAll(workspace, "backend/api/grpc", delivery), []string{"federation"}, nil)
			report := runReadiness(t, workspaceReadinessOptions{dir: dir})
			if delivery == "other/api/grpc" {
				requireCode(t, report, codeConfigurationMissing, "fail")
				return
			}
			requireNoCode(t, report, codeConfigurationMissing)
			requireNoCode(t, report, codeConfigurationDirMissing)
			require.Contains(t, reportJSON(t, report), "container runs and deployments still require authored federation")
		})
	}
}

func TestDoctorNativeFederationDoesNotCoverAnotherConsumer(t *testing.T) {
	host := &resources.Service{Name: "api", WorkspaceConfigurationDependencies: []string{"federation"}}
	host.WithModule("backend")
	other := &resources.Service{Name: "worker", WorkspaceConfigurationDependencies: []string{"federation"}}
	other.WithModule("backend")
	env := &environments.Environment{Host: &environments.EnvironmentHost{Coordinate: "owner/local/host", Domain: "owner", Delivery: "backend/api/grpc"}}
	require.True(t, nativeFederationCoversScope(env, []*resources.Service{host}))
	require.False(t, nativeFederationCoversScope(env, []*resources.Service{host, other}))
	require.False(t, nativeFederationCoversScope(env, nil))
}
