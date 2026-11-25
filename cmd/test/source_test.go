package test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/contract"
	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
	codev1 "github.com/codefly-dev/core/generated/go/codefly/services/code/v1"
	toolingv1 "github.com/codefly-dev/core/generated/go/codefly/services/tooling/v1"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type sourceHandshakeAgent struct {
	agentv1.UnimplementedAgentServer
	runtime bool
}

func (s sourceHandshakeAgent) GetAgentInformation(context.Context, *agentv1.AgentInformationRequest) (*agentv1.AgentInformation, error) {
	info := &agentv1.AgentInformation{Contract: contract.Current()}
	if s.runtime {
		info.Capabilities = []*agentv1.Capability{{Type: agentv1.Capability_RUNTIME}}
	}
	return info, nil
}

type sourceHandshakeCode struct {
	codev1.UnimplementedCodeServer
}

func (sourceHandshakeCode) Execute(context.Context, *codev1.CodeRequest) (*codev1.CodeResponse, error) {
	return &codev1.CodeResponse{}, nil
}

type sourceHandshakeTooling struct {
	toolingv1.UnimplementedToolingServer
}

func (sourceHandshakeTooling) GetProjectInfo(context.Context, *toolingv1.GetProjectInfoRequest) (*toolingv1.GetProjectInfoResponse, error) {
	return &toolingv1.GetProjectInfoResponse{}, nil
}

func TestPrepareSourceWorkspaceUsesExactAgentOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareSourceWorkspace(context.Background(), dir, "codefly.dev/go:9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if got := prepared.Service.Agent; got.Publisher != "codefly.dev" || got.Name != "go" || got.Version != "9.9.9" {
		t.Fatalf("source agent = %+v, want exact codefly.dev/go:9.9.9", got)
	}
}

func TestPrepareSourceWorkspaceAcceptsFloatingAgentOverride(t *testing.T) {
	for _, spec := range []string{"example.test/unknown", "example.test/unknown:latest"} {
		t.Run(spec, func(t *testing.T) {
			prepared, err := prepareSourceWorkspace(t.Context(), t.TempDir(), spec)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if prepared.Service.Agent.Version != "latest" {
				t.Fatal("floating selection was statically pinned")
			}
		})
	}
}

func TestPrepareSourceWorkspaceRejectsNoncanonicalAgentOverride(t *testing.T) {
	for _, spec := range []string{"codefly.dev/go:v1.2.3"} {
		t.Run(strings.ReplaceAll(spec, "/", "_"), func(t *testing.T) {
			if _, err := prepareSourceWorkspace(context.Background(), t.TempDir(), spec); err == nil {
				t.Fatalf("agent override %q was accepted", spec)
			}
		})
	}
}

func TestVerifySourceCapabilityClientsRequiresRuntimeCodeAndTooling(t *testing.T) {
	tests := []struct {
		name            string
		runtime         bool
		registerCode    bool
		registerTooling bool
		wantError       string
	}{
		{name: "complete handshake", runtime: true, registerCode: true, registerTooling: true},
		{name: "runtime missing", registerCode: true, registerTooling: true, wantError: "Runtime capability"},
		{name: "code missing", runtime: true, registerTooling: true, wantError: "Code capability handshake"},
		{name: "tooling missing", runtime: true, registerCode: true, wantError: "Tooling capability handshake"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			listener := bufconn.Listen(1024 * 1024)
			server := grpc.NewServer()
			agentv1.RegisterAgentServer(server, sourceHandshakeAgent{runtime: test.runtime})
			if test.registerCode {
				codev1.RegisterCodeServer(server, sourceHandshakeCode{})
			}
			if test.registerTooling {
				toolingv1.RegisterToolingServer(server, sourceHandshakeTooling{})
			}
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()

			connection, err := grpc.NewClient("passthrough:///source-handshake",
				grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			agent := &resources.Agent{Publisher: "codefly.dev", Name: "fixture", Version: "1.2.3"}
			err = verifySourceCapabilityClients(context.Background(), agent, connection)
			if test.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("handshake error = %v, want %q", err, test.wantError)
			}
		})
	}
}
