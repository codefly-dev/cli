package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
)

// Inspection must not initialize Runtime; the first mutation must. Controlled
// typed responses test host lifecycle decisions without a released plugin.
func TestReadOnlyCodeAndCommandsRunWithoutRuntimeInitialization(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, root, "pyproject.toml", "[project]\nname = \"probe\"\nversion = \"0.0.0\"\n")
	writeSourceFile(t, root, "broken.py", "def oops(:\n    return\n")

	agent := protocoltest.Install(t, "inspection-peer")[0]
	protocoltest.Response(t, root, "project", &codev0.CodeResponse{Result: &codev0.CodeResponse_GetProjectInfo{GetProjectInfo: &codev0.GetProjectInfoResponse{Module: "probe", Language: "opaque-language"}}})

	host, err := NewWorkspaceHost(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })

	service, err := host.Service(ServiceTarget{Root: root, Agent: agent, ForceSource: true})
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()

	project, err := service.ExecuteCode(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GetProjectInfo{GetProjectInfo: &codev0.GetProjectInfoRequest{}},
	})
	if err != nil {
		t.Fatalf("GetProjectInfo transport error: %v", err)
	}
	info := project.GetGetProjectInfo()
	if info.GetModule() != "probe" || info.GetLanguage() != "opaque-language" {
		t.Fatalf("project info response was not preserved: %+v", info)
	}

	// Agent-level command discovery and execution are not part of the runtime
	// lifecycle either; they must succeed against the started agent alone.
	if _, err := service.ListCommands(ctx, &agentv0.ListCommandsRequest{}); err != nil {
		t.Fatalf("ListCommands transport error: %v", err)
	}
	if _, err := service.RunCommand(ctx, &agentv0.RunPluginCommandRequest{Command: "bash", Args: []string{"echo", "ok"}}); err != nil {
		t.Fatalf("RunCommand transport error: %v", err)
	}

	// The single decoupling invariant: read-only inspection and agent-level
	// commands started the agent but never ran Runtime Load/Init.
	session, err := service.supervisor.acquire(ctx, service.target)
	if err != nil {
		t.Fatalf("acquire session: %v", err)
	}
	if session.runtimeOK {
		t.Fatal("read-only Code/Tooling calls must not initialize the runtime")
	}

	// A mutating Code operation acts on the runtime-managed workspace, so it must
	// initialize the runtime on first use and write through to real source.
	write, err := service.ExecuteCode(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_WriteFile{WriteFile: &codev0.WriteFileRequest{Path: "note.txt", Content: "ok\n"}},
	})
	if err != nil {
		t.Fatalf("WriteFile transport error: %v", err)
	}
	if !write.GetWriteFile().GetSuccess() {
		t.Fatalf("WriteFile did not succeed: %+v", write)
	}
	if !session.runtimeOK {
		t.Fatal("a mutating Code operation must initialize the runtime on first use")
	}
	if data, err := os.ReadFile(filepath.Join(root, "note.txt")); err != nil || string(data) != "ok\n" {
		t.Fatalf("mutating Code operation did not write through to source: data=%q err=%v", data, err)
	}
}

func writeSourceFile(t *testing.T, root, relative, body string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
