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

// hostWorkspace composes one module with one service, under a host block that
// names gateway when it is not empty; the service declares module-authority
// when declares is true.
func hostWorkspace(t *testing.T, gateway string, declares bool) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	block := hostBlock
	if gateway != "" {
		block += "      gateway: " + gateway + "\n"
	}
	block += "      module_audience: module-capabilities\n"
	service := "name: api\nagent:\n  name: go\n  publisher: codefly.dev\n  version: 0.0.0\n"
	if declares {
		service += "workspace-configuration-dependencies:\n  - module-authority\n"
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

// A host.gateway names a composed service with a workload of its own; no
// service opts into anything for it to be named, and a composition without one
// is valid.
func TestHostGatewayMustNameAComposedService(t *testing.T) {
	for _, tc := range []struct {
		name, gateway, want string
	}{
		{"unknown", "app/missing", "no composed service"},
		{"foreign module", "missing/api", "no composed service"},
		{"valid", "app/api", ""},
		{"absent", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := hostWorkspace(t, tc.gateway, false)
			err := ValidateWorkspace(t.Context(), workspace)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// A service declaring module-authority needs the two audiences the render
// projects into the group, and nothing else: the principal it is handed is
// the mesh identity of its own ServiceAccount, which it never opts into.
func TestModuleAuthorityRequiresTheHostAudiences(t *testing.T) {
	for _, tc := range []struct {
		name, audience, moduleAudience, want string
	}{
		{"missing audience", "", "module-capabilities", "requires host.audience"},
		{"missing module audience", "mint", "", "requires host.module_audience"},
		{"valid", "mint", "module-capabilities", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := hostWorkspace(t, "app/api", true)
			env, err := Select(workspace, "prod")
			require.NoError(t, err)
			env.Host.Audience, env.Host.ModuleAudience = tc.audience, tc.moduleAudience
			err = ValidateHostComposition(t.Context(), workspace, env)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}
