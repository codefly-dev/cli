package test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	toolingv0 "github.com/codefly-dev/core/generated/go/codefly/services/tooling/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type sourceHandshakeAgent struct {
	agentv0.UnimplementedAgentServer
	runtime bool
}

func (s sourceHandshakeAgent) GetAgentInformation(context.Context, *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	info := &agentv0.AgentInformation{}
	if s.runtime {
		info.Capabilities = []*agentv0.Capability{{Type: agentv0.Capability_RUNTIME}}
	}
	return info, nil
}

type sourceHandshakeCode struct {
	codev0.UnimplementedCodeServer
}

func (sourceHandshakeCode) Execute(context.Context, *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	return &codev0.CodeResponse{}, nil
}

type sourceHandshakeTooling struct {
	toolingv0.UnimplementedToolingServer
}

func (sourceHandshakeTooling) GetProjectInfo(context.Context, *toolingv0.GetProjectInfoRequest) (*toolingv0.GetProjectInfoResponse, error) {
	return &toolingv0.GetProjectInfoResponse{}, nil
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

func TestPrepareSourceWorkspaceRejectsFloatingAgentOverride(t *testing.T) {
	for _, spec := range []string{"codefly.dev/go", "codefly.dev/go:latest", "codefly.dev/go:v1.2.3"} {
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
			agentv0.RegisterAgentServer(server, sourceHandshakeAgent{runtime: test.runtime})
			if test.registerCode {
				codev0.RegisterCodeServer(server, sourceHandshakeCode{})
			}
			if test.registerTooling {
				toolingv0.RegisterToolingServer(server, sourceHandshakeTooling{})
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
