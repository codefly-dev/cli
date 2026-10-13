package publish_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/cmd/publish"
)

// A service manifest whose agent block, and so the agent's own `version:`,
// comes before the service's top-level version: the release must rewrite the
// service's line and leave the agent's alone.
const agentFirstServiceManifest = `name: api
agent:
    kind: codefly:service
    name: go-grpc
    version: 0.1.53
    publisher: codefly.dev
library-dependencies:
    - name: compute
      version: ^0.0.1
# released with the module
version: 0.0.0
`

const plainServiceManifest = `name: worker
version: 0.0.0 # set by codefly publish
agent:
    kind: codefly:service
    name: go
    version: 0.0.58
    publisher: codefly.dev
`

// moduleAgentRepo lays out a module agent's repository the way module repos
// are: agent.codefly.yaml at the root, the workspace that composes the module,
// and its services. It also carries a qualification workspace with a service
// of its own, which the module's workspace does not compose.
func moduleAgentRepo(t *testing.T, agentVersion string) (dir, origin string, files map[string]string) {
	t.Helper()
	dir = t.TempDir()
	origin = initBareGitRepo(t, dir)
	files = map[string]string{
		"agent.codefly.yaml":                                               "publisher: codefly.dev\nkind: codefly:module\nname: payments\nversion: " + agentVersion + "\n",
		"workspace.codefly.yaml":                                           "name: payments\nlayout: modules\nmodules:\n    - name: payments\n",
		"modules/payments/module.codefly.yaml":                             "kind: module\nname: payments\nservices:\n    - name: api\n    - name: worker\n",
		"modules/payments/services/api/service.codefly.yaml":               agentFirstServiceManifest,
		"modules/payments/services/worker/service.codefly.yaml":            plainServiceManifest,
		"qualification/modules/owners/services/owner/service.codefly.yaml": "name: owner\nversion: 0.0.0\n",
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		require.NoError(t, runGit(dir, "add", rel))
	}
	require.NoError(t, runGit(dir, "commit", "-m", "module repository"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))
	return dir, origin, files
}

func readRel(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, rel))
	require.NoError(t, err)
	return string(data)
}

func moduleEngine(t *testing.T, dir, bump string) *publish.Engine {
	t.Helper()
	manifest, err := publish.Detect(dir)
	require.NoError(t, err)
	require.Equal(t, publish.ModeAgent, manifest.Mode)
	return &publish.Engine{Manifest: manifest, BumpType: bump, WorkDir: dir}
}

// A module release sets every service its workspace composes to the released
// version in the release commit itself, so the tag names a tree whose
// services report the module's version instead of 0.0.0.
func TestModuleReleaseSetsEveryComposedServiceVersion(t *testing.T) {
	dir, origin, files := moduleAgentRepo(t, "0.2.0")

	tag, err := moduleEngine(t, dir, "patch").Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.2.1", tag)

	require.Contains(t, readRel(t, dir, "agent.codefly.yaml"), "version: 0.2.1\n")
	require.Equal(t,
		strings.Replace(agentFirstServiceManifest, "\nversion: 0.0.0\n", "\nversion: 0.2.1\n", 1),
		readRel(t, dir, "modules/payments/services/api/service.codefly.yaml"),
		"only the service's top-level version moves; the agent's and the library's stay")
	require.Equal(t,
		strings.Replace(plainServiceManifest, "version: 0.0.0 #", "version: 0.2.1 #", 1),
		readRel(t, dir, "modules/payments/services/worker/service.codefly.yaml"))
	require.Equal(t, files["qualification/modules/owners/services/owner/service.codefly.yaml"],
		readRel(t, dir, "qualification/modules/owners/services/owner/service.codefly.yaml"),
		"a service the module's workspace does not compose is not released")

	// One commit carries all three, and it is what the pushed tag names.
	out, err := exec.Command("git", "-C", origin, "show", "--name-only", "--format=%s", tag+"^{commit}").Output()
	require.NoError(t, err)
	require.Equal(t, []string{
		"release:", "v0.2.1",
		"agent.codefly.yaml",
		"modules/payments/services/api/service.codefly.yaml",
		"modules/payments/services/worker/service.codefly.yaml",
	}, strings.Fields(string(out)))
}

// The dry run names every service manifest the release would set and writes
// none of them.
func TestModuleReleaseDryRunWritesNoServiceManifest(t *testing.T) {
	dir, _, files := moduleAgentRepo(t, "0.2.0")
	engine := moduleEngine(t, dir, "minor")
	engine.DryRun = true

	tag, err := engine.Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.3.0", tag)
	for rel, content := range files {
		require.Equal(t, content, readRel(t, dir, rel), "a dry run wrote %s", rel)
	}
}

