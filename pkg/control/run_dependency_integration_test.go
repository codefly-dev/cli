//go:build integration

package control

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/conformance/conformancetest"
	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The host selects test-only protocol peers from a disposable profile. No
// released agent, provider implementation or toolchain participates here.
const (
	runDepWorkspaceYAML = `name: rundep
layout: modules
modules:
    - name: app
run-profiles:
    local:
        exclude-dependencies:
            - app/managed
        exclude-workspace-configurations:
            - managed-auth
    saas: {}
`
	runDepModuleYAML = `kind: module
name: app
services:
    - name: api
    - name: dependency
    - name: managed
`
	runDepAPIServiceYAML = `kind: service
name: api
version: 0.0.0
module: app
agent:
    kind: codefly:service
    name: unselected-root
    version: 0.0.1
    publisher: example.test
service-dependencies:
    - name: dependency
      module: app
      endpoints:
          - name: tcp
    - name: managed
      module: app
      endpoints:
          - name: tcp
workspace-configuration-dependencies:
    - local-auth
    - managed-auth
`
	runDepServiceYAML = `kind: service
name: dependency
version: 0.0.0
module: app
agent:
    kind: codefly:service
    name: dependency-peer
    version: 0.0.1
    publisher: example.test
endpoints:
    - name: tcp
workspace-configuration-dependencies:
    - local-auth
    - managed-auth
`
	runDepManagedServiceYAML = `kind: service
name: managed
version: 0.0.0
module: app
agent:
    kind: codefly:service
    name: dependency-peer
    version: 0.0.1
    publisher: example.test
endpoints:
    - name: tcp
workspace-configuration-dependencies:
    - local-auth
    - managed-auth
`
)

// writeRunDependencyWorkspace lays the fixture on disk and returns its root.
func writeRunDependencyWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                               runDepWorkspaceYAML,
		"modules/app/module.codefly.yaml":                      runDepModuleYAML,
		"modules/app/services/api/service.codefly.yaml":        runDepAPIServiceYAML,
		"modules/app/services/dependency/service.codefly.yaml": runDepServiceYAML,
		"modules/app/services/managed/service.codefly.yaml":    runDepManagedServiceYAML,
		"configurations/local/local-auth.env":                  "TOKEN=local\n",
		"configurations/local/managed-auth.env":                "TOKEN=managed\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func workspaceConfigurationNames(configs []*basev0.Configuration) []string {
	var names []string
	for _, configuration := range configs {
		if configuration.GetOrigin() != resources.ConfigurationWorkspace {
			continue
		}
		for _, info := range configuration.GetInfos() {
			names = append(names, info.GetName())
		}
	}
	sort.Strings(names)
	return names
}

func activeRunFlow(t *testing.T, plane Plane) *orchestration.Flow {
	t.Helper()
	implementation, ok := plane.(*planeImpl)
	if !ok {
		t.Fatalf("plane type = %T, want *planeImpl", plane)
	}
	_, managed := implementation.host.Flows().Active()
	flow, ok := managed.(*orchestration.Flow)
	if !ok {
		t.Fatalf("active flow type = %T, want *orchestration.Flow", managed)
	}
	return flow
}

// connectionStringFrom extracts the "connection" value a dependency agent
// publishes, the standard codefly key for a dependency's connection string
// (see core's sdk.WithDependencies).
func connectionStringFrom(configs []*basev0.Configuration) string {
	for _, conf := range configs {
		for _, info := range conf.GetInfos() {
			for _, val := range info.GetConfigurationValues() {
				if val.GetKey() == "connection" {
					return val.GetValue()
				}
			}
		}
	}
	return ""
}

