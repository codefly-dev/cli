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

// rivalFixturePackageManifest declares the same fixture name as
// fixturePackageManifest, under a different package id.
const rivalFixturePackageManifest = `kind: module-package
schema: codefly/module-package/v2
id: rival-starter
version: 1.0.0
minimum-codefly-version: ">=0.3.32"
artifact-roots:
  - contracts
contracts:
  composition: ">=0.1.0"
  fixtures: ">=0.1.0"
fixtures:
  - name: dev-admin
    description: A second seed under the same name
    principals:
      - id: rival-admin
        email: rival@example.com
        role: admin
        token: rival-token
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

// unloadablePackageManifest is a real manifest at the real path that core
// refuses: the schema is v1, not the required v2.
const unloadablePackageManifest = `kind: module-package
schema: codefly/module-package/v1
id: legacy
version: 1.0.0
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

	fixtures, problems := WorkspaceFixtures(context.Background(), workspace)
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
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

// A collision is exactly what this listing is used to diagnose, so it must be
// reported WITHOUT withholding what each package declares — core's Fixtures
// returns nothing at all in that case.
func TestWorkspaceFixturesStillNamesTheDeclarationsOnACollision(t *testing.T) {
	workspace := composedWorkspace(t,
		composedModule{name: "saas", manifest: fixturePackageManifest},
		composedModule{name: "rival", manifest: rivalFixturePackageManifest},
	)

	fixtures, problems := WorkspaceFixtures(context.Background(), workspace)
	if len(problems) == 0 {
		t.Fatal("a name declared by two packages was not reported as a collision")
	}
	if !errors.Is(errors.Join(problems...), corecomposition.ErrCollision) {
		t.Errorf("problems = %v, want ErrCollision", problems)
	}
	if len(fixtures) != 2 {
		t.Fatalf("fixtures = %d, want both declarations listed: %+v", len(fixtures), fixtures)
	}
}

// A module composing no package ships no manifest. Reporting that as a problem
// would make every ordinary workspace look broken.
func TestComposedPackageManifestsSkipsAModuleShippingNoPackage(t *testing.T) {
	workspace := composedWorkspace(t,
		composedModule{name: "backend"},
		composedModule{name: "saas", manifest: fixturePackageManifest},
	)

	manifests, problems := composedPackageManifests(context.Background(), workspace)
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if len(manifests) != 1 {
		t.Fatalf("manifests = %d, want only the composed package", len(manifests))
	}
	if manifests[0].ID != "saas-starter" {
		t.Errorf("manifest id = %q, want saas-starter", manifests[0].ID)
	}
}

// A manifest that exists but does not load is a problem, not an absent package:
// reading it as "composes nothing" would hide a package that declares fixtures.
func TestComposedPackageManifestsReportsAnUnloadableManifest(t *testing.T) {
	workspace := composedWorkspace(t, composedModule{name: "legacy", manifest: unloadablePackageManifest})

	manifests, problems := composedPackageManifests(context.Background(), workspace)
	if len(problems) == 0 {
		t.Fatal("an invalid package manifest was reported as composing nothing")
	}
	if len(manifests) != 0 {
		t.Errorf("manifests = %d, want none loaded", len(manifests))
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

// Once every composed package is readable, the declared set is authoritative:
// a name no package declares is refused even though a service could implement
// it itself. That is the documented behavior change — on the run path such a
// name is selected with --set <service>:CODEFLY__FIXTURE= instead.
func TestValidateFixtureSelectionRefusesAnUndeclaredNameWhenEveryPackageIsReadable(t *testing.T) {
	workspace := composedWorkspace(t,
		composedModule{name: "backend"},
		composedModule{name: "saas", manifest: fixturePackageManifest},
	)

	if err := ValidateFixtureSelection(context.Background(), workspace, "my-seed"); err == nil {
		t.Fatal("an undeclared fixture was accepted in a fully readable workspace")
	}
}

// A package that could not be read may be the one declaring the name, so the
// selection cannot be called a typo. Refusing here would fail a run over a
// manifest it never had to load.
func TestValidateFixtureSelectionDoesNotRefuseWhileAPackageCannotBeRead(t *testing.T) {
	workspace := composedWorkspace(t,
		composedModule{name: "saas", manifest: fixturePackageManifest},
		composedModule{name: "legacy", manifest: unloadablePackageManifest},
	)

	if err := ValidateFixtureSelection(context.Background(), workspace, "my-seed"); err != nil {
		t.Fatalf("a run was refused over a package it could not read: %v", err)
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
