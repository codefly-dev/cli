package composition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/codefly-dev/cli/pkg/cli"
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

// WorkspaceFixtures returns the fixtures the workspace's composed packages
// declare, sorted by name, together with everything that kept that from being a
// complete answer: a package that could not be read, and a name two packages
// both declare.
//
// A collision does not suppress the listing. This is the command a user runs to
// diagnose one, so it reports the collision and still names what each package
// declares — core's Fixtures returns nothing at all in that case, which would
// withhold exactly the data needed to act on it.
func WorkspaceFixtures(ctx context.Context, workspace *resources.Workspace) ([]corecomposition.ProvidedFixture, []error) {
	manifests, problems := composedPackageManifests(ctx, workspace)
	fixtures, err := corecomposition.Fixtures(manifests...)
	if err != nil {
		problems = append(problems, err)
		for _, manifest := range manifests {
			fixtures = append(fixtures, manifest.Fixtures...)
		}
		sort.Slice(fixtures, func(i, j int) bool { return fixtures[i].Name < fixtures[j].Name })
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
// Two cases deliberately pass. A workspace whose composed packages declare no
// fixture at all is left alone: the selection predates the manifest schema and
// still travels to the runtime as CODEFLY__FIXTURE. And a selection cannot be
// called a typo while a package this workspace composes could not be read —
// that package may be the one declaring it.
func ValidateFixtureSelection(ctx context.Context, workspace *resources.Workspace, name string) error {
	if name == "" {
		return nil
	}
	manifests, problems := composedPackageManifests(ctx, workspace)
	declared, err := corecomposition.Fixtures(manifests...)
	if err != nil {
		return err
	}
	if len(declared) == 0 {
		return nil
	}
	if _, err := corecomposition.ResolveFixture(name, manifests...); err != nil {
		if len(problems) > 0 {
			cli.Warning("cannot verify fixture %q against every composed package: %v", name, errors.Join(problems...))
			return nil
		}
		return err
	}
	return nil
}