// The CLI must project only the selected profile and honor the peer's accepted
// endpoint addresses. This proves host behavior, not any provider runtime.
func TestRunProfilesThroughProtocolPeers(t *testing.T) {
	conformancetest.Gate(t, "linux-amd64-native-control", "go")
	protocoltest.Install(t, "dependency-peer")
	tests := []struct {
		profile            string
		wantDependencies   []string
		wantConfigurations []string
	}{
		{
			profile:            "local",
			wantDependencies:   []string{"app/dependency"},
			wantConfigurations: []string{"local-auth"},
		},
		{
			profile:            "saas",
			wantDependencies:   []string{"app/dependency", "app/managed"},
			wantConfigurations: []string{"local-auth", "managed-auth"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			root := writeRunDependencyWorkspace(t)
			outputEnvironment := filepath.Join(t.TempDir(), "runtime.env")
			plane, err := NewAt(root)
			if err != nil {
				t.Fatalf("NewAt: %v", err)
			}
			defer func() {
				if err := plane.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()

			// Exercise the host lifecycle and accepted mappings, not a provider's
			// installation or runtime packaging behavior.
			if _, err := plane.Run(ctx, RunRequest{
				Service:        "app/api",
				Profile:        tt.profile,
				RuntimeContext: resources.RuntimeContextNative,
				ExcludeRoot:    true,
				Wait:           true,
				OutputEnv:      outputEnvironment,
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			defer func() {
				if _, err := plane.Stop(context.Background(), StopRequest{Destroy: true}); err != nil {
					t.Errorf("Stop: %v", err)
				}
			}()

			flow := activeRunFlow(t, plane)
			_, dependencies := flow.ManagedServices()
			sort.Strings(dependencies)
			if fmt.Sprint(dependencies) != fmt.Sprint(tt.wantDependencies) {
				t.Fatalf("started dependencies = %v, want %v", dependencies, tt.wantDependencies)
			}
			// ManagedServices reports module-qualified uniques, which is the
			// identity ServiceReachable takes: a bare service name cannot name a
			// service, since two composed modules may each declare one by that
			// name.
			for _, dependency := range dependencies {
				if !flow.ServiceReachable(ctx, dependency) {
					t.Fatalf("dependency %s is not reachable", dependency)
				}
			}

			configs, err := plane.Configurations(ctx, "app/api")
			if err != nil {
				t.Fatalf("Configurations: %v", err)
			}
			if got := workspaceConfigurationNames(configs); fmt.Sprint(got) != fmt.Sprint(tt.wantConfigurations) {
				t.Fatalf("workspace configurations = %v, want %v", got, tt.wantConfigurations)
			}

			body, err := os.ReadFile(outputEnvironment)
			if err != nil {
				t.Fatalf("read excluded-root output environment: %v", err)
			}
			environment := string(body)
			for _, expected := range []string{
				"CODEFLY__MODULE=app\n",
				"CODEFLY__SERVICE=api\n",
				"CODEFLY__ENDPOINT__APP__DEPENDENCY__TCP__TCP=",
				"CODEFLY__WORKSPACE_CONFIGURATION__LOCAL_AUTH__TOKEN=local\n",
			} {
				if !strings.Contains(environment, expected) {
					t.Fatalf("excluded-root environment is missing %q; keys=%v", expected, outputEnvironmentKeys(environment))
				}
			}
			if tt.profile == "local" && strings.Contains(environment, "MANAGED_AUTH") {
				t.Fatalf("excluded workspace configuration leaked into output; keys=%v", outputEnvironmentKeys(environment))
			}
			info, err := os.Stat(outputEnvironment)
			if err != nil {
				t.Fatalf("stat excluded-root output environment: %v", err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("excluded-root output environment mode = %o, want 600", got)
			}

			var addresses []string
			for _, dependency := range tt.wantDependencies {
				var dsn string
				for _, config := range configs {
					if config.GetOrigin() == dependency {
						dsn = connectionStringFrom([]*basev0.Configuration{config})
					}
				}
				address, err := url.Parse(dsn)
				require.NoError(t, err)
				require.Equal(t, "tcp", address.Scheme)
				conn, err := net.DialTimeout("tcp", address.Host, 10*time.Second)
				require.NoError(t, err)
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
				line, err := bufio.NewReader(conn).ReadString('\n')
				require.NoError(t, conn.Close())
				require.NoError(t, err)
				require.Equal(t, dependency+"\n", line)
				addresses = append(addresses, address.Host)
			}
			_, err = plane.Stop(ctx, StopRequest{Destroy: true})
			require.NoError(t, err)
			for _, address := range addresses {
				conn, err := net.DialTimeout("tcp", address, time.Second)
				if conn != nil {
					_ = conn.Close()
				}
				require.Error(t, err, "stopped dependency must not keep listening")
			}
			for _, dependency := range tt.wantDependencies {
				calls := protocoltest.Calls(t, filepath.Join(root, "modules", "app", "services", strings.TrimPrefix(dependency, "app/")))
				var lifecycle []string
				for _, call := range calls {
					if strings.HasPrefix(call.Method, "Runtime.") {
						lifecycle = append(lifecycle, call.Method)
					}
				}
				require.Subset(t, lifecycle, []string{"Runtime.Load", "Runtime.Init", "Runtime.Start", "Runtime.Stop", "Runtime.Destroy"})
				require.Equal(t, []string{"Runtime.Load", "Runtime.Init", "Runtime.Start"}, lifecycle[:3])
			}
		})
	}
	for _, declaration := range []string{"missing", "future"} {
		t.Run("reject-"+declaration, func(t *testing.T) {
			t.Setenv("CODEFLY_TEST_PEER_CONTRACT", declaration)
			root := writeRunDependencyWorkspace(t)
			plane, err := NewAt(root)
			require.NoError(t, err)
			defer plane.Close()
			output := filepath.Join(t.TempDir(), "runtime.env")
			_, err = plane.Run(t.Context(), RunRequest{Service: "app/api", Profile: "local", RuntimeContext: resources.RuntimeContextNative, ExcludeRoot: true, Wait: true, OutputEnv: output})
			require.ErrorContains(t, err, "incompatible agent contract")
			_, err = os.Stat(output)
			require.ErrorIs(t, err, os.ErrNotExist)
			_, err = os.Stat(filepath.Join(root, "modules/app/services/dependency/.protocol-test"))
			require.ErrorIs(t, err, os.ErrNotExist, "rejected peer must not receive Runtime.Load")
		})
	}
}

func outputEnvironmentKeys(body string) []string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	keys := make([]string, 0, len(lines))
	for _, line := range lines {
		key, _, _ := strings.Cut(line, "=")
		keys = append(keys, key)
	}
	return keys
}
