package ci

import (
	"context"
	"fmt"
	"slices"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// validateConfigurationReferences refuses the gate before any phase runs when
// a service the plan tests, or one it depends on, declares a workspace
// configuration whose ${endpoint:…} reference cannot resolve. Only a gate that
// runs the test phase starts services and so resolves configurations; lint,
// compile, audit, SBOM and image builds read none.
func validateConfigurationReferences(ctx context.Context, workspace *resources.Workspace, plan *Plan, phases []string) error {
	if plan == nil || len(plan.Services) == 0 || !slices.Contains(phases, ciPhaseTest) {
		return nil
	}
	env, err := orchestration.SelectEnvironment(workspace, orchestration.LocalEnvironmentName)
	if err != nil {
		return err
	}
	roots := make([]*resources.Service, 0, len(plan.Services))
	for _, planned := range plan.Services {
		ref, err := resources.ParseServiceWithOptionalModule(planned.Service)
		if err != nil {
			return fmt.Errorf("parse affected service %q: %w", planned.Service, err)
		}
		module, err := workspace.LoadModuleFromName(ctx, ref.Module)
		if err != nil {
			return fmt.Errorf("load module %q: %w", ref.Module, err)
		}
		service, err := module.LoadServiceFromName(ctx, ref.Name)
		if err != nil {
			return fmt.Errorf("load service %q: %w", planned.Service, err)
		}
		roots = append(roots, service)
	}
	return orchestration.PlanConfigurationReferences(ctx, workspace, env, roots, false)
}
