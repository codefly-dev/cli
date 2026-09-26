package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const devBuild = "0.1.47-dev.abc123def456"

// fakeInspection replaces the live admission for the test: it records every
// candidate and fails for the versions in unpublished, standing in for a
// version whose release does not exist.
func fakeInspection(t *testing.T, unpublished ...string) *[]resources.Agent {
	t.Helper()
	previous := inspectAgent
	t.Cleanup(func() { inspectAgent = previous })
	var seen []resources.Agent
	inspectAgent = func(_ context.Context, candidate *resources.Agent) error {
		seen = append(seen, *candidate)
		for _, version := range unpublished {
			if candidate.Version == version {
				return errors.New("release not found")
			}
		}
		return nil
	}
	return &seen
}

func pinService(name, agent, version string) string {
	return "kind: service\nname: " + name + "\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: " + agent + "\n  version: " + version + "\n  publisher: codefly.dev\n"
}

// writePinWorkspace composes two local modules, acme and billing, whose
// services run go-grpc 0.1.46 and 0.1.45 and nextjs 0.0.159.
func writePinWorkspace(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                               "name: example\nlayout: modules\nmodules:\n  - name: acme\n  - name: billing\n" + extra,
		"modules/acme/module.codefly.yaml":                     "kind: module\nname: acme\nservices:\n  - name: accounts\n  - name: frontend\n",
		"modules/acme/services/accounts/service.codefly.yaml":  pinService("accounts", "go-grpc", "0.1.46"),
		"modules/acme/services/frontend/service.codefly.yaml":  pinService("frontend", "nextjs", "0.0.159"),
		"modules/billing/module.codefly.yaml":                  "kind: module\nname: billing\nservices:\n  - name: ledger\n",
		"modules/billing/services/ledger/service.codefly.yaml": pinService("ledger", "go-grpc", "0.1.45"),
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return dir
}

func readWorkspaceFile(t *testing.T, dir string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, resources.WorkspaceConfigurationName))
	require.NoError(t, err)
	return string(content)
}

func TestPinAgentOverrideWritesAPrereleaseAndReadsItBack(t *testing.T) {
	ctx := context.Background()
	seen := fakeInspection(t)
	dir := writePinWorkspace(t, "agent-overrides:\n  codefly.dev/nextjs: 0.0.160\n")
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	require.NoError(t, err)

	overrides, err := parseAgentOverrideFlags([]string{"codefly.dev/go-grpc=" + devBuild})
	require.NoError(t, err)
	uses, err := pinAgentOverrides(ctx, workspace, overrides)
	require.NoError(t, err)

	// The candidate was admitted at the dev version with the kind a composed
	// service runs it as.
	require.Len(t, *seen, 1)
	require.Equal(t, devBuild, (*seen)[0].Version)
	require.Equal(t, "go-grpc", (*seen)[0].Name)
	require.Equal(t, "codefly.dev", (*seen)[0].Publisher)
	require.Equal(t, resources.AgentKind("runtime::service"), (*seen)[0].Kind)

	require.Len(t, uses, 2)
	require.Equal(t, "codefly.dev/go-grpc", uses[0].Key())
	require.Equal(t, []string{"acme/accounts", "billing/ledger"}, uses[0].Services)

	// Read back from disk through core's loader: the new entry is there, the
	// entry the command did not name is kept, and a composed service runs it.
	reloaded, err := resources.LoadWorkspaceFromDir(ctx, dir)
	require.NoError(t, err)
	got, err := reloaded.AgentOverrides()
	require.NoError(t, err)
	require.Equal(t, []resources.AgentOverride{
		{Publisher: "codefly.dev", Name: "go-grpc", Version: devBuild},
		{Publisher: "codefly.dev", Name: "nextjs", Version: "0.0.160"},
	}, got)
	mod, err := reloaded.LoadModuleFromName(ctx, "acme")
	require.NoError(t, err)
	accounts, err := mod.LoadServiceFromName(ctx, "accounts")
	require.NoError(t, err)
	require.Equal(t, devBuild, accounts.Agent.Version)

	// The module's own file is untouched.
	declared, err := resources.LoadServiceFromDir(ctx, filepath.Join(dir, "modules/acme/services/accounts"))
	require.NoError(t, err)
	require.Equal(t, "0.1.46", declared.Agent.Version)
}

func TestPinAgentOverrideRefusesInvalidVersions(t *testing.T) {
	for _, value := range []string{
		"codefly.dev/go-grpc=latest",
		"codefly.dev/go-grpc=^0.1.47",
		"codefly.dev/go-grpc=0.1",
		"codefly.dev/go-grpc=v0.1.47",
		"codefly.dev/go-grpc=0.1.47-dev..abc",
		"codefly.dev/go-grpc",
		"codefly.dev/go-grpc=",
		"go-grpc=0.1.47",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := parseAgentOverrideFlags([]string{value})
			require.Error(t, err)
		})
	}
	_, err := parseAgentOverrideFlags([]string{"codefly.dev/go-grpc=0.1.47", "codefly.dev/go-grpc=0.1.48"})
	require.ErrorContains(t, err, "twice")
}

func TestPinAgentOverrideRefusesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		flag        string
		unpublished string
		want        string
	}{
		"agent no service runs on": {flag: "codefly.dev/go-grcp=0.1.47", want: "codefly.dev/go-grcp"},
		"unpublished version":      {flag: "codefly.dev/go-grpc=" + devBuild, unpublished: devBuild, want: "release not found"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeInspection(t, tc.unpublished)
			dir := writePinWorkspace(t, "")
			before := readWorkspaceFile(t, dir)
			workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
			require.NoError(t, err)
			overrides, err := parseAgentOverrideFlags([]string{tc.flag})
			require.NoError(t, err)
			_, err = pinAgentOverrides(ctx, workspace, overrides)
			require.ErrorContains(t, err, tc.want)
			require.Equal(t, before, readWorkspaceFile(t, dir), "a refused override must not touch the file")
			_, still := workspace.Extensions[resources.AgentOverridesKey]
			require.False(t, still, "a refused override must not linger in memory")
		})
	}
}

func TestPinServiceAgentWritesAPrereleaseAndReadsItBack(t *testing.T) {
	ctx := context.Background()
	seen := fakeInspection(t)
	dir := t.TempDir()
	content := "# retained comment\n" + pinService("api", "go-grpc", "0.1.46") + "future-field: retained\n"
	file := filepath.Join(dir, resources.ServiceConfigurationName)
	require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
	svc, err := resources.LoadServiceFromDir(ctx, dir)
	require.NoError(t, err)

	update, err := pinServiceAgent(ctx, svc, devBuild)
	require.NoError(t, err)
	require.Equal(t, &agentUpdate{Name: "go-grpc", From: "0.1.46", To: devBuild}, update)
	require.Len(t, *seen, 1)
	require.Equal(t, devBuild, (*seen)[0].Version)

	reloaded, err := resources.LoadServiceFromDir(ctx, dir)
	require.NoError(t, err)
	require.Equal(t, devBuild, reloaded.Agent.Version)
	after, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Contains(t, string(after), "# retained comment")
	require.Contains(t, string(after), "future-field: retained")

	// Pinning the version already written is a no-op that admits nothing.
	update, err = pinServiceAgent(ctx, reloaded, devBuild)
	require.NoError(t, err)
	require.Nil(t, update)
	require.Len(t, *seen, 1)
}

func TestPinServiceAgentRefusesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		version     string
		unpublished string
		want        string
	}{
		"latest":              {version: "latest", want: "not an exact semantic version"},
		"range":               {version: "^0.1.47", want: "not an exact semantic version"},
		"v prefix":            {version: "v0.1.47", want: "not an exact semantic version"},
		"partial":             {version: "0.1", want: "not an exact semantic version"},
		"unpublished version": {version: devBuild, unpublished: devBuild, want: "release not found"},
	} {
		t.Run(name, func(t *testing.T) {
			seen := fakeInspection(t, tc.unpublished)
			dir := t.TempDir()
			content := pinService("api", "go-grpc", "0.1.46")
			file := filepath.Join(dir, resources.ServiceConfigurationName)
			require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
			svc, err := resources.LoadServiceFromDir(ctx, dir)
			require.NoError(t, err)

			update, err := pinServiceAgent(ctx, svc, tc.version)
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, update)
			after, err := os.ReadFile(file)
			require.NoError(t, err)
			require.Equal(t, content, string(after))
			require.Equal(t, "0.1.46", svc.Agent.Version)
			if tc.unpublished == "" {
				require.Empty(t, *seen, "an invalid version must be refused before any download")
			}
		})
	}
}

func TestAdmissionErrorNamesAnUnpublishedVersion(t *testing.T) {
	candidate := &resources.Agent{Publisher: "codefly.dev", Name: "go-grpc", Version: devBuild}
	cause := fmt.Errorf("%w: unexpected status code 404 when downloading agent", manager.ErrAgentBinaryNotFound)
	err := fmt.Errorf("cannot override agent: %w", admissionError(candidate, cause))
	require.ErrorIs(t, err, manager.ErrAgentBinaryNotFound)
	// The line the terminal prints without --debug is the innermost single
	// unwrap: it must name the agent and the version.
	root := err
	for next := errors.Unwrap(root); next != nil; next = errors.Unwrap(root) {
		root = next
	}
	require.Contains(t, root.Error(), "agent codefly.dev/go-grpc is not published at version "+devBuild)

	other := errors.New("incompatible")
	require.Equal(t, other, admissionError(candidate, other), "only a missing release is reworded")
}

func TestUpdateServiceRefusesAComposedModule(t *testing.T) {
	workspace := &resources.Workspace{Modules: []*resources.ModuleReference{
		{Name: "acme", Source: "example.com/acme/module-acme", Version: "0.0.1"},
		{Name: "local"},
	}}
	svc := &resources.Service{Name: "api", Agent: &resources.Agent{Publisher: "codefly.dev", Name: "go-grpc", Version: "0.1.46"}}

	err := refuseComposedService(workspace, &resources.Module{Name: "acme"}, svc)
	require.ErrorContains(t, err, "codefly update workspace --agent-override codefly.dev/go-grpc=<version>")
	require.NoError(t, refuseComposedService(workspace, &resources.Module{Name: "local"}, svc))
}
