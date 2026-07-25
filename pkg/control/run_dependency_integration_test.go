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
	"testing"
	"time"

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
`
	runDepModuleYAML = `kind: module
name: app
services:
    - name: api
    - name: redis
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
`
)

// writeRunDependencyWorkspace lays the fixture on disk and returns its root.
func writeRunDependencyWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                          runDepWorkspaceYAML,
		"modules/app/module.codefly.yaml":                 runDepModuleYAML,
		"modules/app/services/api/service.codefly.yaml":   runDepAPIServiceYAML,
		"modules/app/services/redis/service.codefly.yaml": runDepRedisServiceYAML,
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

// TestRunExcludeRootStartsRealDependencyInProcess is the acceptance proof for
// #126: an embedder starts a real dependency flow in-process — no `codefly`
// subprocess, no gRPC control channel, no os.Setenv round-trip — reads the
// started dependency's connection string via Configurations, connects to it
// for real, and tears down with no leaked containers or agent processes.
func TestRunExcludeRootStartsRealDependencyInProcess(t *testing.T) {
	root := writeRunDependencyWorkspace(t)

	plane, err := NewAt(root)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	defer func() {
		if err := plane.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, err := plane.Run(ctx, RunRequest{Service: "app/api", RuntimeContext: resources.RuntimeContextContainer, ExcludeRoot: true, Wait: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		if err := plane.Stop(context.Background(), StopRequest{Destroy: true}); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	configs, err := plane.Configurations(ctx, "app/api")
	if err != nil {
		t.Fatalf("Configurations: %v", err)
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
		// The published DSN is meant for a sibling container, which resolves
		// that name via Docker's embedded DNS. The test itself runs on the
		// host, which has no such entry — but the published port is bound to
		// every host interface, so the loopback address reaches it directly.
		host = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 10*time.Second)
	if err != nil {
		t.Fatalf("dial resolved dependency address %s:%s: %v", host, port, err)
	}
	_ = conn.Close()
}
