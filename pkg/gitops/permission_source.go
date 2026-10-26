package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	clicomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
)

// validatePermissionSource checks the source snapshot already recorded by
// generate contracts. It does not compile source or prove descriptor derivation;
// generate contracts --permissions --check remains the full regeneration gate.
func validatePermissionSource(ctx context.Context, workspace *resources.Workspace, module *resources.Module, selectedSource *resources.Service) error {
	manifest, err := composition.LoadPackageManifest(module.Dir())
	if errors.Is(err, os.ErrNotExist) {
		return nil // Modules without published contracts have no snapshot to compare.
	}
	if err != nil {
		return err
	}
	var problems []error
	for index := range manifest.Services {
		provided := &manifest.Services[index]
		for _, contract := range provided.APIContracts {
			if contract.Kind != "protobuf" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			service := selectedSource
			if service == nil || service.Name != provided.Name {
				source, err := clicomposition.ServiceUsesSource(ctx, workspace, module.Name, provided.Name)
				if err != nil {
					return err
				}
				if !source {
					continue
				}
				service, err = module.LoadServiceFromName(ctx, provided.Name)
				if err != nil {
					return err
				}
			}

			current := filepath.Join(service.Dir(), "proto")
			snapshot := filepath.Join(module.Dir(), filepath.Dir(contract.Path), "proto")
			if err := comparePermissionSource(current, snapshot); err != nil {
				problems = append(problems, fmt.Errorf("module %s service %s endpoint %s: source/export freshness: %w; run codefly generate contracts %s", module.Name, provided.Name, contract.Endpoint, err, module.Name))
			}
		}
	}
	return errors.Join(problems...)
}

func comparePermissionSource(current, snapshot string) error {
	for _, dir := range []string{current, snapshot} {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("required proto tree %s: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("required proto tree %s is not a directory", dir)
		}
	}
	diffs, err := generators.DiffTrees(current, snapshot)
	if err != nil {
		return err
	}
	if len(diffs) != 0 {
		return fmt.Errorf("proto source differs from exported snapshot: %s", strings.Join(diffs, ", "))
	}
	return nil
}
