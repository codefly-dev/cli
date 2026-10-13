package environments

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestHostGatewayShapeAndRoundTrip(t *testing.T) {
	for _, gateway := range []string{"platform/gateway", "some.module/api-v2", "a_b/c.d"} {
		var host EnvironmentHost
		require.NoError(t, yaml.Unmarshal([]byte(hostBlock+"      gateway: "+gateway+"\n"), &host))
		require.NoError(t, host.Validate())
		encoded, err := yaml.Marshal(host)
		require.NoError(t, err)
		var reloaded EnvironmentHost
		require.NoError(t, yaml.Unmarshal(encoded, &reloaded))
		require.Equal(t, gateway, reloaded.Gateway)
	}
	for _, gateway := range []string{"gateway", "platform/gateway/rest", "https://gateway.example", "Platform/gateway", "platform/../gateway", " platform/gateway"} {
		var host EnvironmentHost
		require.NoError(t, yaml.Unmarshal([]byte(hostBlock), &host))
		host.Gateway = gateway
		require.ErrorContains(t, host.Validate(), "host gateway")
	}
	for _, domain := range []string{"", strings.Repeat("x", 256), "UPPER", "a/b", "a:123", "a\n--evil"} {
		require.Error(t, ValidateTrustDomain(domain))
	}
}

func identityWorkspace(t *testing.T, gateway string, declares bool) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	block := hostBlock
	if gateway != "" {
		block += "      gateway: " + gateway + "\n"
	}
	service := "name: api\nagent:\n  name: go\n  publisher: codefly.dev\n  version: 0.0.0\n"
	if declares {
		service += "workspace-configuration-dependencies:\n  - workload-identity\n"
	}
	files := map[string]string{
		resources.WorkspaceConfigurationName:                             "name: example\nlayout: modules\nmodules:\n  - name: app\nenvironments:\n  - name: prod\n    namespace: example\n    host:\n" + block,
		"modules/app/" + resources.ModuleConfigurationName:               "kind: module\nname: app\nservices:\n  - name: api\n",
		"modules/app/services/api/" + resources.ServiceConfigurationName: service,
	}
	for name, content := range files {
		file := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
		require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), root)
	require.NoError(t, err)
	return workspace
}

func TestHostGatewayRequiredByExplicitConsumersAndMustExist(t *testing.T) {
	for _, tc := range []struct {
		name, gateway, want string
		declares            bool
	}{
		{"required", "", "requires host.gateway", true},
		{"unknown", "app/missing", "no composed service", true},
		{"foreign module", "missing/api", "no composed service", true},
		{"valid", "app/api", "", true},
		{"not needed", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := identityWorkspace(t, tc.gateway, tc.declares)
			err := ValidateWorkspace(t.Context(), workspace)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestModuleAuthorityRequiresWorkloadIdentityAndHostAudience(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity bool
		audience string
		want     string
	}{
		{"missing identity", false, "mint", "must also declare workload-identity"},
		{"missing audience", true, "", "requires host.audience"},
		{"valid", true, "mint", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := identityWorkspace(t, "app/api", tc.identity)
			path := filepath.Join(workspace.Dir(), "modules/app/services/api", resources.ServiceConfigurationName)
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			if !tc.identity {
				body = append(body, []byte("workspace-configuration-dependencies:\n")...)
			}
			body = append(body, []byte("  - module-authority\n")...)
			require.NoError(t, os.WriteFile(path, body, 0o600))
			workspace, err = resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
			require.NoError(t, err)
			env, err := Select(workspace, "prod")
			require.NoError(t, err)
			env.Host.Audience = tc.audience
			_, err = WorkloadIdentityServices(t.Context(), workspace, env)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}
