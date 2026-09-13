package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestParseInstallAgent(t *testing.T) {
	tests := []struct {
		name          string
		specification string
		override      string
		kind          string
		wantPublisher string
		wantName      string
		wantVersion   string
		wantKind      resources.AgentKind
		wantErr       bool
	}{
		{name: "latest", specification: "go-grpc", wantPublisher: "codefly.dev", wantName: "go-grpc", wantVersion: "latest"},
		{name: "pinned", specification: "codefly.dev/postgres:v1.2.3", wantPublisher: "codefly.dev", wantName: "postgres", wantVersion: "1.2.3"},
		{name: "override", specification: "redis:1.0.0", override: "2.0.0-rc.1", wantPublisher: "codefly.dev", wantName: "redis", wantVersion: "2.0.0-rc.1"},
		{name: "traversal", specification: "../redis:1.0.0", wantErr: true},
		{name: "bad version", specification: "redis:not-semver", wantErr: true},
		{name: "runnable", specification: "python:0.0.1", kind: "runnable", wantPublisher: "codefly.dev", wantName: "python", wantVersion: "0.0.1", wantKind: resources.RunnableAgent},
		{name: "registered kind form", specification: "python:0.0.1", kind: "codefly:runnable", wantPublisher: "codefly.dev", wantName: "python", wantVersion: "0.0.1", wantKind: resources.RunnableAgent},
		{name: "unknown kind", specification: "python:0.0.1", kind: "lambda", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent, err := parseInstallAgent(context.Background(), tc.specification, tc.override, tc.kind)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseInstallAgent(%q) unexpectedly succeeded: %+v", tc.specification, agent)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if agent.Publisher != tc.wantPublisher || agent.Name != tc.wantName || agent.Version != tc.wantVersion {
				t.Fatalf("parseInstallAgent(%q) = %+v", tc.specification, agent)
			}
			wantKind := tc.wantKind
			if wantKind == "" {
				wantKind = resources.ServiceAgent
			}
			if agent.Kind != wantKind {
				t.Fatalf("parseInstallAgent(%q) kind = %q, want %q", tc.specification, agent.Kind, wantKind)
			}
		})
	}
}

func TestInstallCommandReturnsValidationError(t *testing.T) {
	previous := installVersion
	installVersion = ""
	t.Cleanup(func() { installVersion = previous })

	err := InstallCmd.RunE(InstallCmd, []string{"../redis:1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "invalid agent") {
		t.Fatalf("validation error = %v", err)
	}
}
