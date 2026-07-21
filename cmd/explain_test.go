package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/helpprovider"
)

func TestHelpProviderUsesVersionedProtocol(t *testing.T) {
	if os.Getenv("CODEFLY_HELP_PROVIDER_TEST_PROCESS") == "1" {
		data, _ := io.ReadAll(os.Stdin)
		var request helpprovider.Request
		if json.Unmarshal(data, &request) != nil ||
			request.ProtocolVersion != helpprovider.ProtocolVersion ||
			request.Application != "codefly" ||
			request.Command != "codefly build service" ||
			!strings.Contains(request.StaticHelp, "--push") ||
			!strings.Contains(string(request.Context), `"workspace":"demo"`) {
			os.Exit(2)
		}
		_, _ = fmt.Fprint(os.Stdout, `{"protocol_version":1,"explanation":"Use this to build demo/api."}`)
		os.Exit(0)
	}

	t.Setenv("CODEFLY_HELP_PROVIDER_TEST_PROCESS", "1")
	provider := &helpProvider{path: os.Args[0], args: []string{"-test.run=TestHelpProviderUsesVersionedProtocol"}}
	explanation, err := provider.explain(t.Context(), "codefly build service", "Flags:\n  --push", `{"workspace":"demo"}`)
	if err != nil {
		t.Fatal(err)
	}
	if explanation != "Use this to build demo/api." {
		t.Fatalf("explanation = %q", explanation)
	}
}

func TestHelpProviderIsOptional(t *testing.T) {
	t.Setenv("CODEFLY_HELP_PROVIDER", filepath.Join(t.TempDir(), "missing-provider"))
	if provider, configured := helpProviderFromEnvironment(); configured || provider != nil {
		t.Fatalf("provider = %#v, configured = %v", provider, configured)
	}
}

func TestHelpWorkspaceContextUsesOnlyResourceNames(t *testing.T) {
	root := t.TempDir()
	moduleDirectory := filepath.Join(root, "modules", "backend")
	workingDirectory := filepath.Join(moduleDirectory, "services", "api")
	if err := os.MkdirAll(workingDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := `name: demo
layout: modules
modules:
  - name: backend
environments:
  - name: staging
secrets:
  database-password: do-not-send
`
	module := `kind: module
name: backend
services:
  - name: api
jobs:
  - name: migrate
`
	if err := os.WriteFile(filepath.Join(root, "workspace.codefly.yaml"), []byte(workspace), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDirectory, "module.codefly.yaml"), []byte(module), 0o600); err != nil {
		t.Fatal(err)
	}

	context := helpWorkspaceContext(workingDirectory)
	for _, expected := range []string{
		`"workspace":"demo"`,
		`"current_path":"modules/backend/services/api"`,
		`"modules":["backend"]`,
		`"services":["backend/api"]`,
		`"jobs":["backend/migrate"]`,
		`"environments":["staging"]`,
	} {
		if !strings.Contains(context, expected) {
			t.Errorf("context %s does not contain %s", context, expected)
		}
	}
	if strings.Contains(context, "do-not-send") {
		t.Fatalf("context leaked unrelated workspace configuration: %s", context)
	}
}

func TestHelpWorkspaceContextDoesNotReadModuleOutsideWorkspace(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := "name: demo\nlayout: modules\nmodules:\n  - name: escaped\n    path: ../outside\n"
	module := "name: escaped\nservices:\n  - name: should-not-appear\n"
	if err := os.WriteFile(filepath.Join(root, "workspace.codefly.yaml"), []byte(workspace), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "module.codefly.yaml"), []byte(module), 0o600); err != nil {
		t.Fatal(err)
	}

	context := helpWorkspaceContext(root)
	if strings.Contains(context, "should-not-appear") {
		t.Fatalf("context read outside the workspace: %s", context)
	}
}

func TestHelpWorkspaceContextDoesNotFollowModuleSymlinkOutsideWorkspace(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(filepath.Join(root, "modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "modules", "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	workspace := "name: demo\nlayout: modules\nmodules:\n  - name: linked\n"
	module := "name: linked\nservices:\n  - name: should-not-appear\n"
	if err := os.WriteFile(filepath.Join(root, "workspace.codefly.yaml"), []byte(workspace), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "module.codefly.yaml"), []byte(module), 0o600); err != nil {
		t.Fatal(err)
	}

	context := helpWorkspaceContext(root)
	if strings.Contains(context, "should-not-appear") {
		t.Fatalf("context followed a module symlink outside the workspace: %s", context)
	}
}
