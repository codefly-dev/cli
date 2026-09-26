package ci

import (
	"context"
	"slices"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// phaseResolvesConfigurations reports whether a phase's flow resolves workspace
// configurations, and so can be refused by an unresolvable ${endpoint:…}
// reference. Test starts services. Lint and compile look like they would not,
// but both drive a runtime flow whose Manager.Load creates a Runner and whose
// RuntimeValidationPolicy schedules RuntimeInit for every service, which
// resolves the workspace configurations exactly as a run does. Audit, SBOM,
// sync-drift and image builds read none.
func phaseResolvesConfigurations(phase string) bool {
	switch phase {
	case ciPhaseTest, ciPhaseLint, ciPhaseCompile:
		return true
	default:
		return false
	}
}

// validateConfigurationReferences refuses the gate before any phase runs when
// a service the plan tests, or one it depends on, declares a workspace
// configuration whose ${endpoint:…} reference cannot resolve.
func validateConfigurationReferences(ctx context.Context, workspace *resources.Workspace, plan *Plan, phases []string) error {
	if plan == nil || len(plan.Services) == 0 || !slices.ContainsFunc(phases, phaseResolvesConfigurations) {
		return nil
	}
	env, err := orchestration.SelectEnvironment(workspace, orchestration.LocalEnvironmentName)
	if err != nil {
		return err
	}
	roots := make([]*resources.Service, 0, len(plan.Services))
	for i := range plan.Services {
		_, service, err := loadPlannedService(ctx, workspace, &plan.Services[i])
		if err != nil {
			return err
		}
		roots = append(roots, service)
	}
	return orchestration.PlanConfigurationReferences(ctx, workspace, env, roots, false)
}
