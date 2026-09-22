package ci

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func overrideWorkspace(t *testing.T, overlay string) *resources.Workspace {
	t.Helper()
	dir := t.TempDir()
	if overlay != "" {
		if err := os.WriteFile(filepath.Join(dir, resources.LocalOverlayConfigurationName), []byte(overlay), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace := &resources.Workspace{
		Name:    "solution",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: "acme/host", Version: "1.0"}},
	}
	workspace.WithDir(dir)
	return workspace
}

// A CI plan is a claim about the committed workspace. A machine-local service
// override silently makes it a claim about a different one, so it is refused
// rather than reported.
func TestCIRefusesServiceOverrides(t *testing.T) {
	workspace := overrideWorkspace(t, "resolve:\n  saas:\n    services:\n      accounts:\n        path: /tmp/accounts\n")

	err := refuseServiceOverrides(context.Background(), workspace, false, "codefly ci plan")
	if err == nil {
		t.Fatal("ci planned with a service override in effect")
	}
	for _, want := range []string{"saas/accounts", "path /tmp/accounts", "--allow-service-overrides", resources.LocalOverlayConfigurationName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

func TestCIAllowsServiceOverridesWhenAsked(t *testing.T) {
	workspace := overrideWorkspace(t, "resolve:\n  saas:\n    services:\n      accounts:\n        version: \"0.0.66\"\n")

	if err := refuseServiceOverrides(context.Background(), workspace, true, "codefly ci plan"); err != nil {
		t.Fatalf("--allow-service-overrides still refused: %v", err)
	}
}

// Module-level directives are committed-config-equivalent machinery the CI path
// already handles; only per-service overrides are refused.
func TestCIIgnoresModuleLevelOverlayEntries(t *testing.T) {
	for name, overlay := range map[string]string{
		"no overlay":     "",
		"module pinned":  "resolve:\n  saas:\n    pinned: true\n",
		"module path":    "resolve:\n  saas:\n    path: /tmp/saas\n",
		"other module":   "resolve:\n  billing:\n    services:\n      ledger:\n        path: /tmp/ledger\n",
		"empty services": "resolve:\n  saas:\n    pinned: true\n    services: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			workspace := overrideWorkspace(t, overlay)
			if err := refuseServiceOverrides(context.Background(), workspace, false, "codefly ci plan"); err != nil {
				t.Fatalf("refused without a service override on a composed module: %v", err)
			}
		})
	}
}

func TestDescribeServiceDirectiveNamesTheSpelling(t *testing.T) {
	cases := map[*resources.ServiceResolveDirective]string{
		{Path: "/tmp/x"}:             "path /tmp/x",
		{Worktree: "acme/host@main"}: "worktree acme/host@main",
		{Version: "0.0.66"}:          "version 0.0.66",
	}
	for directive, want := range cases {
		if got := describeServiceDirective(directive); got != want {
			t.Errorf("describeServiceDirective = %q, want %q", got, want)
		}
	}
}
