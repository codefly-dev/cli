package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestNoDeployVerbIsExposedAsATool records a decision that was until now only an
// absence.
//
// The `codefly deploy` family acts on real clusters and real secret stores with
// the operator's own credentials — `deploy secrets` writes an environment's
// secret store through their `gcloud` login, and `deploy gitops publish` pushes
// what a cluster reconciles. MCP tools are called by agents, so exposing any of
// them would hand an agent that authority without the operator being the one who
// typed the command.
//
// Nothing in the tool registry said so, which meant the next person to add a
// deploy tool would find no reason not to. Now they find this, and can change it
// deliberately: the boundary is one line to move, with the reason attached.
func TestNoDeployVerbIsExposedAsATool(t *testing.T) {
	server, err := NewServer(context.Background(), "test-version")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	definitions := server.toolbox.Definitions()
	if len(definitions) == 0 {
		t.Fatal("the registry exposed no tools at all; this guard would pass vacuously")
	}
	for _, tool := range definitions {
		if exposesDeploySurface(tool.Name) {
			t.Errorf("tool %q exposes the deploy surface to an agent; deploy verbs act on real clusters and secret stores with the operator's credentials — if this is intended, change this test and say why", tool.Name)
		}
	}
}

// exposesDeploySurface reports whether a tool name names a deploy verb.
func exposesDeploySurface(name string) bool {
	lowered := strings.ToLower(name)
	return lowered == "deploy" || strings.HasPrefix(lowered, "deploy_") || strings.HasSuffix(lowered, "_deploy")
}

// The boundary test passes today because no deploy tool exists, so on its own it
// cannot tell "correctly empty" from "predicate that never matches". This pins
// both directions.
func TestDeployBoundaryGuardCatchesADeployTool(t *testing.T) {
	for _, name := range []string{"deploy", "deploy_secrets", "deploy_gitops_publish", "gitops_deploy"} {
		if !exposesDeploySurface(name) {
			t.Errorf("a tool named %q would slip past the deploy boundary", name)
		}
	}
	for _, name := range []string{"build", "run_service", "workspace_info", "list_modules", "add_service"} {
		if exposesDeploySurface(name) {
			t.Errorf("%q is a workspace tool and must not be flagged as deploy surface", name)
		}
	}
}
