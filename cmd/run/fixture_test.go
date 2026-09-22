package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const runFixturePackageManifest = `kind: module-package
schema: codefly/module-package/v2
id: saas-starter
version: 1.0.0
minimum-codefly-version: ">=0.3.32"
artifact-roots:
  - contracts
contracts:
  composition: ">=0.1.0"
  fixtures: ">=0.1.0"
fixtures:
  - name: dev-admin
    description: Seeded administrator
    principals:
      - id: user-admin
        email: admin@example.com
        role: admin
        token: dev-admin-token
`

func composedFixtureWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "saas")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "contracts"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, corecomposition.PackageManifestFileName),
		[]byte(runFixturePackageManifest), 0o644))

	override := "saas"
	workspace := &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "saas", PathOverride: &override}},
	}
	workspace.WithDir(root)
	return workspace
}

func useRunFixtureFlag(t *testing.T, value string) {
	t.Helper()
	previous := fixture
	t.Cleanup(func() { fixture = previous })
	fixture = value
}

// The run path must verify the fixture it actually runs with, not the flag. An
// environment declares the fixture its runtime uses, so this workspace runs
// with --fixture unset and a typo in workspace.codefly.yaml reaches the stack
// exactly as an unverified flag would — validating only the flag let it boot
// the whole solution and fail somewhere inside.
func TestRunFixtureRejectsATypoDeclaredByTheEnvironment(t *testing.T) {
	useRunFixtureFlag(t, "")
	workspace := composedFixtureWorkspace(t)

	_, err := runFixture(context.Background(), workspace, &environments.Environment{Fixture: "dev-admn"})
	require.Error(t, err, "an environment-declared typo reached the run unverified")
	require.Contains(t, err.Error(), "dev-admin", "the error does not name the available fixture")
}

func TestRunFixtureAcceptsAFixtureDeclaredByTheEnvironment(t *testing.T) {
	useRunFixtureFlag(t, "")
	workspace := composedFixtureWorkspace(t)

	selected, err := runFixture(context.Background(), workspace, &environments.Environment{Fixture: "dev-admin"})
	require.NoError(t, err)
	require.Equal(t, "dev-admin", selected)
}

func TestRunFixtureRejectsATypoPassedAsTheFlag(t *testing.T) {
	useRunFixtureFlag(t, "dev-admn")
	workspace := composedFixtureWorkspace(t)

	_, err := runFixture(context.Background(), workspace, &environments.Environment{})
	require.Error(t, err)
}

// The flag wins over the declaration, so the flag's value is the one verified.
func TestRunFixtureVerifiesTheOverrideRatherThanTheDeclaration(t *testing.T) {
	useRunFixtureFlag(t, "dev-admin")
	workspace := composedFixtureWorkspace(t)

	selected, err := runFixture(context.Background(), workspace, &environments.Environment{Fixture: "dev-admn"})
	require.NoError(t, err, "the overridden declaration was verified instead of the override")
	require.Equal(t, "dev-admin", selected)
}

func TestRunFixtureIgnoresAnUnsetSelection(t *testing.T) {
	useRunFixtureFlag(t, "")
	workspace := composedFixtureWorkspace(t)

	selected, err := runFixture(context.Background(), workspace, &environments.Environment{})
	require.NoError(t, err)
	require.Empty(t, selected)
}