// A release that aborts before its commit — release-grade CI failing — leaves
// every service manifest exactly as it found it, staged nothing included.
func TestModuleReleaseAbortRestoresServiceManifests(t *testing.T) {
	dir, _, files := moduleAgentRepo(t, "0.2.0")
	engine := moduleEngine(t, dir, "patch")
	ciFailed := errors.New("release-grade agent CI failed")
	var during string
	engine.BeforeCommit = func(context.Context, string) error {
		during = readRel(t, dir, "modules/payments/services/worker/service.codefly.yaml")
		return ciFailed
	}

	_, err := engine.Release(context.Background())
	require.ErrorIs(t, err, ciFailed)
	require.Contains(t, during, "version: 0.2.1", "CI runs against the services at the version being released")
	for rel, content := range files {
		require.Equal(t, content, readRel(t, dir, rel), "%s was not restored", rel)
	}
	status, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(string(status)))
}

// A service already carrying a version past the release would have to move
// backwards to agree with it. That is refused before anything is written,
// naming the service and the bump that would carry the module past it.
func TestModuleReleaseRefusesToMoveAServiceVersionBackwards(t *testing.T) {
	dir, _, files := moduleAgentRepo(t, "0.0.47")
	worker := filepath.Join(dir, "modules/payments/services/worker/service.codefly.yaml")
	require.NoError(t, os.WriteFile(worker, []byte(strings.Replace(plainServiceManifest, "version: 0.0.0", "version: 0.1.0", 1)), 0o600))
	require.NoError(t, runGit(dir, "commit", "-am", "worker at 0.1.0"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))

	_, err := moduleEngine(t, dir, "patch").Release(context.Background())
	require.ErrorContains(t, err, "carries version 0.1.0, past the release 0.0.48")
	require.ErrorContains(t, err, "worker/service.codefly.yaml")
	require.Equal(t, files["agent.codefly.yaml"], readRel(t, dir, "agent.codefly.yaml"))

	tag, err := moduleEngine(t, dir, "minor").Release(context.Background())
	require.NoError(t, err, "a release that reaches the service's version is not a move backwards")
	require.Equal(t, "v0.1.0", tag)
	require.Contains(t, readRel(t, dir, "modules/payments/services/worker/service.codefly.yaml"), "version: 0.1.0 #")
}

// A composed service with no top-level version has nothing for the release to
// set; it is refused at pre-flight, naming the manifest, rather than released
// with a service that keeps reporting no version.
func TestModuleReleaseRefusesAServiceWithNoVersion(t *testing.T) {
	dir, _, _ := moduleAgentRepo(t, "0.2.0")
	api := filepath.Join(dir, "modules/payments/services/api/service.codefly.yaml")
	require.NoError(t, os.WriteFile(api, []byte(strings.Replace(agentFirstServiceManifest, "version: 0.0.0\n", "", 1)), 0o600))
	require.NoError(t, runGit(dir, "commit", "-am", "api without a version"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))

	engine := moduleEngine(t, dir, "patch")
	engine.DryRun = true
	_, err := engine.Release(context.Background())
	require.ErrorContains(t, err, "api/service.codefly.yaml")
	require.ErrorContains(t, err, "declare one (version: 0.0.0)")
}

// A module agent with no workspace beside it cannot say which services it
// ships; the release refuses rather than tagging a module whose services were
// never set.
func TestModuleReleaseRefusesWithoutAWorkspace(t *testing.T) {
	dir := t.TempDir()
	initBareGitRepo(t, dir)
	agent := filepath.Join(dir, "agent.codefly.yaml")
	require.NoError(t, os.WriteFile(agent, []byte("publisher: codefly.dev\nkind: codefly:module\nname: payments\nversion: 0.2.0\n"), 0o600))
	require.NoError(t, addAndCommit(dir, agent, "module agent"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))

	engine := moduleEngine(t, dir, "patch")
	engine.DryRun = true
	_, err := engine.Release(context.Background())
	require.ErrorContains(t, err, "has no workspace.codefly.yaml")
}

// Every other agent kind releases its agent manifest alone, as before.
func TestServiceAgentReleaseTouchesNoServiceManifest(t *testing.T) {
	dir, _, files := moduleAgentRepo(t, "0.2.0")
	agent := filepath.Join(dir, "agent.codefly.yaml")
	require.NoError(t, os.WriteFile(agent, []byte("publisher: codefly.dev\nkind: codefly:service\nname: payments\nversion: 0.2.0\n"), 0o600))
	require.NoError(t, runGit(dir, "commit", "-am", "a service agent"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))

	_, err := moduleEngine(t, dir, "patch").Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, files["modules/payments/services/worker/service.codefly.yaml"],
		readRel(t, dir, "modules/payments/services/worker/service.codefly.yaml"))
}
