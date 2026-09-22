package composition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
)

// composedPackageManifests returns the package manifest of every module the
// workspace composes, in workspace declaration order, along with whatever kept
// that answer from being complete.
//
// Most modules compose no package and ship no manifest, so an absent one is not
// a problem. A module that does not resolve, or whose manifest exists and does
// not load, is returned as a problem rather than an error: whether an
// incomplete picture is fatal belongs to the caller. Listing fixtures must
// report it; refusing to boot over a package the run never had to read must
// not.
func composedPackageManifests(ctx context.Context, workspace *resources.Workspace) ([]*corecomposition.PackageManifest, []error) {
	var manifests []*corecomposition.PackageManifest
	var problems []error
	for _, ref := range workspace.Modules {
		dir, err := ResolveComposedModuleDir(ctx, workspace, ref)
		if err != nil {
			problems = append(problems, fmt.Errorf("cannot resolve composed module %q: %w", ref.Name, err))
			continue
		}
		manifest, err := corecomposition.LoadPackageManifest(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			problems = append(problems, fmt.Errorf("module %q: %w", ref.Name, err))
			continue
		}
		manifests = append(manifests, manifest)
	}
	return manifests, problems
}

// fixtureDeclaration is one composed package's declaration of a fixture name.
type fixtureDeclaration struct {
	fixture corecomposition.ProvidedFixture
	owner   string
}

// declarationsByName groups every fixture the readable manifests declare under
// its name, in package order, with each declaration's principals detached from
// the manifest it came from.
//
// Grouping here rather than calling core's Fixtures is what lets a collision be
// scoped to the name it actually affects: Fixtures answers for the whole
// workspace and returns nothing at all once any two packages clash, which
// cannot express "dev-admin is ambiguous but staging-seed is still resolvable".
func declarationsByName(manifests []*corecomposition.PackageManifest) map[string][]fixtureDeclaration {
	declarations := make(map[string][]fixtureDeclaration)
	for _, manifest := range manifests {
		for _, fixture := range manifest.Fixtures {
			// The struct copy still shares the principals array with the
			// manifest, so without this a caller editing a resolved principal
			// would rewrite the package's own declaration.
			fixture.Principals = slices.Clone(fixture.Principals)
			declarations[fixture.Name] = append(declarations[fixture.Name],
				fixtureDeclaration{fixture: fixture, owner: manifest.ID})
		}
	}
	return declarations
}

// ambiguous reports whether declarations of one name would seed different
// state. A workspace composing one package under two module references declares
// that package's fixtures twice, identically — that still names one seed, so it
// is not a collision.
func ambiguous(declarations []fixtureDeclaration) bool {
	for _, declaration := range declarations[1:] {
		if !reflect.DeepEqual(declaration.fixture, declarations[0].fixture) {
			return true
		}
	}
	return false
}

func collisionError(name string, declarations []fixtureDeclaration) error {
	var owners []string
	for _, declaration := range declarations {
		if !slices.Contains(owners, declaration.owner) {
			owners = append(owners, declaration.owner)
		}
	}
	return fmt.Errorf("%w: fixture %q is declared differently by %s",
		corecomposition.ErrCollision, name, strings.Join(owners, " and "))
}

// WorkspaceFixtures returns the fixtures the workspace's composed packages
// declare, sorted by name, together with everything that kept that from being a
// complete answer: a package that could not be read, and a name two packages
// declare differently.
//
// A collision does not suppress the listing. This is the command a user runs to
// diagnose one, so it reports the collision and still names what each package
// declares — withholding the listing would withhold exactly the data needed to
// act on it.
func WorkspaceFixtures(ctx context.Context, workspace *resources.Workspace) ([]corecomposition.ProvidedFixture, []error) {
	manifests, problems := composedPackageManifests(ctx, workspace)
	declarations := declarationsByName(manifests)

	names := make([]string, 0, len(declarations))
	for name := range declarations {
		names = append(names, name)
	}
	sort.Strings(names)

	var fixtures []corecomposition.ProvidedFixture
	for _, name := range names {
		group := declarations[name]
		if ambiguous(group) {
			problems = append(problems, collisionError(name, group))
			for _, declaration := range group {
				fixtures = append(fixtures, declaration.fixture)
			}
			continue
		}
		fixtures = append(fixtures, group[0].fixture)
	}
	return fixtures, problems
}

// ValidateFixtureSelection resolves the fixture a run has selected against the
// composed packages' manifests, so a typo fails at load — naming the fixtures
// that do exist — instead of booting the whole stack and failing somewhere
// inside it.
//
// Callers pass the RESOLVED selection, not the raw --fixture flag: an
// environment declares the fixture its runtime uses, so a workspace that names
// one in workspace.codefly.yaml reaches here with the flag unset and a typo
// there is just as undeliverable.
//
// Only the selected name is judged. A name two packages declare differently is
// refused because that selection no longer names one seed; a collision on some
// other name is not this run's problem and does not block it.
//
// Two cases pass without verifying anything, and both say so rather than
// passing silently by omission: a package this workspace composes that could
// not be read may be the one declaring the selection, and a workspace whose
// readable packages declare no fixture at all predates the manifest schema.
func ValidateFixtureSelection(ctx context.Context, workspace *resources.Workspace, name string) error {
	if name == "" {
		return nil
	}
	manifests, problems := composedPackageManifests(ctx, workspace)
	declarations := declarationsByName(manifests)

	if group, declared := declarations[name]; declared {
		if ambiguous(group) {
			return collisionError(name, group)
		}
		return nil
	}

	// Nothing readable declares the selection. An unreadable package may be the
	// one that does, so this cannot be called a typo — but it must not pass in
	// silence either, because the run then boots having verified nothing.
	if len(problems) > 0 {
		cli.Warning("cannot verify fixture %q against every composed package: %v", name, errors.Join(problems...))
		return nil
	}
	if len(declarations) == 0 {
		return nil
	}

	available := make([]string, 0, len(declarations))
	for declaredName := range declarations {
		available = append(available, declaredName)
	}
	sort.Strings(available)
	return fmt.Errorf("%w: %q (available: %s)",
		corecomposition.ErrUnknownFixture, name, strings.Join(available, ", "))
}

// ResolveFixtureSelection resolves the fixture a run uses — an explicit
// override wins, otherwise the environment's declared fixture — and verifies
// the result before anything boots.
//
// Every path that selects a fixture goes through here: `run`, `test`, and the
// control plane's test entry. Resolving and verifying were previously paired by
// hand at each call site, so only the one that happened to be covered was ever
// exercised by a test.
func ResolveFixtureSelection(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, override string) (string, error) {
	selected := orchestration.SelectedFixture(env, override)
	if err := ValidateFixtureSelection(ctx, workspace, selected); err != nil {
		return "", err
	}
	return selected, nil
}
