package mcp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func writeMCPWorkspaceWithRunnable(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":              "name: demo\nlayout: modules\nmodules:\n    - name: backend\n",
		"modules/backend/module.codefly.yaml": "kind: module\nname: backend\nrunnables:\n    - name: word-count\n",
		"modules/backend/runnables/word-count/runnable.codefly.yaml": `kind: runnable
name: word-count
version: 0.1.0
agent:
  kind: codefly:runnable
  name: python
  version: 0.0.1
  publisher: codefly.dev
contract:
  protocol: codefly.runnable/v1
  input:
    fields:
      - name: text
        type: string
  output:
    fields:
      - name: count
        type: integer
entrypoint:
  handler: handler.py
execution:
  facilities: [native]
  timeout: 2m
  cancellation: signal
  recovery: recompute
`,
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

func TestListRunnablesToolViaControlPlane(t *testing.T) {
	t.Chdir(writeMCPWorkspaceWithRunnable(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	text := callTool(t, server, ctx, "list_runnables", `{}`).Content[0].Text
	for _, want := range []string{
		`"name": "word-count"`,
		`"module": "backend"`,
		`"version": "0.1.0"`,
		`"agent": "codefly.dev/python:0.0.1"`,
		`"protocol": "codefly.runnable/v1"`,
		`"max_output_bytes": 1048576`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("list_runnables output missing %q:\n%s", want, text)
		}
	}
}

// TestListRunnablesToolFailsOnAnInvalidDeclaration keeps a broken runnable a
// reported failure rather than a shorter list.
func TestListRunnablesToolFailsOnAnInvalidDeclaration(t *testing.T) {
	root := writeMCPWorkspaceWithRunnable(t)
	declaration := filepath.Join(root, "modules/backend/runnables/word-count/runnable.codefly.yaml")
	content, err := os.ReadFile(declaration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(declaration, append(content, []byte("optionnal: true\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.listRunnables(ctx, map[string]string{}); err == nil {
		t.Fatal("list_runnables succeeded with an unloadable runnable declaration")
	}
}

// TestAgentKindVocabularyIsTheExactPublishedSet pins both directions. The
// enum is client-facing MCP schema that is now derived from core's registry,
// so a core-side rename or removal would otherwise change what this server
// publishes on a go.mod bump with nothing failing. Update this list
// deliberately when core registers a kind.
func TestAgentKindVocabularyIsTheExactPublishedSet(t *testing.T) {
	want := []string{"service", "job", "application", "module", "toolbox", "provider", "solution", "runnable"}
	if !slices.Equal(agentKindEnumValues, want) {
		t.Fatalf("published agent kind enum = %q, want %q", agentKindEnumValues, want)
	}
	for _, arg := range want {
		kind, ok := listAgentsKindByArg[arg]
		if !ok {
			t.Errorf("agent kind %q is advertised but not mapped", arg)
			continue
		}
		if _, err := resources.AgentKindRegistrationFor(kind); err != nil {
			t.Errorf("agent kind %q maps to %q, which core does not register: %v", arg, kind, err)
		}
	}
}

// TestAgentKindVocabularyCoversTheRegistry is the other direction: every kind
// core registers must be filterable, so adding one upstream cannot leave the
// MCP tools unable to see it (runnable was exactly that gap).
func TestAgentKindVocabularyCoversTheRegistry(t *testing.T) {
	for _, registration := range resources.AgentKindRegistry() {
		arg := strings.TrimPrefix(string(registration.Resource), "codefly:")
		if !slices.Contains(agentKindEnumValues, arg) {
			t.Errorf("agent kind %q is registered in core but not advertised by the MCP tools", arg)
		}
		if listAgentsKindByArg[arg] != registration.Resource {
			t.Errorf("agent kind arg %q maps to %q, want %q", arg, listAgentsKindByArg[arg], registration.Resource)
		}
	}
}

func TestRunnableAgentPinsAppearBeforeInstallation(t *testing.T) {
	t.Chdir(writeMCPWorkspaceWithRunnable(t))
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	text := callTool(t, server, ctx, "list_agents", `{"kind":"runnable"}`).Content[0].Text
	for _, want := range []string{`"python"`, `"0.0.1"`, `"backend/word-count"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("pinned Runnable agent missing %s: %s", want, text)
		}
	}
}

func TestListRunnablesRefusesUnknownModule(t *testing.T) {
	t.Chdir(writeMCPWorkspaceWithRunnable(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.listRunnables(ctx, map[string]string{fieldModule: "absent"}); err == nil {
		t.Fatal("unknown module was reported as an empty listing")
	}
	content, err := server.listRunnables(ctx, map[string]string{fieldModule: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != 1 || !strings.Contains(content[0].Text, "word-count") {
		t.Fatalf("known module missing its Runnable: %+v", content)
	}
}
