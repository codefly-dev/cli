package generate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
)

// A packageable module's generated contracts carry its release version: the
// API catalog's `version` is the package manifest's (`generate contracts`), and
// every derived operation's identity, and so its digest, carries the same
// version (`generate runnables`, moduleReleaseVersion). A release that moves
// the package version has to move them with it, in the same commit, or the
// module ships contracts that still name the previous release and its own
// `generate contracts --check` / `generate runnables --check` go red.

// ReleasedContractPaths names, for a module directory, the generated paths a
// release re-keys to the package version: the API catalog when the module has
// exported one, and the derived runnables tree when the module derives one. It
// returns nothing for a module with no package manifest, which has no release
// version to carry — its catalog says 0.0.0 and its operations carry a
// contract-digest identity, neither of which a release moves.
func ReleasedContractPaths(moduleDir string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(moduleDir, composition.PackageManifestFileName)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	catalog := filepath.Join(moduleDir, filepath.FromSlash(composition.APIContractCatalogFileName))
	if _, err := os.Stat(catalog); err == nil {
		paths = append(paths, catalog)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	index, err := runnablespkg.LoadIndex(moduleDir)
	if err != nil {
		return nil, err
	}
	if index != nil {
		paths = append(paths, filepath.Join(moduleDir, filepath.FromSlash(runnablespkg.DerivedDir)))
	}
	return paths, nil
}

// RekeyReleasedContracts regenerates the paths ReleasedContractPaths names at
// the version the module's package manifest now carries, through the
// generators that wrote them.
//
// The catalog is re-emitted by writeContractCatalog, the writer `generate
// contracts` uses, over the endpoints the committed catalog records: the only
// field of it a version touches is `version`, and rebuilding the contract
// files themselves would need each service's proto toolchain, which a release
// does not. Before anything is written the committed catalog is held to the
// package manifest and to the contract files on disk
// (composition.ValidatePackageAPIContracts), so a stale catalog is refused
// rather than re-stamped. The derived runnables are regenerated whole by
// deriveRunnables and written by writeDerivedTree, exactly as `generate
// runnables` writes them.
func RekeyReleasedContracts(ctx context.Context, workspaceName, workspaceDir string, module *resources.Module) error {
	manifest, err := composition.LoadPackageManifest(module.Dir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("module %s: %w", module.Name, err)
	}
	catalog, err := composition.LoadAPIContractCatalog(module.Dir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("module %s: %w", module.Name, err)
	}
	if catalog.Package != manifest.ID {
		return fmt.Errorf("module %s: the API catalog names package %s, the package manifest %s; run `codefly generate contracts %s` and commit it before releasing", module.Name, catalog.Package, manifest.ID, module.Name)
	}
	// Held at the committed version first: the contract files and the
	// manifest's api-contracts must already agree with the catalog, or the
	// release would stamp its version on contracts nobody regenerated.
	if err = composition.ValidatePackageAPIContracts(module.Dir(), manifest, catalog); err != nil {
		return fmt.Errorf("module %s: the committed contracts are stale (%w); run `codefly generate contracts %s` and commit it before releasing", module.Name, err, module.Name)
	}
	output := filepath.Dir(filepath.Join(module.Dir(), filepath.FromSlash(composition.APIContractCatalogFileName)))
	if _, err = writeContractCatalog(ctx, module, catalog.Endpoints, generateOptions{canonicalOutput: output, writeDir: output}); err != nil {
		return fmt.Errorf("module %s: %w", module.Name, err)
	}

	index, err := runnablespkg.LoadIndex(module.Dir())
	if err != nil || index == nil {
		return err
	}
	derived, err := deriveRunnablesIn(ctx, workspaceName, workspaceDir, module)
	if err != nil {
		return fmt.Errorf("module %s: %w", module.Name, err)
	}
	if refusal := derived.refusal(); refusal != nil {
		return fmt.Errorf("module %s: %w", module.Name, refusal)
	}
	return writeDerivedTree(ctx, derived.files, filepath.Join(module.Dir(), filepath.FromSlash(runnablespkg.DerivedDir)))
}
