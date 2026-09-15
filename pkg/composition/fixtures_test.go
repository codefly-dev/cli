package composition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
)

const fixturePackageManifest = `kind: module-package
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
      - id: user-member
        email: member@example.com
        role: member
        token: dev-member-token
`

const fixturelessPackageManifest = `kind: module-package
schema: codefly/module-package/v2
id: docs-site
version: 1.0.0
minimum-codefly-version: ">=0.3.0"
artifact-roots:
  - contracts
contracts:
  composition: ">=0.1.0"
`

type composedModule struct {
	name string
	// manifest is the module's module.package.codefly.yaml. Empty writes none,
	// which is what a module composing no package looks like on disk.
	manifest string
}

func composedWorkspace(t *testing.T, modules ...composedModule) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	workspace := &resources.Workspace{Name: "wiki"}
	for _, module := range modules {
		dir := filepath.Join(root, module.name)
		if err := os.MkdirAll(filepath.Join(dir, "contracts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if module.manifest != "" {
			path := filepath.Join(dir, corecomposition.PackageManifestFileName)
			if err := os.WriteFile(path, []byte(module.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		override := module.name
		workspace.Modules = append(workspace.Modules, &resources.ModuleReference{Name: module.name, PathOverride: &override})
	}
	workspace.WithDir(root)
	return workspace
}

func TestWorkspaceFixturesReportsEveryDeclaredPrincipal(t *testing.T) {
	workspace := composedWorkspace(t, composedModule{name: "saas", manifest: fixturePackageManifest})

	fixtures, err := WorkspaceFixtures(context.Background(), workspace)
	if err != nil {
		t.Fatalf("WorkspaceFixtures: %v", err)
	}
	if len(fixtures) != 1 {
		t.Fatalf("fixtures = %d, want 1: %+v", len(fixtures), fixtures)
	}
	if fixtures[0].Name != "dev-admin" {
		t.Errorf("fixture name = %q, want dev-admin", fixtures[0].Name)
	}
	if len(fixtures[0].Principals) != 2 {
		t.Fatalf("principals = %d, want 2", len(fixtures[0].Principals))
	}
	principal, err := fixtures[0].Principal("admin")
	if err != nil {
		t.Fatalf("Principal(admin): %v", err)
	}
	if principal.Email != "admin@example.com" || principal.ID != "user-admin" {
		t.Errorf("admin principal = %+v, want the declared id and email", principal)
	}
}

// A module composing no package ships no manifest. Reporting that as an error
// would make every ordinary workspace fail to list its fixtures.
func TestComposedPackageManifestsSkipsAModuleShippingNoPackage(t *testing.T) {
	workspace := composedWorkspace(t,
		composedModule{name: "backend"},
		composedModule{name: "saas", manifest: fixturePackageManifest},
	)

	manifests, err := ComposedPackageManifests(context.Background(), workspace)
	if err != nil {
		t.Fatalf("ComposedPackageManifests: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("manifests = %d, want only the composed package", len(manifests))
	}
	if manifests[0].ID != "saas-starter" {
		t.Errorf("manifest id = %q, want saas-starter", manifests[0].ID)
	}
}

// A manifest that exists but does not load is a defect in the module: reporting
// "no fixtures" for it would hide a package that declares some.
func TestComposedPackageManifestsReportsAnUnloadableManifest(t *testing.T) {
	workspace := composedWorkspace(t, composedModule{name: "saas", manifest: "kind: module-package\nschema: nonsense\n"})

	if _, err := ComposedPackageManifests(context.Background(), workspace); err == nil {
		t.Fatal("an invalid package manifest was reported as composing nothing")
	}
}

func TestValidateFixtureSelectionAcceptsADeclaredFixture(t *testing.T) {
	workspace := composedWorkspace(t, composedModule{name: "saas", manifest: fixturePackageManifest})

	if err := ValidateFixtureSelection(context.Background(), workspace, "dev-admin"); err != nil {
		t.Fatalf("declared fixture was refused: %v", err)
	}
}

// The whole point of validating at load: a typo names the fixtures that exist
// instead of booting the stack and failing somewhere inside it.
func TestValidateFixtureSelectionRejectsATypoAndNamesTheAvailableFixtures(t *testing.T) {
	workspace := composedWorkspace(t, composedModule{name: "saas", manifest: fixturePackageManifest})

	err := ValidateFixtureSelection(context.Background(), workspace, "dev-admn")
	if err == nil {
		t.Fatal("an undeclared fixture was accepted")
	}
	if !errors.Is(err, corecomposition.ErrUnknownFixture) {
		t.Errorf("error = %v, want ErrUnknownFixture", err)
	}
	if !strings.Contains(err.Error(), "dev-admin") {
		t.Errorf("error %q does not name the available fixture", err)
	}
}

// A selection is only resolvable once a composed package declares fixtures. A
// workspace whose packages declare none still passes the name through to the
// runtime, so validating against an empty set would refuse it outright.
func TestValidateFixtureSelectionIgnoresAWorkspaceDeclaringNoFixtures(t *testing.T) {
	workspace := composedWorkspace(t,
		composedModule{name: "backend"},
		composedModule{name: "docs", manifest: fixturelessPackageManifest},
	)

	if err := ValidateFixtureSelection(context.Background(), workspace, "seed"); err != nil {
		t.Fatalf("a fixture no composed package declares was refused: %v", err)
	}
}

func TestValidateFixtureSelectionIgnoresAnEmptySelection(t *testing.T) {
	workspace := composedWorkspace(t, composedModule{name: "saas", manifest: fixturePackageManifest})

	if err := ValidateFixtureSelection(context.Background(), workspace, ""); err != nil {
		t.Fatalf("an unset --fixture was refused: %v", err)
	}
}
