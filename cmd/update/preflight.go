package update

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
)

// ValidatePolicy checks a freshness policy before any discovery or mutation.
func ValidatePolicy(policy string, deploying bool) error {
	switch policy {
	case "off", "check", "require-current":
		return nil
	case "auto":
		if deploying {
			return fmt.Errorf("agent updates: deploy cannot use auto; run or build with --agent-updates=auto, validate the result, then deploy with check or require-current")
		}
		return nil
	default:
		return fmt.Errorf("invalid agent update policy %q: use check, auto, require-current, or off", policy)
	}
}

var resolveLatestAgent = manager.PinToLatestRelease

type pendingAgent struct {
	service   *resources.Service
	candidate resources.Agent
	composed  bool
}

// Preflight checks the effective workspace selections before execution. Auto
// reuses the same admission and writers as explicit update commands.
func Preflight(ctx context.Context, workspace *resources.Workspace, policy string, out io.Writer) error {
	if err := ValidatePolicy(policy, false); err != nil {
		return err
	}
	if policy == "off" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pending, err := discoverAgentUpdates(ctx, workspace, out)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	if policy == "check" {
		fmt.Fprintln(out, "Use --agent-updates=auto on run/build, or codefly update, to update these selections.")
		return nil
	}
	if policy == "require-current" {
		return fmt.Errorf("agent selections are outdated; update and validate them before continuing")
	}
	// Admit all candidates before writing any selections.
	admitted := map[string]bool{}
	for _, item := range pending {
		key := item.candidate.Identifier()
		if admitted[key] {
			continue
		}
		if err := inspectAgent(ctx, &item.candidate); err != nil {
			return fmt.Errorf("cannot select %s: %w", key, admissionError(&item.candidate, err))
		}
		admitted[key] = true
	}
	overrides := map[string]string{}
	for _, item := range pending {
		if item.composed {
			overrides[item.candidate.Publisher+"/"+item.candidate.Name] = item.candidate.Version
		}
	}
	if len(overrides) > 0 {
		parsed, err := resources.ParseAgentOverrides(overrides)
		if err != nil {
			return err
		}
		if _, err := pinAgentOverrides(ctx, workspace, parsed); err != nil {
			return err
		}
	}
	for _, item := range pending {
		if item.composed {
			continue
		}
		if _, err := selectServiceAgent(ctx, item.service, &item.candidate); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "Agent selections updated. Review and commit the changed manifests.")
	return nil
}

func discoverAgentUpdates(ctx context.Context, workspace *resources.Workspace, out io.Writer) ([]pendingAgent, error) {
	mods, err := workspace.LoadModules(ctx)
	if err != nil {
		return nil, err
	}
	overridesInUse, err := workspace.AgentOverrides()
	if err != nil {
		return nil, err
	}
	candidates := map[string]resources.Agent{}
	var pending []pendingAgent
	for _, mod := range mods {
		services, err := mod.LoadServices(ctx)
		if err != nil {
			return nil, err
		}
		for _, svc := range services {
			if svc.Agent == nil {
				continue
			}
			if version, err := semver.NewVersion(svc.Agent.Version); err == nil && version.Prerelease() != "" {
				continue
			}
			key := svc.Agent.Identifier()
			candidate, ok := candidates[key]
			if !ok {
				candidate = *svc.Agent
				source, err := resolveLatestAgent(ctx, &candidate)
				if err != nil {
					return nil, fmt.Errorf("agent freshness unknown for %s: %w", key, err)
				}
				if source != "github" {
					return nil, fmt.Errorf("agent freshness unknown for %s: release discovery fell back to %s", key, source)
				}
				candidates[key] = candidate
			}
			newer, err := newerRelease(svc.Agent.Version, candidate.Version)
			if err != nil {
				return nil, err
			}
			if !newer {
				continue
			}
			fmt.Fprintf(out, "Agent update available: %s %s -> %s (%s/%s)\n", svc.Agent.Publisher+"/"+svc.Agent.Name, svc.Agent.Version, candidate.Version, mod.Name, svc.Name)
			composed := refuseComposedService(workspace, mod, svc) != nil
			if workspace.Layout != resources.LayoutKindFlat || mod.Name != workspace.Name {
				for _, override := range overridesInUse {
					if override.Matches(svc.Agent) {
						composed = true
					}
				}
			}
			pending = append(pending, pendingAgent{svc, candidate, composed})
		}
	}
	return pending, nil
}

func newerRelease(current, candidate string) (bool, error) {
	old, err := semver.NewVersion(current)
	if err != nil {
		return false, fmt.Errorf("cannot check agent version %q: %w", current, err)
	}
	next, err := semver.NewVersion(candidate)
	if err != nil {
		return false, fmt.Errorf("cannot check release version %q: %w", candidate, err)
	}
	// Never move an explicit development pin or downgrade a newer selection.
	return old.Prerelease() == "" && next.Prerelease() == "" && next.GreaterThan(old), nil
}
