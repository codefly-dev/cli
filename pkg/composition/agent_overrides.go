package composition

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
)

// AgentOverrideUse is one committed `agent-overrides` entry together with the
// composed services it moves: how many, and the versions their modules pin.
type AgentOverrideUse struct {
	resources.AgentOverride
	// Services are the composed services, as <module>/<service>, whose agent
	// the override applies to.
	Services []string
	// ModulePins are the distinct versions those services' modules declare,
	// sorted — what would run without the override.
	ModulePins []string
}

// Line is the announcement a run or render prints for this override.
func (use AgentOverrideUse) Line() string {
	return fmt.Sprintf("agent %s overridden to %s by %s (%d services; module pins: %s)",
		use.Key(), use.Version, resources.WorkspaceConfigurationName, len(use.Services), strings.Join(use.ModulePins, ", "))
}

// ResolveAgentOverrides matches the workspace's committed agent-overrides
// against every composed service. Core applies an override at the one place a
// composed service is loaded (Module.LoadServiceFromReference); this reads the
// same modules back from their own directories to say what each override
// replaced.
//
// A key no composed service's agent answers to is an error naming the key: it
// is the typo surface of a top-level map (as with module-resolution), and an
// override that moves nothing would read as if the agent fix had shipped.
// A module that cannot be loaded is skipped rather than reported — the command
// about to load it fails on it properly — and while any module is skipped an
// unmatched key is not refused, because it may belong to that module.
func ResolveAgentOverrides(ctx context.Context, workspace *resources.Workspace) ([]AgentOverrideUse, error) {
	overrides, err := workspace.AgentOverrides()
	if err != nil || len(overrides) == 0 {
		return nil, err
	}
	uses := make([]AgentOverrideUse, len(overrides))
	pins := make([]map[string]bool, len(overrides))
	for i, override := range overrides {
		uses[i].AgentOverride = override
		pins[i] = map[string]bool{}
	}
	incomplete := false
	for _, ref := range workspace.Modules {
		if workspace.Layout == resources.LayoutKindFlat && ref.Name == workspace.Name {
			// The workspace's own services are authored here, not composed.
			continue
		}
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			incomplete = true
			continue
		}
		// Loaded straight from its directory, the module carries no override:
		// it is the copy that says what the module itself pins.
		declared, err := resources.LoadModuleFromDir(ctx, mod.Dir())
		if err != nil {
			incomplete = true
			continue
		}
		services, err := declared.LoadServices(ctx)
		if err != nil {
			incomplete = true
			continue
		}
		for _, service := range services {
			for i, override := range overrides {
				if override.Matches(service.Agent) {
					uses[i].Services = append(uses[i].Services, ref.Name+"/"+service.Name)
					pins[i][service.Agent.Version] = true
				}
			}
		}
	}
	var unused []string
	for i := range uses {
		if len(uses[i].Services) == 0 {
			unused = append(unused, uses[i].Key())
			continue
		}
		for pin := range pins[i] {
			uses[i].ModulePins = append(uses[i].ModulePins, pin)
		}
		sort.Strings(uses[i].ModulePins)
		sort.Strings(uses[i].Services)
	}
	if len(unused) > 0 && !incomplete {
		return nil, fmt.Errorf("%s in %s names %s, but no composed service runs on that agent; the override would move nothing — check the <publisher>/<name> spelling against the services' agent:",
			resources.AgentOverridesKey, resources.WorkspaceConfigurationName, strings.Join(unused, ", "))
	}
	var used []AgentOverrideUse
	for _, use := range uses {
		if len(use.Services) > 0 {
			used = append(used, use)
		}
	}
	return used, nil
}

// ReportAgentOverrides prints, once per overridden agent, that the workspace
// moved it — an agent running at a version its modules do not pin is said out
// loud when the run or render starts — and refuses an override that does not
// hold up.
func ReportAgentOverrides(ctx context.Context, workspace *resources.Workspace) error {
	uses, err := ResolveAgentOverrides(ctx, workspace)
	if err != nil {
		return err
	}
	for _, use := range uses {
		cli.Info("%s", use.Line())
	}
	return nil
}
