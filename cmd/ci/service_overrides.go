package ci

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// refuseServiceOverrides stops a CI command whose workspace has machine-local
// per-service overrides in effect, unless the caller says they are intended.
//
// A CI plan is a claim about what the committed workspace builds and tests. A
// service override is machine-local by construction and invisible in committed
// config, so a plan produced with one silently describes a different workspace
// than the one anyone else gets — the one failure mode worth blocking rather
// than reporting. Allowed explicitly, the overrides stand and their trees are
// part of the cache identity, because every digest is taken from the service
// directory that actually resolves.
func refuseServiceOverrides(ctx context.Context, workspace *resources.Workspace, allowed bool, command string) error {
	overrides, err := activeServiceOverrides(ctx, workspace)
	if err != nil || len(overrides) == 0 {
		return err
	}
	if allowed {
		for _, override := range overrides {
			fmt.Printf("service override in effect: %s\n", override)
		}
		return nil
	}
	return fmt.Errorf("%s refuses to run with per-service overrides in %s: %s; drop them with `codefly override service <module>/<service> --clear`, or pass --allow-service-overrides to plan against them deliberately",
		command, resources.LocalOverlayConfigurationName, strings.Join(overrides, ", "))
}

// activeServiceOverrides describes every per-service override the overlay
// declares, as "<module>/<service> (<kind> <where>)".
func activeServiceOverrides(ctx context.Context, workspace *resources.Workspace) ([]string, error) {
	overlay, err := resources.LoadLocalOverlay(ctx, workspace.Dir())
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", resources.LocalOverlayConfigurationName, err)
	}
	if overlay == nil {
		return nil, nil
	}
	var overrides []string
	for _, ref := range workspace.Modules {
		directive := overlay.Resolve[ref.Name]
		if directive == nil {
			continue
		}
		for service, serviceDirective := range directive.Services {
			overrides = append(overrides, fmt.Sprintf("%s/%s (%s)", ref.Name, service, describeServiceDirective(serviceDirective)))
		}
	}
	sort.Strings(overrides)
	return overrides, nil
}

func describeServiceDirective(directive *resources.ServiceResolveDirective) string {
	switch {
	case directive.Path != "":
		return "path " + directive.Path
	case directive.Worktree != "":
		return "worktree " + directive.Worktree
	default:
		return "version " + directive.Version
	}
}
