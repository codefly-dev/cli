package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
)

const codeContractCatalogUnserved = "contract_catalog_unserved_method"

// checkContractCatalogs holds every composed module's published API catalog to
// its descriptors as a verified module update reads them
// (generate.ProtobufCatalogCoverage): a method the descriptor serves that no
// listener's inventory lists is otherwise refused only by a render whose
// binding calls it, or by the update of a verified package. Read from the
// committed catalog and descriptors, so it needs neither a toolchain nor the
// network. A warning here: the hard gate is `codefly generate contracts
// --check` in the module's own repository, and a module resolved from git is
// not held to it at render.
func checkContractCatalogs(ctx context.Context, ws *resources.Workspace, scope string, report *workspaceReadinessReport) {
	for _, ref := range ws.Modules {
		if scope != "" && ref.Name != scope {
			continue
		}
		resolution, err := ws.ResolveModule(ctx, ref)
		if err != nil || resolution.Dir == "" {
			continue
		}
		catalog, err := composition.LoadAPIContractCatalog(resolution.Dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			err = generate.ProtobufCatalogCoverage(catalog.Endpoints, func(endpoint composition.APIContractEndpoint) ([]byte, error) {
				return os.ReadFile(filepath.Join(resolution.Dir, filepath.FromSlash(endpoint.Path)))
			})
		}
		if err != nil {
			report.add(codeContractCatalogUnserved, "contract catalog of "+ref.Name, "warn",
				fmt.Sprintf("module %q publishes %s that does not hold to its descriptors: %v", ref.Name, composition.APIContractCatalogFileName, err),
				fmt.Sprintf("in module %q, list the method in the service's contract inventory and run `codefly generate contracts`", ref.Name))
		}
	}
}
