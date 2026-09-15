package composition

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/codefly-dev/cli/pkg/cli"
	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
)

// ComposedPackageManifests returns the package manifest of every module the
// workspace composes, in workspace declaration order.
//
// Most modules compose no package and ship no manifest, so an absent one is
// skipped rather than reported. A manifest that exists but does not load is
// reported: that is a defect in the module, and treating it as "composes
// nothing" would answer with a fixture set missing whatever it declares.
func ComposedPackageManifests(ctx context.Context, workspace *resources.Workspace) ([]*corecomposition.PackageManifest, error) {
	var manifests []*corecomposition.PackageManifest
	for _, ref := range workspace.Modules {
		dir, err := ResolveComposedModuleDir(ctx, workspace, ref)
		if err != nil {
			// Materializing composed pinned modules is best effort — it warns and
			// carries on when a pull fails — so a module with no local checkout
			// declares nothing this invocation rather than failing a command that
			// never had to load it.
			cli.Warning("cannot resolve composed module <%s>: %v; skipping the fixtures it declares", ref.Name, err)
			continue
		}
		manifest, err := corecomposition.LoadPackageManifest(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("module %q: %w", ref.Name, err)
		}
		manifests = append(manifests, manifest)
	}
	return manifests, nil
}

// WorkspaceFixtures returns the fixtures the workspace's composed packages
// declare, sorted by name. Two packages declaring the same name collide, and
// core reports that rather than letting a selection name two seeds.
func WorkspaceFixtures(ctx context.Context, workspace *resources.Workspace) ([]corecomposition.ProvidedFixture, error) {
	manifests, err := ComposedPackageManifests(ctx, workspace)
	if err != nil {
		return nil, err
	}
	return corecomposition.Fixtures(manifests...)
}

// ValidateFixtureSelection resolves a fixture selection against the composed
// packages' manifests, so a typo fails at load — naming the fixtures that do
// exist — instead of booting the whole stack and failing somewhere inside it.
//
// A workspace whose composed packages declare no fixture at all is left alone.
// The selection predates the manifest schema and still travels to the runtime
// as CODEFLY__FIXTURE, so resolving against an empty set would refuse every
// workspace naming a fixture its own services implement.
func ValidateFixtureSelection(ctx context.Context, workspace *resources.Workspace, name string) error {
	if name == "" {
		return nil
	}
	manifests, err := ComposedPackageManifests(ctx, workspace)
	if err != nil {
		return err
	}
	declared, err := corecomposition.Fixtures(manifests...)
	if err != nil {
		return err
	}
	if len(declared) == 0 {
		return nil
	}
	_, err = corecomposition.ResolveFixture(name, manifests...)
	return err
}
