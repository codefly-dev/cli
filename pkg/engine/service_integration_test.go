package engine

import (
	"os"
	"path/filepath"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	toolingv0 "github.com/codefly-dev/core/generated/go/codefly/services/tooling/v0"
)

// TestReadOnlyCodeAndToolingRunWithoutRuntimeInitialization exercises the real
// production agent against malformed source. GetProjectInfo/GetSemanticIndex and
// the agent-level command surface must run without ever triggering Runtime
// Load/Init, and the first runtime call must initialize it lazily.
func TestReadOnlyCodeAndToolingRunWithoutRuntimeInitialization(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, root, "pyproject.toml", "[project]\nname = \"probe\"\nversion = \"0.0.0\"\n")
	writeSourceFile(t, root, "broken.py", "def oops(:\n    return\n")

	agent, err := DetectSourceAgent(root)
	if err != nil {
		t.Fatalf("detect source agent: %v", err)
	}

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
	if info.GetModule() != "probe" || info.GetLanguage() != "python" {
		t.Fatalf("project info did not recover identity from malformed source: %+v", info)
	}

	semantic, err := service.GetSemanticIndex(ctx, &toolingv0.GetSemanticIndexRequest{})
	if err != nil {
		t.Fatalf("GetSemanticIndex transport error: %v", err)
	}
	index := semantic.GetIndex()
	if index.GetState() != basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_DEGRADED {
		t.Fatalf("malformed source should degrade, not fail: state=%s issues=%+v", index.GetState(), index.GetIssues())
	}
	if len(index.GetLanguages()) != 1 || index.GetLanguages()[0] != "python" {
		t.Fatalf("semantic languages = %v, want [python]", index.GetLanguages())
	}
	if !hasIssueCode(index.GetIssues(), "parse_failed") {
		t.Fatalf("semantic recovery should report the parse failure, got %+v", index.GetIssues())
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

	if _, err := service.Test(ctx, &runtimev0.TestRequest{}); err != nil {
		t.Fatalf("Test transport error: %v", err)
	}
	if !session.runtimeOK {
		t.Fatal("the first runtime call must initialize the runtime lazily")
	}
}

func hasIssueCode(issues []*basev0.SemanticIssue, code string) bool {
	for _, issue := range issues {
		if issue.GetCode() == code {
			return true
		}
	}
	return false
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
