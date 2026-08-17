//go:build integration

package control

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// A workspace with an origin service ("api") that declares a real redis
// dependency. api's own agent is never spawned (Run uses ExcludeRoot), so it
// only needs to be well-formed YAML, not an installed agent.
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
    - name: redis
    - name: managed
`
	runDepAPIServiceYAML = `kind: service
name: api
version: 0.0.0
module: app
agent:
    kind: codefly:service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
service-dependencies:
    - name: redis
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
	runDepRedisServiceYAML = `kind: service
name: redis
version: 0.0.0
module: app
agent:
    kind: codefly:service
    name: redis
    version: 0.0.74
    publisher: codefly.dev
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
    name: redis
    version: 0.0.74
    publisher: codefly.dev
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
		"workspace.codefly.yaml":                            runDepWorkspaceYAML,
		"modules/app/module.codefly.yaml":                   runDepModuleYAML,
		"modules/app/services/api/service.codefly.yaml":     runDepAPIServiceYAML,
		"modules/app/services/redis/service.codefly.yaml":   runDepRedisServiceYAML,
		"modules/app/services/managed/service.codefly.yaml": runDepManagedServiceYAML,
		"configurations/local/local-auth.env":               "TOKEN=local\n",
		"configurations/local/managed-auth.env":             "TOKEN=managed\n",
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

var libpqHostPort = regexp.MustCompile(`\bhost=(\S+)\b.*\bport=(\S+)\b|\bport=(\S+)\b.*\bhost=(\S+)\b`)

// hostPortFromConnectionString extracts host:port from a connection string in
// either URL form (redis://host:port, postgres://user:pass@host:port/db) or
// libpq keyword/value form (host=... port=... ...).
func hostPortFromConnectionString(dsn string) (host, port string, err error) {
	if u, parseErr := url.Parse(dsn); parseErr == nil && u.Host != "" {
		host, port = u.Hostname(), u.Port()
		if host != "" && port != "" {
			return host, port, nil
		}
	}
	if m := libpqHostPort.FindStringSubmatch(dsn); m != nil {
		if m[1] != "" {
			return m[1], m[2], nil
		}
		return m[4], m[3], nil
	}
	return "", "", fmt.Errorf("no host:port found in %q", dsn)
}

func TestHostPortFromConnectionString(t *testing.T) {
	cases := []struct {
		name     string
		dsn      string
		wantHost string
		wantPort string
	}{
		{"redis URL", "redis://host.docker.internal:36780", "host.docker.internal", "36780"},
		{"postgres URL with credentials", "postgres://user:pass@127.0.0.1:5432/db", "127.0.0.1", "5432"},
		{"libpq host-then-port", "host=127.0.0.1 port=5432 user=postgres dbname=postgres", "127.0.0.1", "5432"},
		{"libpq port-then-host", "port=5432 host=127.0.0.1 user=postgres", "127.0.0.1", "5432"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, port, err := hostPortFromConnectionString(c.dsn)
			if err != nil {
				t.Fatalf("hostPortFromConnectionString(%q): %v", c.dsn, err)
			}
			if host != c.wantHost || port != c.wantPort {
				t.Fatalf("hostPortFromConnectionString(%q) = (%q, %q), want (%q, %q)", c.dsn, host, port, c.wantHost, c.wantPort)
			}
		})
	}
}

func TestHostPortFromConnectionStringRejectsUnparseable(t *testing.T) {
	if _, _, err := hostPortFromConnectionString("not-a-connection-string"); err == nil {
		t.Fatal("expected an error for an unparseable connection string")
	}
}

// TestRunProfilesStartRealDependencyShapesInProcess proves that both named
// profiles drive live Codefly agents and project only their selected workspace
// configurations. The connection check preserves the in-process acceptance
// coverage for dependency configuration and teardown.
func TestRunProfilesStartRealDependencyShapesInProcess(t *testing.T) {
	tests := []struct {
		profile            string
		wantDependencies   []string
		wantConfigurations []string
	}{
		{
			profile:            "local",
			wantDependencies:   []string{"redis"},
			wantConfigurations: []string{"local-auth"},
		},
		{
			profile:            "saas",
			wantDependencies:   []string{"managed", "redis"},
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

			// The profile and environment-export contract is backend-neutral. Nix
			// keeps this real-agent proof reproducible without making an unrelated
			// desktop Docker daemon a precondition for the control-plane suite.
			if _, err := plane.Run(ctx, RunRequest{
				Service:        "app/api",
				Profile:        tt.profile,
				RuntimeContext: resources.RuntimeContextNix,
				ExcludeRoot:    true,
				Wait:           true,
				OutputEnv:      outputEnvironment,
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			defer func() {
				if err := plane.Stop(context.Background(), StopRequest{Destroy: true}); err != nil {
					t.Errorf("Stop: %v", err)
				}
			}()

			flow := activeRunFlow(t, plane)
			_, dependencies := flow.ManagedServices()
			sort.Strings(dependencies)
			if fmt.Sprint(dependencies) != fmt.Sprint(tt.wantDependencies) {
				t.Fatalf("started dependencies = %v, want %v", dependencies, tt.wantDependencies)
			}
			for _, dependency := range dependencies {
				if !flow.ServiceReachable("app/" + dependency) {
					t.Fatalf("dependency app/%s is not reachable", dependency)
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
				"CODEFLY__ENDPOINT__APP__REDIS__TCP__TCP=",
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

			dsn := connectionStringFrom(configs)
			if dsn == "" {
				t.Fatalf("no dependency connection string in configurations: %+v", configs)
			}

			host, port, err := hostPortFromConnectionString(dsn)
			if err != nil {
				t.Fatalf("parse connection string %q: %v", dsn, err)
			}
			if host == "host.docker.internal" {
				host = "127.0.0.1"
			}
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 10*time.Second)
			if err != nil {
				t.Fatalf("dial resolved dependency address %s:%s: %v", host, port, err)
			}
			_ = conn.Close()
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
